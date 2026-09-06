package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/copilot"
	"ci-signal/internal/gitlab"
	"ci-signal/internal/markdown"
	"ci-signal/internal/repository"
	"ci-signal/internal/review"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestUnchangedAndCheckboxOnlyRerunsAvoidAI(t *testing.T) {
	settings := testConfig(t)
	capture := testCapture()
	fingerprint, err := Fingerprint(capture, settings)
	if err != nil {
		t.Fatal(err)
	}
	state := priorState(capture, fingerprint)
	reviewCalls, repairCalls, publishCalls := 0, 0, 0
	hooks := testHooks(capture)
	hooks.Review = func(context.Context, BatchRequest) (copilot.Result, error) {
		reviewCalls++
		return copilot.Result{}, errors.New("unexpected AI")
	}
	hooks.RepairLabels = func(context.Context, review.State, gitlab.LabelPlan, Capture) error { repairCalls++; return nil }
	hooks.Recover = func(context.Context) (gitlab.Recovery, error) {
		return gitlab.Recovery{Current: &gitlab.RecoveredReport{NoteID: 4, State: state}}, nil
	}
	hooks.Publish = func(context.Context, PublicationRequest) (review.State, error) {
		publishCalls++
		return review.State{}, nil
	}
	coordinator := mustCoordinator(t, settings, hooks)
	result, err := coordinator.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.UsedAI || reviewCalls != 0 || repairCalls != 1 || publishCalls != 0 {
		t.Fatalf("unchanged result=%#v calls=%d/%d/%d", result, reviewCalls, repairCalls, publishCalls)
	}

	state.Findings = []review.Finding{{ID: "finding-1", Category: review.CategoryRisk, Subcategory: review.SubcategoryCorrections, Relationship: review.RelationshipIntroduced, Title: "Regression", Explanation: "Fails", Assessment: review.AssessmentPresent, State: review.FindingOpen, FirstSeenAt: time.Unix(1, 0), LastSeenAt: time.Unix(1, 0)}}
	state.Runs[0].Verdict = review.VerdictNeedsReview
	source, err := markdown.NewCodec().Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, "- [ ]", "- [x]", 1)
	hooks.Recover = func(context.Context) (gitlab.Recovery, error) {
		return gitlab.Recovery{Current: &gitlab.RecoveredReport{NoteID: 4, State: state, Source: source}}, nil
	}
	hooks.Publish = func(_ context.Context, request PublicationRequest) (review.State, error) {
		publishCalls++
		return request.State, nil
	}
	coordinator = mustCoordinator(t, settings, hooks)
	result, err = coordinator.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.UsedAI || reviewCalls != 0 || publishCalls != 1 || result.Verdict != review.VerdictNeedsReview || result.Completion != review.SubmissionComplete || result.State.Findings[0].State != review.FindingAcknowledged || len(result.State.Runs) != 1 {
		t.Fatalf("checkbox result=%#v", result)
	}
	if len(result.State.Runs[0].Telemetry.Usage) != 1 {
		t.Fatal("checkbox-only rerun lost prior telemetry")
	}

	checkedState := result.State
	source, err = markdown.NewCodec().Encode(checkedState)
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, "- [x]", "- [ ]", 1)
	hooks.Recover = func(context.Context) (gitlab.Recovery, error) {
		return gitlab.Recovery{Current: &gitlab.RecoveredReport{NoteID: 5, State: checkedState, Source: source}}, nil
	}
	coordinator = mustCoordinator(t, settings, hooks)
	result, err = coordinator.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.UsedAI || reviewCalls != 0 || publishCalls != 2 || result.State.Findings[0].State != review.FindingOpen {
		t.Fatalf("uncheck result=%#v", result)
	}
	if len(result.State.Runs[0].Telemetry.Usage) != 1 {
		t.Fatal("checkbox uncheck lost prior telemetry")
	}
}

