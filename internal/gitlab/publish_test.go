package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ci-signal/internal/markdown"
	"ci-signal/internal/review"
)

func TestPublishRecoversUncertainCreateAndReplacesPredecessor(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	prior := fixture.addReport(t, codec, fixture.reviewState("old"), "generation-old", 0)
	fixture.uncertainCreate = true

	request := fixture.publishRequest(fixture.reviewState("new"), "generation-new", prior)
	result, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.createCalls != 1 || fixture.updateCalls == 0 || result.NoteID == prior.NoteID {
		t.Fatalf("result=%#v creates=%d updates=%d", result, fixture.createCalls, fixture.updateCalls)
	}
	if fixture.hasNote(prior.NoteID) || !fixture.hasNote(result.NoteID) {
		t.Fatalf("notes after replacement = %v", fixture.noteIDs())
	}
	recovered, err := publisher.Recover(context.Background())
	if err != nil || recovered.Current == nil || recovered.Current.NoteID != result.NoteID || len(recovered.Superseded) != 0 {
		t.Fatalf("recovery=%#v err=%v", recovered, err)
	}
}

func TestPublishVerificationFailurePreservesPredecessor(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	prior := fixture.addReport(t, codec, fixture.reviewState("old"), "generation-old", 0)
	fixture.corruptSuccessorRead = true

	_, err := publisher.Publish(context.Background(), fixture.publishRequest(fixture.reviewState("new"), "generation-new", prior))
	if err == nil || !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("error = %v", err)
	}
	if !fixture.hasNote(prior.NoteID) || fixture.deleteCalls != 0 {
		t.Fatalf("predecessor was not preserved: notes=%v deletes=%d", fixture.noteIDs(), fixture.deleteCalls)
	}
}

func TestPublishReconcilesLateHumanCheckAndUncheckAcrossReplacement(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	prior := fixture.addReport(t, codec, fixture.reviewState("one"), "generation-one", 0)
	fixture.lateControl = func(body string) string { return strings.Replace(body, "- [ ] ", "- [x] ", 1) }

	first, err := publisher.Publish(context.Background(), fixture.publishRequest(fixture.reviewState("two"), "generation-two", prior))
	if err != nil {
		t.Fatal(err)
	}
	if first.State.Findings[0].State != review.FindingAcknowledged || first.State.Findings[0].Acknowledgement.Method != review.AcknowledgementCheckbox {
		t.Fatalf("checked finding = %#v", first.State.Findings[0])
	}

	fixture.lateControl = func(body string) string { return strings.Replace(body, "- [x] ", "- [ ] ", 1) }
	fixture.lateApplied = false
	second, err := publisher.Publish(context.Background(), fixture.publishRequest(fixture.reviewStateFrom(first.State, "three"), "generation-three", RecoveredReport{NoteID: first.NoteID, State: first.State}))
	if err != nil {
		t.Fatal(err)
	}
	if second.State.Findings[0].State != review.FindingOpen || second.State.Findings[0].Acknowledgement != nil || len(second.State.Findings[0].History) < 2 {
		t.Fatalf("unchecked finding = %#v", second.State.Findings[0])
	}
}

func TestPublishBlocksCleanupWhenRetiringReportHasHumanReply(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	prior := fixture.addReport(t, codec, fixture.reviewState("old"), "generation-old", 0)
	fixture.replyTo = prior.NoteID

	result, err := publisher.Publish(context.Background(), fixture.publishRequest(fixture.reviewState("new"), "generation-new", prior))
	if err == nil || !errors.Is(err, ErrCleanupBlocked) {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if !fixture.hasNote(prior.NoteID) || fixture.countOwnedReports() != 2 || fixture.deleteCalls != 0 {
		t.Fatalf("cleanup state: notes=%v deletes=%d", fixture.noteIDs(), fixture.deleteCalls)
	}
}

func TestRecoverRejectsBindingAndExpectationConflicts(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	foreign := fixture.reviewState("foreign")
	foreign.MR.MRIID = 8
	fixture.addReport(t, codec, foreign, "generation-foreign", 0)
	if _, err := publisher.Recover(context.Background()); err == nil || !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("foreign binding error = %v", err)
	}

	fixture.resetNotes()
	prior := fixture.addReport(t, codec, fixture.reviewState("old"), "generation-old", 0)
	request := fixture.publishRequest(fixture.reviewState("new"), "generation-new", prior)
	request.Expected.NoteID++
	if _, err := publisher.Publish(context.Background(), request); err == nil || !errors.Is(err, ErrStalePublication) {
		t.Fatalf("predecessor mismatch error = %v", err)
	}
}