func TestUnchangedFailedReviewRetriesAIAndPreservesPriorState(t *testing.T) {
	settings := testConfig(t)
	capture := testCapture()
	fingerprint, err := Fingerprint(capture, settings)
	if err != nil {
		t.Fatal(err)
	}
	state := priorState(capture, fingerprint)
	state.Runs[0].Completion = review.SubmissionPartial
	state.Runs[0].Verdict = review.VerdictNeedsReview
	state.Runs[0].Coverage[0] = review.UnitCoverage{UnitID: "unit-1", Outcome: review.CoverageFailed, Explanation: "review session failed"}
	state.Findings = []review.Finding{{
		ID: "finding-1", IdentityKey: "stable", Category: review.CategoryRisk, Subcategory: review.SubcategoryReliability,
		Relationship: review.RelationshipIntroduced, Title: "Prior risk", Explanation: "Still unassessed",
		AssignedUnits: []review.ReviewUnitID{"unit-1"}, Assessment: review.AssessmentPresent, State: review.FindingOpen,
		History: []review.FindingEvent{{Kind: review.FindingEventCreated, RunID: "old", At: time.Unix(1, 0)}}, FirstSeenAt: time.Unix(1, 0), LastSeenAt: time.Unix(1, 0),
	}}
	reviewCalls := 0
	hooks := testHooks(capture)
	hooks.Recover = func(context.Context) (gitlab.Recovery, error) {
		return gitlab.Recovery{Current: &gitlab.RecoveredReport{NoteID: 4, State: state}}, nil
	}
	hooks.Review = func(_ context.Context, request BatchRequest) (copilot.Result, error) {
		reviewCalls++
		return acceptedResult("retry-partial", review.Submission{
			Verdict: review.VerdictNeedsReview, Completion: review.SubmissionPartial,
			Coverage:    []review.UnitCoverage{{UnitID: request.Units[0].ID, Outcome: review.CoveragePartial, Explanation: "retry incomplete"}},
			Limitations: []string{"retry incomplete"},
		}), nil
	}

	result, err := mustCoordinator(t, settings, hooks).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 1 || !result.UsedAI || result.Completion != review.SubmissionPartial || len(result.State.Runs) != 2 {
		t.Fatalf("retry result=%#v calls=%d", result, reviewCalls)
	}
	if result.State.Runs[0].Coverage[0].Outcome != review.CoverageFailed || result.State.Runs[0].Coverage[0].Explanation != "review session failed" {
		t.Fatalf("prior failed run changed: %#v", result.State.Runs[0])
	}
	finding := result.State.Findings[0]
	if finding.ID != "finding-1" || finding.Assessment != review.AssessmentPresent || finding.State != review.FindingOpen || len(finding.History) != 1 {
		t.Fatalf("unassessed prior finding changed: %#v", finding)
	}
}

func TestFailedReviewPreservesObservedTelemetry(t *testing.T) {
	settings := testConfig(t)
	capture := testCapture()
	hooks := testHooks(capture)
	hooks.Review = func(context.Context, BatchRequest) (copilot.Result, error) {
		return copilot.Result{Telemetry: review.Telemetry{
			RequestedModels: []string{config.OllamaCloudModel},
			ObservedModels:  []review.ModelObservation{{SessionID: "session-1", Model: "observed-model"}},
			UsageComplete:   false,
		}}, errors.New("model integrity failure")
	}

	result, err := mustCoordinator(t, settings, hooks).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	telemetry := result.State.Runs[len(result.State.Runs)-1].Telemetry
	if len(telemetry.RequestedModels) != 1 || len(telemetry.ObservedModels) != 1 || telemetry.ObservedModels[0].Model != "observed-model" || telemetry.UsageComplete {
		t.Fatalf("failed review telemetry = %#v", telemetry)
	}
}