func TestRecoverRequiresAuthenticatedOwnerBeyondCopiedMarker(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	report := fixture.addReport(t, codec, fixture.reviewState("copied"), "generation-copied", 0)
	fixture.mu.Lock()
	fixture.notes[report.NoteID].AuthorID = 9
	fixture.mu.Unlock()

	recovery, err := publisher.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Current != nil || len(recovery.Superseded) != 0 {
		t.Fatalf("human-copied marker was treated as owned: %#v", recovery)
	}
}

func TestPublishRejectsOverflowAndRestrictedHiddenStateBeforeMutation(t *testing.T) {
	fixture, publisher, _ := newPublicationFixture(t)
	state := fixture.reviewState("large")
	state.Findings[0].Explanation = strings.Repeat("x", 2048)
	request := fixture.publishRequest(state, "generation-large", RecoveredReport{})
	request.Expected = PublicationExpectation{}
	request.MaxReportBytes = 128
	if _, err := publisher.Publish(context.Background(), request); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("overflow error = %v", err)
	}
	if fixture.createCalls != 0 {
		t.Fatalf("overflow created %d reports", fixture.createCalls)
	}

	for name, inject := range map[string]func(*review.State, *PublishRequest){
		"evidence": func(state *review.State, _ *PublishRequest) {
			state.Findings[0].Evidence = []review.Evidence{{Explanation: "clean visible summary", SourceRefs: []review.SourceReferenceID{"restricted-note"}}}
		},
		"acknowledgement": func(state *review.State, _ *PublishRequest) {
			state.Findings[0].Acknowledgement = &review.Acknowledgement{Method: review.AcknowledgementAIDiscussion, SourceRefs: []review.SourceReferenceID{"restricted-note"}, At: time.Now()}
		},
		"history": func(state *review.State, _ *PublishRequest) {
			state.Findings[0].History = append(state.Findings[0].History, review.FindingEvent{Kind: review.FindingEventAcknowledged, SourceRefs: []review.SourceReferenceID{"restricted-note"}})
		},
		"interaction": func(_ *review.State, request *PublishRequest) {
			request.Interactions = []markdown.Interaction{{SourceRefID: "restricted-note", GitLabID: 91, Human: true}}
		},
		"durable record": func(state *review.State, _ *PublishRequest) {
			state.Sources = []review.SourceRecord{{ID: "restricted-note", Kind: review.SourceGitLabNote, GitLabID: 91, Author: "developer", Human: true, Body: "must not be persisted", At: time.Now()}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := fixture.reviewState("restricted-" + name)
			request := fixture.publishRequest(state, review.PublicationGeneration("generation-restricted-"+name), RecoveredReport{})
			request.Expected = PublicationExpectation{}
			request.RestrictedSourceRefs = map[review.SourceReferenceID]struct{}{"restricted-note": {}}
			inject(&request.State, &request)
			if _, err := publisher.Publish(context.Background(), request); err == nil || !strings.Contains(err.Error(), "restricted source") {
				t.Fatalf("restricted source error = %v", err)
			}
		})
	}
	if fixture.createCalls != 0 {
		t.Fatalf("restricted state created %d reports", fixture.createCalls)
	}
}

func TestPublishRetainsReferencedHumanSourceTextAndAttributionBeforeDelete(t *testing.T) {
	t.Run("retained", func(t *testing.T) {
		fixture, publisher, codec := newPublicationFixture(t)
		priorState := fixture.reviewState("source-old")
		priorState.Sources = []review.SourceRecord{{
			ID: "discussion-91", Kind: review.SourceGitLabDiscussion, GitLabID: 91,
			Author: "alice", Human: true, Body: "This limitation is accepted for the current rollout.",
			At: time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC),
		}}
		priorState.Findings[0].Evidence = []review.Evidence{{Explanation: "The limitation was explicitly accepted.", SourceRefs: []review.SourceReferenceID{"discussion-91"}}}
		prior := fixture.addReport(t, codec, priorState, "generation-source-old", 0)
		nextState := fixture.reviewStateFrom(prior.State, "source-new")
		nextState.Sources = append([]review.SourceRecord(nil), prior.State.Sources...)

		result, err := publisher.Publish(context.Background(), fixture.publishRequest(nextState, "generation-source-new", prior))
		if err != nil {
			t.Fatal(err)
		}
		if fixture.hasNote(prior.NoteID) {
			t.Fatal("predecessor remained after durable source was retained")
		}
		recovered, err := publisher.Recover(context.Background())
		if err != nil || recovered.Current == nil {
			t.Fatalf("recover successor: %#v, %v", recovered, err)
		}
		if len(recovered.Current.State.Sources) != 1 {
			t.Fatalf("successor sources = %#v", recovered.Current.State.Sources)
		}
		source := recovered.Current.State.Sources[0]
		if source.Body != "This limitation is accepted for the current rollout." || source.Author != "alice" || source.GitLabID != 91 || !source.Human {
			t.Fatalf("durable human source = %#v; publish result=%#v", source, result)
		}
	})

	t.Run("missing record blocks cleanup", func(t *testing.T) {
		fixture, publisher, codec := newPublicationFixture(t)
		priorState := fixture.reviewState("source-missing-old")
		priorState.Sources = []review.SourceRecord{{ID: "note-92", Kind: review.SourceGitLabNote, GitLabID: 92, Author: "bob", Human: true, Body: "Please retain this evidence."}}
		priorState.Findings[0].History = append(priorState.Findings[0].History, review.FindingEvent{Kind: review.FindingEventAcknowledged, SourceRefs: []review.SourceReferenceID{"note-92"}})
		prior := fixture.addReport(t, codec, priorState, "generation-source-missing-old", 0)
		nextState := fixture.reviewStateFrom(prior.State, "source-missing-new")

		_, err := publisher.Publish(context.Background(), fixture.publishRequest(nextState, "generation-source-missing-new", prior))
		if err == nil || !errors.Is(err, ErrCleanupBlocked) || !strings.Contains(err.Error(), "not retained") {
			t.Fatalf("missing source error = %v", err)
		}
		if !fixture.hasNote(prior.NoteID) {
			t.Fatal("publisher deleted the only copy of missing human evidence")
		}
	})

	t.Run("legacy GitLab reference without predecessor record blocks cleanup", func(t *testing.T) {
		fixture, publisher, codec := newPublicationFixture(t)
		priorState := fixture.reviewState("source-legacy-old")
		priorState.Findings[0].Evidence = []review.Evidence{{Explanation: "Legacy discussion evidence.", SourceRefs: []review.SourceReferenceID{"legacy-discussion-93"}}}
		prior := fixture.addReport(t, codec, priorState, "generation-source-legacy-old", 0)
		nextState := fixture.reviewStateFrom(prior.State, "source-legacy-new")
		request := fixture.publishRequest(nextState, "generation-source-legacy-new", prior)
		request.SourceKinds = map[review.SourceReferenceID]review.SourceReferenceKind{"legacy-discussion-93": review.SourceGitLabDiscussion}

		_, err := publisher.Publish(context.Background(), request)
		if err == nil || !errors.Is(err, ErrCleanupBlocked) || !strings.Contains(err.Error(), "lacks a durable predecessor record") {
			t.Fatalf("legacy source error = %v", err)
		}
		if !fixture.hasNote(prior.NoteID) {
			t.Fatal("publisher deleted legacy GitLab evidence without durable provenance")
		}
		if err := validateRetainedSourceRecords(prior.State, nextState, map[review.SourceReferenceID]review.SourceReferenceKind{"legacy-discussion-93": review.SourceRepositoryDiff}); err != nil {
			t.Fatalf("repository evidence incorrectly required a durable human record: %v", err)
		}
		if err := validateRetainedSourceRecords(prior.State, nextState, nil); err == nil || !strings.Contains(err.Error(), "unknown provenance") {
			t.Fatalf("unknown legacy provenance was accepted: %v", err)
		}
	})
}

func TestPublishRecoversDuplicateGenerationAndRetainsUnrelatedNote(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	old := fixture.addReport(t, codec, fixture.reviewState("old"), "generation-old", 0)
	state := fixture.reviewState("new")
	first := fixture.addReport(t, codec, state, "generation-new", old.NoteID)
	second := fixture.addReport(t, codec, state, "generation-new", old.NoteID)
	fixture.addUnrelatedNote("developer note")

	request := fixture.publishRequest(state, "generation-new", old)
	result, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.NoteID != second.NoteID || fixture.hasNote(old.NoteID) || fixture.hasNote(first.NoteID) || !fixture.hasNote(second.NoteID) {
		t.Fatalf("duplicate recovery result=%#v notes=%v", result, fixture.noteIDs())
	}
	if !fixture.hasBody("developer note") {
		t.Fatal("unrelated developer note was removed")
	}
}