func TestContextChangeBatchesAllUnitsAndCrossFileFinding(t *testing.T) {
	settings := testConfig(t)
	settings.Limits.MaxUnitsPerSession = 2
	capture := testCapture()
	capture.Inventory.Units = []repository.Unit{
		{ID: "a", Kind: repository.UnitDeclaration, Symbol: "A", Head: &repository.Span{Path: "a.go"}},
		{ID: "b", Kind: repository.UnitDeclaration, Symbol: "B", Head: &repository.Span{Path: "b.go"}},
		{ID: "c", Kind: repository.UnitFile, Head: &repository.Span{Path: "c.go"}},
	}
	baseFingerprint, _ := Fingerprint(capture, settings)
	prior := priorState(capture, baseFingerprint)
	prior.Findings = []review.Finding{{ID: "prior-c", IdentityKey: "prior-c", Category: review.CategoryRisk, Subcategory: review.SubcategoryReliability, Relationship: review.RelationshipIntroduced, Title: "Existing C risk", Explanation: "Still requires assessment", AssignedUnits: []review.ReviewUnitID{"c"}, Assessment: review.AssessmentPresent, State: review.FindingOpen, FirstSeenAt: time.Unix(1, 0), LastSeenAt: time.Unix(1, 0)}}
	capture.Context.Notes = []*gitlabapi.Note{{ID: 22, Body: "new public context", Author: gitlabapi.NoteAuthor{Username: "dev"}}}
	reviewCalls := 0
	priorAssignments := 0
	seen := map[review.ReviewUnitID]bool{}
	hooks := testHooks(capture)
	hooks.Recover = func(context.Context) (gitlab.Recovery, error) {
		return gitlab.Recovery{Current: &gitlab.RecoveredReport{NoteID: 4, State: prior}}, nil
	}
	hooks.Review = func(_ context.Context, request BatchRequest) (copilot.Result, error) {
		reviewCalls++
		coverage := make([]review.UnitCoverage, 0, len(request.Units))
		for _, unit := range request.Units {
			seen[unit.ID] = true
			coverage = append(coverage, review.UnitCoverage{UnitID: unit.ID, Outcome: review.CoverageComplete})
		}
		submission := review.Submission{Verdict: review.VerdictApproved, Completion: review.SubmissionComplete, Coverage: coverage}
		if _, ok := request.Assignment.Findings["prior-c"]; ok {
			priorAssignments++
			submission.Reassessments = []review.Reassessment{{FindingID: "prior-c", Assessment: review.AssessmentPresent, Explanation: "The risk remains.", AssignedUnits: []review.ReviewUnitID{"c"}}}
		}
		if reviewCalls == 1 {
			submission.Findings = []review.SubmittedFinding{{Category: review.CategoryRisk, Subcategory: review.SubcategoryCorrections, Relationship: review.RelationshipIntroduced, Title: "Cross-file regression", Explanation: "A breaks B", AssignedUnits: []review.ReviewUnitID{"a", "b"}}}
		}
		return acceptedResult("digest-"+string(rune('0'+reviewCalls)), submission), nil
	}
	coordinator := mustCoordinator(t, settings, hooks)
	result, err := coordinator.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 2 || !seen["a"] || !seen["b"] || !seen["c"] || !seen[integrationUnitID(result.Fingerprint)] {
		t.Fatalf("batches=%d seen=%v", reviewCalls, seen)
	}
	if result.Verdict != review.VerdictNeedsReview || len(result.State.Findings) != 2 || priorAssignments != 1 {
		t.Fatalf("result=%#v", result)
	}
}

func TestPartialAndStaleRunsCannotApproveOrOverwrite(t *testing.T) {
	settings := testConfig(t)
	capture := testCapture()
	publishCalls, staleCalls := 0, 0
	hooks := testHooks(capture)
	hooks.Review = func(_ context.Context, request BatchRequest) (copilot.Result, error) {
		coverage := []review.UnitCoverage{}
		for _, u := range request.Units {
			coverage = append(coverage, review.UnitCoverage{UnitID: u.ID, Outcome: review.CoveragePartial})
		}
		return acceptedResult("partial", review.Submission{Verdict: review.VerdictApproved, Completion: review.SubmissionPartial, Coverage: coverage, Limitations: []string{"budget"}}), nil
	}
	hooks.Publish = func(_ context.Context, request PublicationRequest) (review.State, error) {
		publishCalls++
		return request.State, nil
	}
	coordinator := mustCoordinator(t, settings, hooks)
	result, err := coordinator.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != review.VerdictNeedsReview || result.Completion != review.SubmissionPartial || publishCalls != 1 {
		t.Fatalf("partial result=%#v calls=%d", result, publishCalls)
	}

	settings.Copilot.StateDir = filepath.Join(t.TempDir(), "stale")
	hooks = testHooks(capture)
	hooks.Recheck = func(context.Context, Capture) error { return errors.New("head changed") }
	hooks.HandleStale = func(context.Context, PublicationRequest) error {
		staleCalls++
		return errors.New("head changed")
	}
	coordinator = mustCoordinator(t, settings, hooks)
	_, err = coordinator.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stale") || publishCalls != 1 || staleCalls != 1 {
		t.Fatalf("stale err=%v publish=%d stale=%d", err, publishCalls, staleCalls)
	}
}