func TestSyncPublishedLabelsIsGenerationBoundAndDoesNotRewriteHistory(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	report := fixture.addReport(t, codec, fixture.reviewState("labels"), "generation-labels", 0)
	fixture.labels = []string{"unrelated", "ai-review::verdict::needs-review"}
	fixture.labelDefinitions = []string{"unrelated", "ai-review::verdict::approved", "ai-review::verdict::needs-review"}
	plan := LabelPlan{Desired: []string{"ai-review::verdict::approved"}, ManagedScopes: []string{"ai-review::verdict"}}
	fresh := func(context.Context, review.State) error { return nil }

	for range 2 {
		if _, err := publisher.SyncPublishedLabels(context.Background(), PublicationExpectation{NoteID: report.NoteID, Generation: "generation-labels"}, report.State, plan, false, fresh); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.labelUpdateCalls != 1 || !containsString(fixture.labels, "unrelated") || !containsString(fixture.labels, "ai-review::verdict::approved") {
		t.Fatalf("labels=%v updates=%d", fixture.labels, fixture.labelUpdateCalls)
	}
	recovered, err := publisher.Recover(context.Background())
	if err != nil || recovered.Current == nil || recovered.Current.State.Publication.StateDigest != report.State.Publication.StateDigest {
		t.Fatalf("report history changed during label repair: %#v, %v", recovered, err)
	}
	if _, err := publisher.SyncPublishedLabels(context.Background(), PublicationExpectation{NoteID: report.NoteID, Generation: "other-generation"}, report.State, plan, false, fresh); err == nil || !errors.Is(err, ErrStalePublication) {
		t.Fatalf("changed generation error = %v", err)
	}
	if fixture.labelUpdateCalls != 1 {
		t.Fatalf("stale generation mutated labels, updates=%d", fixture.labelUpdateCalls)
	}
}

func TestSyncPublishedLabelsClearsManagedVerdictWhenSnapshotIsKnownStale(t *testing.T) {
	fixture, publisher, codec := newPublicationFixture(t)
	report := fixture.addReport(t, codec, fixture.reviewState("stale-labels"), "generation-stale", 0)
	fixture.labels = []string{"unrelated", "ai-review::verdict::approved"}
	fixture.labelDefinitions = []string{"unrelated", "ai-review::verdict::approved"}
	plan := LabelPlan{Desired: []string{"ai-review::verdict::approved"}, ManagedScopes: []string{"ai-review::verdict"}}
	stale := func(context.Context, review.State) error { return errors.New("source head changed") }

	_, err := publisher.SyncPublishedLabels(context.Background(), PublicationExpectation{NoteID: report.NoteID, Generation: "generation-stale"}, report.State, plan, false, stale)
	if err == nil || !errors.Is(err, ErrStalePublication) {
		t.Fatalf("stale label error = %v", err)
	}
	if fixture.labelUpdateCalls != 1 || !containsString(fixture.labels, "unrelated") || containsString(fixture.labels, "ai-review::verdict::approved") {
		t.Fatalf("stale labels=%v updates=%d", fixture.labels, fixture.labelUpdateCalls)
	}
}

type publicationFixture struct {
	t                    *testing.T
	server               *httptest.Server
	codec                *markdown.Codec
	mu                   sync.Mutex
	notes                map[int64]*fixtureNote
	nextID               int64
	uncertainCreate      bool
	corruptSuccessorRead bool
	lateControl          func(string) string
	lateApplied          bool
	replyTo              int64
	labels               []string
	labelDefinitions     []string
	createCalls          int
	updateCalls          int
	deleteCalls          int
	labelUpdateCalls     int
}

type fixtureNote struct {
	ID       int64
	Body     string
	AuthorID int64
	Internal bool
}

func newPublicationFixture(t *testing.T) (*publicationFixture, *Publisher, *markdown.Codec) {
	t.Helper()
	fixture := &publicationFixture{t: t, notes: make(map[int64]*fixtureNote), nextID: 100, codec: markdown.NewCodec()}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	client := newTestClient(t, fixture.server, testJobToken, WithRetryPolicy(0, 0, 0))
	publisher, err := NewPublisher(client, fixture.codec)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, publisher, fixture.codec
}

func (f *publicationFixture) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writer.Header().Set("X-Next-Page", "")
	path := request.URL.Path
	switch {
	case request.Method == http.MethodGet && strings.HasSuffix(path, "/user"):
		assertCredential(f.t, request, CredentialAPIToken)
		writeJSON(f.t, writer, http.StatusOK, map[string]any{"id": 5, "username": "review-bot", "bot": true})
	case strings.HasSuffix(path, "/discussions") && request.Method == http.MethodGet:
		assertCredential(f.t, request, CredentialAPIToken)
		if f.replyTo == 0 {
			writeJSON(f.t, writer, http.StatusOK, []any{})
			return
		}
		writeJSON(f.t, writer, http.StatusOK, []any{map[string]any{
			"id":              "thread",
			"individual_note": false,
			"notes": []any{
				f.notePayload(f.notes[f.replyTo]),
				map[string]any{"id": 999, "body": "human reply", "author": map[string]any{"id": 9, "username": "developer"}, "noteable_iid": 7},
			},
		}})
	case strings.HasSuffix(path, "/labels") && request.Method == http.MethodGet:
		assertCredential(f.t, request, CredentialAPIToken)
		values := make([]any, 0, len(f.labelDefinitions))
		for index, name := range f.labelDefinitions {
			values = append(values, map[string]any{"id": index + 1, "name": name})
		}
		writeJSON(f.t, writer, http.StatusOK, values)
	case strings.HasSuffix(path, "/merge_requests/7") && request.Method == http.MethodGet:
		assertCredential(f.t, request, CredentialJobToken)
		writeJSON(f.t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "labels": f.labels})
	case strings.HasSuffix(path, "/merge_requests/7") && request.Method == http.MethodPut:
		assertCredential(f.t, request, CredentialAPIToken)
		var input map[string]string
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			f.t.Fatal(err)
		}
		f.labelUpdateCalls++
		f.labels = applyLabels(f.labels, strings.Split(input["add_labels"], ","), strings.Split(input["remove_labels"], ","))
		writeJSON(f.t, writer, http.StatusOK, map[string]any{"id": 70, "iid": 7, "labels": f.labels})
	case strings.HasSuffix(path, "/notes"):
		f.serveNotes(writer, request)
	case strings.Contains(path, "/notes/"):
		f.serveNote(writer, request)
	default:
		f.t.Fatalf("unexpected publication request %s %s", request.Method, path)
	}
}

func (f *publicationFixture) serveNotes(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		assertCredential(f.t, request, CredentialJobToken)
		ids := f.noteIDsLocked()
		values := make([]any, 0, len(ids))
		for _, id := range ids {
			values = append(values, f.notePayload(f.notes[id]))
		}
		writeJSON(f.t, writer, http.StatusOK, values)
	case http.MethodPost:
		assertCredential(f.t, request, CredentialAPIToken)
		var input struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			f.t.Fatal(err)
		}
		f.createCalls++
		f.nextID++
		note := &fixtureNote{ID: f.nextID, Body: input.Body, AuthorID: 5}
		f.notes[note.ID] = note
		if f.uncertainCreate {
			f.uncertainCreate = false
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(f.t, writer, http.StatusCreated, f.notePayload(note))
	default:
		f.t.Fatalf("unexpected notes method %s", request.Method)
	}
}

func (f *publicationFixture) serveNote(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimSuffix(request.URL.Path, "/"), "/")
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		f.t.Fatal(err)
	}
	note := f.notes[id]
	if note == nil {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	switch request.Method {
	case http.MethodGet:
		if credentialOf(request) != CredentialJobToken && credentialOf(request) != CredentialAPIToken {
			f.t.Fatal("note read had no credential")
		}
		if f.lateControl != nil && !f.lateApplied && id != f.nextID {
			note.Body = f.lateControl(note.Body)
			f.lateApplied = true
		}
		payload := f.notePayload(note)
		if f.corruptSuccessorRead && id == f.nextID {
			payload["body"] = note.Body + "\ncorrupt"
		}
		writeJSON(f.t, writer, http.StatusOK, payload)
	case http.MethodPut:
		assertCredential(f.t, request, CredentialAPIToken)
		var input struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			f.t.Fatal(err)
		}
		f.updateCalls++
		note.Body = input.Body
		writeJSON(f.t, writer, http.StatusOK, f.notePayload(note))
	case http.MethodDelete:
		assertCredential(f.t, request, CredentialAPIToken)
		f.deleteCalls++
		delete(f.notes, id)
		writer.WriteHeader(http.StatusNoContent)
	default:
		f.t.Fatalf("unexpected note method %s", request.Method)
	}
}