func TestAcceptedCheckpointReusedOnlyForIdenticalFingerprint(t *testing.T) {
	settings := testConfig(t)
	capture := testCapture()
	reviewCalls := 0
	failPublish := true
	hooks := testHooks(capture)
	hooks.Review = func(_ context.Context, request BatchRequest) (copilot.Result, error) {
		reviewCalls++
		return completeResult(request.Units), nil
	}
	hooks.Publish = func(_ context.Context, request PublicationRequest) (review.State, error) {
		if failPublish {
			return review.State{}, errors.New("uncertain create")
		}
		return request.State, nil
	}
	coordinator := mustCoordinator(t, settings, hooks)
	if _, err := coordinator.Run(context.Background()); err == nil {
		t.Fatal("first publish succeeded")
	}
	failPublish = false
	coordinator = mustCoordinator(t, settings, hooks)
	if _, err := coordinator.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 1 {
		t.Fatalf("identical retry AI calls=%d", reviewCalls)
	}
	capture.Context.MergeRequest.Description = "changed"
	hooks.Capture = func(context.Context, time.Time) (Capture, error) { return capture, nil }
	settings.Copilot.StateDir = filepath.Join(t.TempDir(), "changed")
	coordinator = mustCoordinator(t, settings, hooks)
	if _, err := coordinator.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 2 {
		t.Fatalf("changed fingerprint AI calls=%d", reviewCalls)
	}
}

func TestPartialCheckpointIsNotReused(t *testing.T) {
	settings := testConfig(t)
	capture := testCapture()
	reviewCalls := 0
	failPublish := true
	hooks := testHooks(capture)
	hooks.Review = func(_ context.Context, request BatchRequest) (copilot.Result, error) {
		reviewCalls++
		if reviewCalls == 1 {
			return acceptedResult("partial", review.Submission{
				Verdict: review.VerdictNeedsReview, Completion: review.SubmissionPartial,
				Coverage:    []review.UnitCoverage{{UnitID: request.Units[0].ID, Outcome: review.CoveragePartial, Explanation: "incomplete"}},
				Limitations: []string{"incomplete"},
			}), nil
		}
		return completeResult(request.Units), nil
	}
	hooks.Publish = func(_ context.Context, request PublicationRequest) (review.State, error) {
		if failPublish {
			return review.State{}, errors.New("uncertain create")
		}
		return request.State, nil
	}
	if _, err := mustCoordinator(t, settings, hooks).Run(context.Background()); err == nil {
		t.Fatal("first publish succeeded")
	}
	failPublish = false
	result, err := mustCoordinator(t, settings, hooks).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 2 || !result.UsedAI || result.Completion != review.SubmissionComplete {
		t.Fatalf("partial checkpoint retry result=%#v calls=%d", result, reviewCalls)
	}
}

func TestSessionBudgetRecordsEveryOmittedUnitFailed(t *testing.T) {
	settings := testConfig(t)
	settings.Limits.MaxUnitsPerSession = 2
	settings.Limits.MaxSessions = 1
	capture := testCapture()
	capture.Inventory.Units = []repository.Unit{
		{ID: "a", Kind: repository.UnitFile, Head: &repository.Span{Path: "a.go"}},
		{ID: "b", Kind: repository.UnitFile, Head: &repository.Span{Path: "b.go"}},
		{ID: "c", Kind: repository.UnitFile, Head: &repository.Span{Path: "c.go"}},
		{ID: "d", Kind: repository.UnitFile, Head: &repository.Span{Path: "d.go"}},
		{ID: "e", Kind: repository.UnitFile, Head: &repository.Span{Path: "e.go"}},
	}
	result, err := mustCoordinator(t, settings, testHooks(capture)).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, coverage := range result.State.Runs[len(result.State.Runs)-1].Coverage {
		if coverage.Outcome == review.CoverageFailed {
			failed++
		}
	}
	if result.Verdict != review.VerdictNeedsReview || result.Completion != review.SubmissionPartial || failed != 4 {
		t.Fatalf("budget result=%#v failed=%d", result, failed)
	}
}