func (f *publicationFixture) notePayload(note *fixtureNote) map[string]any {
	return map[string]any{"id": note.ID, "body": note.Body, "internal": note.Internal, "author": map[string]any{"id": note.AuthorID, "username": fmt.Sprintf("user-%d", note.AuthorID)}, "noteable_iid": 7, "project_id": 70}
}

func (f *publicationFixture) reviewState(suffix string) review.State {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	unchecked := false
	return review.State{
		SchemaVersion: 1,
		MR:            review.MRIdentity{BaseURL: f.server.URL + "/gitlab", Project: "group/project", MRIID: 7},
		Snapshot:      review.Snapshot{ID: review.SnapshotID("snapshot-" + suffix), BaseCommit: "base", HeadCommit: "head-" + suffix, SourceTree: "head", CapturedAt: now},
		Fingerprint:   review.Fingerprint("fingerprint-" + suffix),
		Findings: []review.Finding{{
			ID: "finding-1", IdentityKey: "stable-finding", Category: review.CategoryRisk, Subcategory: review.SubcategoryCorrections,
			Relationship: review.RelationshipIntroduced, Title: "Regression", Explanation: "A cross-file regression.",
			Assessment: review.AssessmentPresent, State: review.FindingOpen, LastRenderedCheckbox: &unchecked,
			History: []review.FindingEvent{{Kind: review.FindingEventCreated, At: now}}, FirstSeenAt: now, LastSeenAt: now,
		}},
		Runs: []review.Run{{ID: review.RunID("run-" + suffix), Fingerprint: review.Fingerprint("fingerprint-" + suffix), StartedAt: now, CompletedAt: now, Completion: review.SubmissionComplete, Coverage: []review.UnitCoverage{{UnitID: "unit-1", Outcome: review.CoverageComplete}}}},
	}
}

func (f *publicationFixture) reviewStateFrom(prior review.State, suffix string) review.State {
	state := f.reviewState(suffix)
	state.Findings = cloneReviewState(prior).Findings
	return state
}

func (f *publicationFixture) addReport(t *testing.T, codec *markdown.Codec, state review.State, generation review.PublicationGeneration, predecessorID int64) RecoveredReport {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	noteID := f.nextID
	state.Publication = &review.Publication{Generation: generation, NoteID: noteID, PredecessorNoteID: predecessorID, PublishedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	digest, err := calculateStateDigest(codec, state)
	if err != nil {
		t.Fatal(err)
	}
	state.Publication.StateDigest = digest
	body, err := codec.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	f.notes[noteID] = &fixtureNote{ID: noteID, Body: body, AuthorID: 5}
	decoded, err := codec.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	return RecoveredReport{NoteID: noteID, Source: body, State: decoded}
}

func (f *publicationFixture) addUnrelatedNote(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.notes[f.nextID] = &fixtureNote{ID: f.nextID, Body: body, AuthorID: 9}
}

func (f *publicationFixture) publishRequest(state review.State, generation review.PublicationGeneration, predecessor RecoveredReport) PublishRequest {
	expected := PublicationExpectation{}
	if predecessor.NoteID > 0 {
		expected = PublicationExpectation{NoteID: predecessor.NoteID, Generation: predecessor.State.Publication.Generation}
	}
	return PublishRequest{
		State: state, Generation: generation, Expected: expected,
		PublishedAt: time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC), MaxReportBytes: 900 * 1024,
		Freshness: func(context.Context, review.State) error { return nil },
	}
}

func (f *publicationFixture) hasNote(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.notes[id]
	return ok
}

func (f *publicationFixture) hasBody(body string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, note := range f.notes {
		if note.Body == body {
			return true
		}
	}
	return false
}

func (f *publicationFixture) noteIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.noteIDsLocked()
}

func (f *publicationFixture) noteIDsLocked() []int64 {
	ids := make([]int64, 0, len(f.notes))
	for id := range f.notes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (f *publicationFixture) countOwnedReports() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, note := range f.notes {
		if note.AuthorID == 5 && strings.Contains(note.Body, ownedStatePrefix) {
			count++
		}
	}
	return count
}

func (f *publicationFixture) resetNotes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = make(map[int64]*fixtureNote)
	f.nextID = 100
}