func TestPublicationSourceKindsClassifiesOnlyHostNamespaces(t *testing.T) {
	state := review.State{
		Sources:  []review.SourceRecord{{ID: "note-7-version", Kind: review.SourceGitLabNote, Body: "public"}},
		Findings: []review.Finding{{Evidence: []review.Evidence{{SourceRefs: []review.SourceReferenceID{"src_012345", "unknown-1"}}}}},
	}
	kinds := publicationSourceKinds(state, Capture{})
	if kinds["note-7-version"] != review.SourceGitLabNote || kinds["src_012345"] != review.SourceRepositorySource {
		t.Fatalf("source kinds = %#v", kinds)
	}
	if _, ok := kinds["unknown-1"]; ok {
		t.Fatal("unknown source was classified instead of preserving the predecessor")
	}
}

func TestConfiguredConcurrencyBoundsParallelBatches(t *testing.T) {
	settings := testConfig(t)
	settings.Limits.MaxUnitsPerSession = 2
	settings.Limits.MaxConcurrency = 2
	capture := testCapture()
	capture.Inventory.Units = []repository.Unit{
		{ID: "a", Kind: repository.UnitFile, Head: &repository.Span{Path: "a.go"}},
		{ID: "b", Kind: repository.UnitFile, Head: &repository.Span{Path: "b.go"}},
		{ID: "c", Kind: repository.UnitFile, Head: &repository.Span{Path: "c.go"}},
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	hooks := testHooks(capture)
	hooks.Review = func(_ context.Context, request BatchRequest) (copilot.Result, error) {
		entered <- struct{}{}
		<-release
		return completeResult(request.Units), nil
	}
	coordinator := mustCoordinator(t, settings, hooks)
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.Run(context.Background())
		done <- err
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("configured concurrent batches did not start")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintIncludesReviewBudgetsButExcludesDiagnosticsAndLabels(t *testing.T) {
	capture := testCapture()
	settings := testConfig(t)
	baseline, err := Fingerprint(capture, settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.Limits.MaxSessions++
	changed, err := Fingerprint(capture, settings)
	if err != nil {
		t.Fatal(err)
	}
	if changed == baseline {
		t.Fatal("session budget did not change review fingerprint")
	}
	settings.Limits.MaxSessions--
	settings.Diagnostics.LogLevel = config.LogDebug
	settings.Labels.Static = []string{"ai-review::configured"}
	incidental, err := Fingerprint(capture, settings)
	if err != nil {
		t.Fatal(err)
	}
	if incidental != baseline {
		t.Fatal("diagnostics or managed labels changed the review trigger")
	}
}

func TestFingerprintIncludesAuthorClassification(t *testing.T) {
	capture := testCapture()
	capture.Context.Notes = []*gitlabapi.Note{{ID: 22, Body: "discussion", Author: gitlabapi.NoteAuthor{ID: 12, Username: "actor"}}}
	capture.Context.AuthorKinds = map[int64]gitlab.AuthorKind{12: gitlab.AuthorUnknown}
	unknown, err := Fingerprint(capture, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	capture.Context.AuthorKinds[12] = gitlab.AuthorHuman
	human, err := Fingerprint(capture, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if human == unknown {
		t.Fatal("author classification did not change review fingerprint")
	}
}

func TestDiscussionThreadAssociatesHumanReplyWithQuotedFinding(t *testing.T) {
	root := &gitlabapi.Note{ID: 71, Body: "Regarding Existing C risk", Author: gitlabapi.NoteAuthor{ID: 11, Username: "alice"}}
	reply := &gitlabapi.Note{ID: 72, Body: "We accept this limitation for now.", Author: gitlabapi.NoteAuthor{ID: 12, Username: "bob"}}
	context := gitlab.Context{Notes: []*gitlabapi.Note{root, reply}, Discussions: []gitlab.Discussion{{ID: "thread-1", NoteIDs: []int64{71, 72}}}, AuthorKinds: map[int64]gitlab.AuthorKind{11: gitlab.AuthorHuman, 12: gitlab.AuthorHuman}}
	findings := []review.Finding{{ID: "prior-c", Title: "Existing C risk"}}
	sources := associateSources(context, findings, 900)
	replySource := sources[noteSourceID(reply.ID, reply.Body, reply.UpdatedAt)]
	if replySource.Kind != review.SourceGitLabDiscussion || replySource.FindingID != "prior-c" || !replySource.Human {
		t.Fatalf("discussion reply source = %#v", replySource)
	}
}

func TestOnlyConfirmedHumanDiscussionCanReachAcknowledgementValidation(t *testing.T) {
	validator, err := review.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		kind    gitlab.AuthorKind
		wantErr bool
	}{
		{name: "human", kind: gitlab.AuthorHuman},
		{name: "bot", kind: gitlab.AuthorBot, wantErr: true},
		{name: "unknown", kind: gitlab.AuthorUnknown, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			note := &gitlabapi.Note{ID: 72, Body: "Regarding Existing C risk, we accept this limitation.", Author: gitlabapi.NoteAuthor{ID: 12, Username: "actor"}}
			context := gitlab.Context{
				Notes:       []*gitlabapi.Note{note},
				Discussions: []gitlab.Discussion{{ID: "thread-1", NoteIDs: []int64{72}}},
				AuthorKinds: map[int64]gitlab.AuthorKind{12: test.kind},
			}
			findings := []review.Finding{{ID: "prior-c", Title: "Existing C risk"}}
			sources := associateSources(context, findings, 900)
			sourceID := noteSourceID(note.ID, note.Body, note.UpdatedAt)
			submission := review.Submission{
				Verdict:    review.VerdictNeedsReview,
				Completion: review.SubmissionComplete,
				Findings:   []review.SubmittedFinding{},
				Reassessments: []review.Reassessment{{
					FindingID: "prior-c", Assessment: review.AssessmentPresent, Explanation: "The accepted limitation remains present.",
					Evidence: []review.Evidence{{Explanation: "The response accepts the limitation.", SourceRefs: []review.SourceReferenceID{sourceID}}}, AssignedUnits: []review.ReviewUnitID{"unit-1"},
				}},
				AcknowledgementChanges: []review.AcknowledgementTransition{{FindingID: "prior-c", Action: review.AcknowledgementActionAcknowledge, Method: review.AcknowledgementAIDiscussion, Explanation: "The author accepts the limitation.", SourceRefs: []review.SourceReferenceID{sourceID}}},
				Coverage:               []review.UnitCoverage{{UnitID: "unit-1", Outcome: review.CoverageComplete}},
				Limitations:            []string{},
			}
			raw, err := json.Marshal(submission)
			if err != nil {
				t.Fatal(err)
			}
			assignment := review.Assignment{
				UnitIDs:    map[review.ReviewUnitID]struct{}{"unit-1": {}},
				Findings:   map[review.FindingID]review.KnownFinding{"prior-c": {ID: "prior-c", State: review.FindingOpen}},
				SourceRefs: sources,
			}
			_, err = validator.Validate(raw, assignment)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "human discussion") {
					t.Fatalf("validation error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func testHooks(capture Capture) Hooks {
	return Hooks{
		Capture: func(context.Context, time.Time) (Capture, error) { return capture, nil },
		Recover: func(context.Context) (gitlab.Recovery, error) { return gitlab.Recovery{}, nil },
		Review: func(_ context.Context, request BatchRequest) (copilot.Result, error) {
			return completeResult(request.Units), nil
		},
		Recheck:      func(context.Context, Capture) error { return nil },
		Publish:      func(_ context.Context, request PublicationRequest) (review.State, error) { return request.State, nil },
		HandleStale:  func(context.Context, PublicationRequest) error { return nil },
		RepairLabels: func(context.Context, review.State, gitlab.LabelPlan, Capture) error { return nil },
	}
}

func testCapture() Capture {
	at := time.Unix(100, 0).UTC()
	mr := &gitlabapi.MergeRequest{BasicMergeRequest: gitlabapi.BasicMergeRequest{IID: 7, Title: "MR", Description: "desc", TargetBranch: "main", SourceBranch: "feature"}, DiffRefs: gitlabapi.MergeRequestDiffRefs{BaseSha: strings.Repeat("a", 40), HeadSha: strings.Repeat("b", 40)}}
	snapshot := repository.Snapshot{ID: "snapshot-1", BaseCommit: mr.DiffRefs.BaseSha, HeadCommit: mr.DiffRefs.HeadSha, CapturedAt: at}
	return Capture{Context: gitlab.Context{MergeRequest: mr, Complete: true}, Snapshot: snapshot, Inventory: repository.Inventory{SnapshotID: snapshot.ID, Scope: review.ScopeMRImpact, Units: []repository.Unit{{ID: "unit-1", Kind: repository.UnitFile, Head: &repository.Span{Path: "main.go"}}}}, Guidance: repository.GuidanceSnapshot{RuntimeDirectory: "/tmp/runtime"}, RelevantFiles: map[string][]byte{"skill": []byte("review")}}
}

func testConfig(t *testing.T) config.Config {
	root := t.TempDir()
	return config.Config{Version: 1, GitLab: config.GitLab{BaseURL: "https://gitlab.example.com", Project: "group/project", MRIID: 7, JobTokenEnv: "CI_JOB_TOKEN", APITokenEnv: "GITLAB_API_TOKEN"}, Repository: config.Repository{ProjectDir: "/project"}, Copilot: config.Copilot{Provider: config.Provider{Name: config.ProviderOllamaCloud, Type: config.ProviderTypeOpenAI, Endpoint: config.OllamaCloudEndpoint, Model: config.OllamaCloudModel, CredentialEnv: "OLLAMA_API_KEY", WireAPI: config.WireAPIResponses}, HomeDir: filepath.Join(root, "home"), StateDir: filepath.Join(root, "state")}, Guidance: config.Guidance{CoreSkillDirs: []string{"/skills"}, CoreInstructionDirs: []string{"/instructions"}, ReviewPromptFile: "/review.md", RequiredSkills: []string{"review"}, ReservedSkillNames: []string{"review"}}, Limits: config.Limits{OverallTimeout: config.Duration(time.Minute), SessionTimeout: config.Duration(time.Minute), ToolTimeout: config.Duration(time.Second), APITimeout: config.Duration(time.Second), MaxConcurrency: 1, MaxSessions: 8, MaxCorrectionAttempts: 2, MaxUnitsPerSession: 20, MaxSourceBytes: 1024, MaxDiffBytes: 1024, MaxToolOutputBytes: 1024, MaxReportBytes: 100000, MaxInputTokens: 1000, MaxOutputTokens: 1000}, Review: config.Review{Scope: review.ScopeMRImpact, GatingCategories: []review.Category{review.CategoryRisk}, ExitPolicy: config.ExitOnIncomplete}, Permissions: config.Permissions{Mode: config.PermissionReadOnly, NativeTools: []string{"submit_review"}}, Tools: config.Tools{GitPath: "git", ASTGrepPath: "ast-grep"}, Diagnostics: config.Diagnostics{LogLevel: config.LogInfo}}
}

func priorState(capture Capture, fingerprint review.Fingerprint) review.State {
	telemetry := review.Telemetry{RequestedModels: []string{config.OllamaCloudModel}, UsageComplete: true, Usage: []review.UsageEvent{{EventID: "one", SessionID: "old", TotalTokens: 2}}}
	return review.State{SchemaVersion: 1, MR: review.MRIdentity{BaseURL: "https://gitlab.example.com", Project: "group/project", MRIID: 7}, Snapshot: domainSnapshot(capture.Snapshot), Fingerprint: fingerprint, Runs: []review.Run{{ID: "old", Fingerprint: fingerprint, StartedAt: time.Unix(1, 0), CompletedAt: time.Unix(2, 0), Completion: review.SubmissionComplete, Verdict: review.VerdictApproved, Coverage: []review.UnitCoverage{{UnitID: "unit-1", Outcome: review.CoverageComplete}}, Telemetry: telemetry}}}
}

func completeResult(units []repository.Unit) copilot.Result {
	coverage := make([]review.UnitCoverage, 0, len(units))
	for _, unit := range units {
		coverage = append(coverage, review.UnitCoverage{UnitID: unit.ID, Outcome: review.CoverageComplete})
	}
	return acceptedResult("complete", review.Submission{Verdict: review.VerdictApproved, Completion: review.SubmissionComplete, Coverage: coverage})
}
func acceptedResult(digest string, submission review.Submission) copilot.Result {
	return copilot.Result{Accepted: &review.AcceptedSubmission{Receipt: review.SubmissionReceipt{Digest: digest}, Value: submission}, Telemetry: review.Telemetry{RequestedModels: []string{config.OllamaCloudModel}, UsageComplete: true}}
}
func mustCoordinator(t *testing.T, settings config.Config, hooks Hooks) *Coordinator {
	t.Helper()
	value, err := New(settings, hooks)
	if err != nil {
		t.Fatal(err)
	}
	value.now = func() time.Time { return time.Unix(200, 0).UTC() }
	return value
}
