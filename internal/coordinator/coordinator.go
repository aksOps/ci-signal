package coordinator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/copilot"
	"ci-signal/internal/gitlab"
	markdowncodec "ci-signal/internal/markdown"
	"ci-signal/internal/repository"
	"ci-signal/internal/review"
)

type Coordinator struct {
	config      config.Config
	hooks       Hooks
	codec       *markdowncodec.Codec
	reconciler  *review.Reconciler
	checkpoints *checkpointStore
	now         func() time.Time
}

func New(settings config.Config, hooks Hooks) (*Coordinator, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if hooks.Capture == nil || hooks.Recover == nil || hooks.Review == nil || hooks.Recheck == nil || hooks.Publish == nil || hooks.HandleStale == nil || hooks.RepairLabels == nil {
		return nil, errors.New("coordinator hooks are incomplete")
	}
	store, err := newCheckpointStore(settings.Copilot.StateDir)
	if err != nil {
		return nil, err
	}
	return &Coordinator{config: settings, hooks: hooks, codec: markdowncodec.NewCodec(), reconciler: review.NewReconciler(), checkpoints: store, now: time.Now}, nil
}

func (c *Coordinator) Run(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Limits.OverallTimeout.Value())
	defer cancel()
	started := c.now().UTC()
	capture, err := c.hooks.Capture(ctx, started)
	if err != nil {
		return Result{}, err
	}
	if !capture.Context.Complete || capture.Context.MergeRequest == nil {
		return Result{}, errors.New("GitLab merge-request context is incomplete")
	}
	if len(capture.Context.RestrictedNotes) != 0 {
		// Restricted text is intentionally absent from prompts, fingerprints, and state.
		capture.Context.RestrictedNotes = nil
	}
	recovery, err := c.hooks.Recover(ctx)
	if err != nil {
		return Result{}, err
	}
	state := c.initialState(capture, recovery)
	controlsChanged := false
	if recovery.Current != nil && recovery.Current.Source != "" {
		state, controlsChanged, err = c.codec.ReconcileControls(recovery.Current.Source, state, nil, started)
		if err != nil {
			return Result{}, fmt.Errorf("reconcile human report controls: %w", err)
		}
	}
	capture.SourceRefs = mergeSourceReferences(capture.SourceRefs, associateSources(capture.Context, state.Findings, recovery.BotUserID))
	state.Sources = mergeSourceRecords(state.Sources, publicSourceRecords(capture.Context, recovery.BotUserID))
	fingerprint, err := Fingerprint(capture, c.config)
	if err != nil {
		return Result{}, err
	}
	labels, err := c.labels(state, review.VerdictNeedsReview, overallCoverage(latestCoverage(state)))
	if err != nil {
		return Result{}, err
	}
	if state.Fingerprint == fingerprint && sameSnapshot(state.Snapshot, capture.Snapshot) && hasReusableReview(state, fingerprint) {
		state.Sources = referencedSourceRecords(state.Findings, state.Sources)
		verdict, completion := latestDecision(state)
		labels, err = c.labels(state, verdict, overallCoverage(latestCoverage(state)))
		if err != nil {
			return Result{}, err
		}
		if !controlsChanged && len(recovery.Superseded) == 0 {
			if !c.config.Diagnostics.DryRun {
				if err := c.hooks.RepairLabels(ctx, state, labels, capture); err != nil {
					return Result{}, err
				}
			}
			return Result{State: state, Verdict: verdict, Completion: completion, Fingerprint: fingerprint}, nil
		}
		return c.finish(ctx, state, recovery, capture, labels, verdict, completion, false)
	}

	state.Fingerprint = fingerprint
	state.Snapshot = domainSnapshot(capture.Snapshot)
	runID, err := randomID("run_")
	if err != nil {
		return Result{}, err
	}
	findings, coverage, telemetry, proposed, complete, usedAI, err := c.reviewBatches(ctx, capture, state.Findings, fingerprint, review.RunID(runID), started)
	if err != nil {
		return Result{}, err
	}
	state.Findings = findings
	state.Sources = referencedSourceRecords(state.Findings, state.Sources)
	completion := review.SubmissionPartial
	if complete {
		completion = review.SubmissionComplete
	}
	policy := review.VerdictPolicy{GatingCategories: make(map[review.Category]struct{}, len(c.config.Review.GatingCategories))}
	for _, category := range c.config.Review.GatingCategories {
		policy.GatingCategories[category] = struct{}{}
	}
	decision := review.DeriveVerdict(review.VerdictInput{ProposedVerdict: proposed, Completion: completion, Coverage: coverage, Findings: state.Findings}, policy)
	run := review.Run{ID: review.RunID(runID), Fingerprint: fingerprint, StartedAt: started, CompletedAt: c.now().UTC(), Completion: completion, Verdict: decision.Verdict, Coverage: coverage, Telemetry: telemetry}
	state.Runs = append(state.Runs, run)
	labels, err = c.labels(state, decision.Verdict, overallCoverage(coverage))
	if err != nil {
		return Result{}, err
	}
	return c.finish(ctx, state, recovery, capture, labels, decision.Verdict, completion, usedAI)
}

func (c *Coordinator) reviewBatches(ctx context.Context, capture Capture, findings []review.Finding, fingerprint review.Fingerprint, runID review.RunID, started time.Time) ([]review.Finding, []review.UnitCoverage, review.Telemetry, review.Verdict, bool, bool, error) {
	units := append([]repository.Unit(nil), capture.Inventory.Units...)
	sort.Slice(units, func(i, j int) bool { return units[i].ID < units[j].ID })
	if len(units) > c.config.Limits.MaxUnitsPerSession {
		units = append(units, repository.Unit{ID: integrationUnitID(fingerprint), Kind: repository.UnitPackage, Symbol: "cross-batch integration"})
	}
	requirements, err := capture.Inventory.UnitRequirements()
	if err != nil {
		return nil, nil, review.Telemetry{}, review.VerdictNeedsReview, false, false, err
	}
	batches, err := batchUnits(units, requirements, c.config.Limits.MaxUnitsPerSession)
	if err != nil {
		return nil, nil, review.Telemetry{}, review.VerdictNeedsReview, false, false, err
	}
	coverage := excludedCoverage(capture.Inventory)
	telemetry := review.Telemetry{UsageComplete: true}
	proposed := review.VerdictApproved
	complete := true
	usedAI := false
	findingBatches := assignFindingBatches(findings, batches)
	sessionsUsed := 0
	executions := make([]batchExecution, len(batches))
	for index, batch := range batches {
		assignment := review.Assignment{UnitIDs: map[review.ReviewUnitID]struct{}{}, UnitRequirements: map[review.ReviewUnitID]review.UnitRequirement{}, Findings: map[review.FindingID]review.KnownFinding{}, SourceRefs: copySources(capture.SourceRefs)}
		for _, unit := range batch {
			assignment.UnitIDs[unit.ID] = struct{}{}
			if requirement, ok := requirements[unit.ID]; ok {
				assignment.UnitRequirements[unit.ID] = requirement
			}
		}
		for _, finding := range findings {
			if findingBatches[finding.ID] == index {
				assignment.Findings[finding.ID] = knownFinding(finding)
			}
		}
		batchKey := assignmentKey(batch, assignment.Findings)
		result, err := c.checkpoints.load(fingerprint, batchKey)
		if err != nil {
			return nil, nil, review.Telemetry{}, review.VerdictNeedsReview, false, usedAI, err
		}
		executions[index] = batchExecution{batch: batch, assignment: assignment, batchKey: batchKey, result: result}
		if result == nil {
			if sessionsUsed >= c.config.Limits.MaxSessions {
				executions[index].failure = "session budget exhausted"
				continue
			}
			sessionsUsed++
			executions[index].run = true
			usedAI = true
		}
	}

	semaphore := make(chan struct{}, c.config.Limits.MaxConcurrency)
	var wait sync.WaitGroup
	for index := range executions {
		if !executions[index].run {
			continue
		}
		wait.Add(1)
		go func(execution *batchExecution, batchIndex int) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			acceptance, err := acceptanceMetadata(runID, batchIndex, started)
			if err != nil {
				execution.err = err
				return
			}
			value, err := c.hooks.Review(ctx, BatchRequest{Fingerprint: fingerprint, Snapshot: capture.Snapshot, Guidance: capture.Guidance, Units: execution.batch, Assignment: execution.assignment, Prompt: batchPrompt(execution.batch, len(batches), capture.Context, execution.assignment.Findings, c.config.Tools.StructuralScans), Acceptance: acceptance, Scans: c.config.Tools.StructuralScans})
			execution.result = &value
			if err != nil || value.Accepted == nil {
				execution.failure = "review session failed"
				return
			}
			if err := c.checkpoints.save(fingerprint, execution.batchKey, value); err != nil {
				execution.err = err
				return
			}
		}(&executions[index], index)
	}
	wait.Wait()

	for _, execution := range executions {
		if execution.err != nil {
			return nil, nil, review.Telemetry{}, review.VerdictNeedsReview, false, usedAI, execution.err
		}
		if execution.result != nil {
			telemetry = mergeTelemetry(telemetry, execution.result.Telemetry)
		}
		if execution.failure != "" || execution.result == nil {
			complete = false
			for _, unit := range execution.batch {
				coverage = append(coverage, review.UnitCoverage{UnitID: unit.ID, Outcome: review.CoverageFailed, Explanation: execution.failure})
			}
			continue
		}
		accepted := execution.result.Accepted.Value
		if accepted.Completion != review.SubmissionComplete {
			complete = false
		}
		if accepted.Verdict == review.VerdictNeedsReview {
			proposed = review.VerdictNeedsReview
		}
		coverage = append(coverage, accepted.Coverage...)
		findings, err = c.reconciler.Reconcile(findings, accepted, review.ReconcileMetadata{RunID: runID, At: c.now().UTC(), NewFindingIDs: stableFindingIDs(*execution.result.Accepted)})
		if err != nil {
			return nil, nil, review.Telemetry{}, review.VerdictNeedsReview, false, usedAI, err
		}
	}
	if len(batches) == 0 {
		complete = false
	}
	for _, item := range coverage {
		if item.Outcome != review.CoverageComplete && item.Outcome != review.CoverageExcludedByPolicy {
			complete = false
		}
	}
	return findings, dedupeCoverage(coverage), telemetry, proposed, complete, usedAI, nil
}

type batchExecution struct {
	batch      []repository.Unit
	assignment review.Assignment
	batchKey   string
	result     *copilot.Result
	run        bool
	failure    string
	err        error
}

func (c *Coordinator) finish(ctx context.Context, state review.State, recovery gitlab.Recovery, capture Capture, labels gitlab.LabelPlan, verdict review.Verdict, completion review.SubmissionCompletion, usedAI bool) (Result, error) {
	if err := c.hooks.Recheck(ctx, capture); err != nil {
		if !c.config.Diagnostics.DryRun {
			if generation, generationErr := randomID("pub_"); generationErr == nil {
				_ = c.hooks.HandleStale(ctx, PublicationRequest{State: state, Recovery: recovery, Generation: review.PublicationGeneration(generation), Labels: labels, Captured: capture})
			}
		}
		return Result{State: state, Verdict: review.VerdictNeedsReview, Completion: review.SubmissionPartial, UsedAI: usedAI, Fingerprint: state.Fingerprint}, fmt.Errorf("review became stale: %w", err)
	}
	if c.config.Diagnostics.DryRun {
		return Result{State: state, Verdict: verdict, Completion: completion, UsedAI: usedAI, Fingerprint: state.Fingerprint}, nil
	}
	generation, err := randomID("pub_")
	if err != nil {
		return Result{}, err
	}
	published, err := c.hooks.Publish(ctx, PublicationRequest{State: state, Recovery: recovery, Generation: review.PublicationGeneration(generation), Labels: labels, Captured: capture})
	if err != nil {
		return Result{State: state, Verdict: verdict, Completion: completion, UsedAI: usedAI, Fingerprint: state.Fingerprint}, err
	}
	return Result{State: published, Verdict: verdict, Completion: completion, UsedAI: usedAI, Published: true, Fingerprint: state.Fingerprint}, nil
}

func (c *Coordinator) initialState(capture Capture, recovery gitlab.Recovery) review.State {
	if recovery.Current != nil {
		return recovery.Current.State
	}
	return review.State{SchemaVersion: 1, MR: review.MRIdentity{BaseURL: c.config.GitLab.BaseURL, Project: c.config.GitLab.Project, MRIID: c.config.GitLab.MRIID}, Snapshot: domainSnapshot(capture.Snapshot)}
}

func (c *Coordinator) labels(state review.State, verdict review.Verdict, coverage review.CoverageOutcome) (gitlab.LabelPlan, error) {
	telemetry := review.Telemetry{UsageComplete: false}
	if len(state.Runs) != 0 {
		telemetry = state.Runs[len(state.Runs)-1].Telemetry
	}
	return gitlab.DeriveLabels(c.config.Labels, c.config.Metadata, gitlab.LabelInput{Verdict: verdict, Coverage: coverage, Telemetry: telemetry})
}

func domainSnapshot(value repository.Snapshot) review.Snapshot {
	return review.Snapshot{ID: value.ID, BaseCommit: value.BaseCommit, HeadCommit: value.HeadCommit, SourceTree: value.HeadCommit, CapturedAt: value.CapturedAt}
}

func sameSnapshot(left review.Snapshot, right repository.Snapshot) bool {
	return left.ID == right.ID && left.BaseCommit == right.BaseCommit && left.HeadCommit == right.HeadCommit
}

func hasReusableReview(state review.State, fingerprint review.Fingerprint) bool {
	if len(state.Runs) == 0 {
		return false
	}
	run := state.Runs[len(state.Runs)-1]
	return run.Fingerprint == fingerprint && run.Completion == review.SubmissionComplete && completeCoverage(run.Coverage)
}

func completeCoverage(items []review.UnitCoverage) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.Outcome != review.CoverageComplete && item.Outcome != review.CoverageExcludedByPolicy {
			return false
		}
	}
	return true
}

func latestCoverage(state review.State) []review.UnitCoverage {
	if len(state.Runs) == 0 {
		return nil
	}
	return state.Runs[len(state.Runs)-1].Coverage
}

func latestDecision(state review.State) (review.Verdict, review.SubmissionCompletion) {
	if len(state.Runs) == 0 {
		return review.VerdictNeedsReview, review.SubmissionPartial
	}
	run := state.Runs[len(state.Runs)-1]
	if run.Verdict == "" {
		run.Verdict = review.VerdictNeedsReview
	}
	return run.Verdict, run.Completion
}

func overallCoverage(items []review.UnitCoverage) review.CoverageOutcome {
	result := review.CoverageComplete
	for _, item := range items {
		if item.Outcome == review.CoverageFailed {
			return review.CoverageFailed
		}
		if item.Outcome == review.CoveragePartial {
			result = review.CoveragePartial
		}
	}
	return result
}

func batchUnits(units []repository.Unit, requirements map[review.ReviewUnitID]review.UnitRequirement, maximum int) ([][]repository.Unit, error) {
	byID := make(map[review.ReviewUnitID]repository.Unit, len(units))
	for _, unit := range units {
		byID[unit.ID] = unit
	}
	used := make(map[review.ReviewUnitID]struct{})
	groups := make([][]repository.Unit, 0, len(units))
	for _, unit := range units {
		if _, ok := used[unit.ID]; ok {
			continue
		}
		group := []repository.Unit{unit}
		used[unit.ID] = struct{}{}
		if requirement, ok := requirements[unit.ID]; ok && requirement.EnclosingUnitID != "" {
			enclosing, exists := byID[requirement.EnclosingUnitID]
			if !exists {
				return nil, fmt.Errorf("unit %q enclosing unit %q is absent", unit.ID, requirement.EnclosingUnitID)
			}
			if _, seen := used[enclosing.ID]; !seen {
				group = append(group, enclosing)
				used[enclosing.ID] = struct{}{}
			}
		}
		groups = append(groups, group)
	}
	var result [][]repository.Unit
	for _, group := range groups {
		if len(group) > maximum {
			return nil, errors.New("related review-unit group exceeds max_units_per_session")
		}
		if len(result) == 0 || len(result[len(result)-1])+len(group) > maximum {
			result = append(result, nil)
		}
		result[len(result)-1] = append(result[len(result)-1], group...)
	}
	return result, nil
}

func excludedCoverage(inventory repository.Inventory) []review.UnitCoverage {
	var result []review.UnitCoverage
	for _, exclusion := range inventory.Exclusions {
		if exclusion.Excluded {
			result = append(result, review.UnitCoverage{UnitID: review.ReviewUnitID("excluded:" + exclusion.Path), Outcome: review.CoverageExcludedByPolicy, Explanation: string(exclusion.Reason)})
		}
	}
	return result
}

func copySources(input map[review.SourceReferenceID]review.SourceReference) map[review.SourceReferenceID]review.SourceReference {
	result := make(map[review.SourceReferenceID]review.SourceReference, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func mergeSourceReferences(left, right map[review.SourceReferenceID]review.SourceReference) map[review.SourceReferenceID]review.SourceReference {
	result := copySources(left)
	for id, value := range right {
		result[id] = value
	}
	return result
}

func mergeSourceRecords(previous, current []review.SourceRecord) []review.SourceRecord {
	byID := make(map[review.SourceReferenceID]review.SourceRecord, len(previous)+len(current))
	for _, value := range previous {
		byID[value.ID] = value
	}
	for _, value := range current {
		byID[value.ID] = value
	}
	result := make([]review.SourceRecord, 0, len(byID))
	for _, value := range byID {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func referencedSourceRecords(findings []review.Finding, available []review.SourceRecord) []review.SourceRecord {
	references := make(map[review.SourceReferenceID]struct{})
	for _, finding := range findings {
		for _, evidence := range finding.Evidence {
			for _, id := range evidence.SourceRefs {
				references[id] = struct{}{}
			}
		}
		if finding.Acknowledgement != nil {
			for _, id := range finding.Acknowledgement.SourceRefs {
				references[id] = struct{}{}
			}
		}
		for _, event := range finding.History {
			for _, id := range event.SourceRefs {
				references[id] = struct{}{}
			}
		}
	}
	result := make([]review.SourceRecord, 0, len(available))
	for _, value := range available {
		if _, ok := references[value.ID]; ok {
			result = append(result, value)
		}
	}
	return result
}

func knownFinding(finding review.Finding) review.KnownFinding {
	consumed := make(map[review.SourceReferenceID]struct{})
	for _, event := range finding.History {
		if event.Kind == review.FindingEventAcknowledged {
			for _, source := range event.SourceRefs {
				consumed[source] = struct{}{}
			}
		}
	}
	return review.KnownFinding{ID: finding.ID, State: finding.State, Acknowledgement: finding.Acknowledgement, ConsumedSourceRefs: consumed}
}

func assignmentKey(units []repository.Unit, findings map[review.FindingID]review.KnownFinding) string {
	values := make([]string, 0, len(units)+len(findings))
	for _, unit := range units {
		values = append(values, "u:"+string(unit.ID))
	}
	for id := range findings {
		values = append(values, "f:"+string(id))
	}
	sort.Strings(values)
	return strings.Join(values, "\x00")
}

func batchPrompt(units []repository.Unit, count int, context gitlab.Context, findings map[review.FindingID]review.KnownFinding, scans []config.StructuralScan) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Assess assigned review units for a %d-batch review. Retrieve evidence with host tools; do not infer completeness from truncation. Assigned units:\n", count)
	for _, unit := range units {
		fmt.Fprintf(&builder, "- %s %s %s\n", unit.ID, unit.Kind, unit.Symbol)
	}
	if len(scans) != 0 {
		builder.WriteString("\nConfigured repository-wide structural scans available through structural_scan:\n")
		for _, scan := range scans {
			fmt.Fprintf(&builder, "- %s (%s); scan matches are evidence, not assessed-unit coverage\n", scan.Name, scan.Language)
		}
	}
	if context.MergeRequest != nil {
		fmt.Fprintf(&builder, "\nMerge request title: %s\nDescription:\n%s\n", context.MergeRequest.Title, context.MergeRequest.Description)
	}
	if len(findings) != 0 {
		builder.WriteString("\nKnown finding IDs requiring explicit reassessment:\n")
		ids := make([]string, 0, len(findings))
		for id := range findings {
			ids = append(ids, string(id))
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&builder, "- %s\n", id)
		}
	}
	for _, note := range context.Notes {
		if note == nil || note.System || note.Internal || note.Confidential || strings.Contains(note.Body, "<!-- ci-signal-state") {
			continue
		}
		label := "Public note"
		if context.AuthorKinds[note.Author.ID] == gitlab.AuthorHuman {
			label = "Public human note"
		}
		fmt.Fprintf(&builder, "\n%s %d by %s:\n%s\n", label, note.ID, note.Author.Username, note.Body)
	}
	return builder.String()
}

func associateSources(context gitlab.Context, findings []review.Finding, botID int64) map[review.SourceReferenceID]review.SourceReference {
	result := make(map[review.SourceReferenceID]review.SourceReference)
	discussionKinds := discussionNoteKinds(context)
	discussionFindings := discussionFindingAssociations(context, findings)
	for _, note := range context.Notes {
		if note == nil || note.System || note.Internal || note.Confidential || strings.Contains(note.Body, "<!-- ci-signal-state") {
			continue
		}
		id := noteSourceID(note.ID, note.Body, note.UpdatedAt)
		kind := review.SourceGitLabNote
		if discussionKinds[note.ID] {
			kind = review.SourceGitLabDiscussion
		}
		reference := review.SourceReference{ID: id, Kind: kind, GitLabID: note.ID, Author: note.Author.Username, Human: confirmedHuman(context, note.Author.ID, botID)}
		matched := discussionFindings[note.ID]
		if matched == "" {
			matched = uniqueFindingTitleMatch(note.Body, findings)
		}
		reference.FindingID = matched
		result[id] = reference
	}
	return result
}

func publicSourceRecords(context gitlab.Context, botID int64) []review.SourceRecord {
	result := make([]review.SourceRecord, 0, len(context.Notes))
	discussionKinds := discussionNoteKinds(context)
	for _, note := range context.Notes {
		if note == nil || note.System || note.Internal || note.Confidential || strings.Contains(note.Body, "<!-- ci-signal-state") {
			continue
		}
		kind := review.SourceGitLabNote
		if discussionKinds[note.ID] {
			kind = review.SourceGitLabDiscussion
		}
		record := review.SourceRecord{ID: noteSourceID(note.ID, note.Body, note.UpdatedAt), Kind: kind, GitLabID: note.ID, Author: note.Author.Username, Human: confirmedHuman(context, note.Author.ID, botID), Body: note.Body}
		if note.UpdatedAt != nil {
			record.At = note.UpdatedAt.UTC()
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func confirmedHuman(context gitlab.Context, authorID, botID int64) bool {
	if authorID < 1 || (botID > 0 && authorID == botID) {
		return false
	}
	return context.AuthorKinds[authorID] == gitlab.AuthorHuman
}

func discussionNoteKinds(context gitlab.Context) map[int64]bool {
	result := make(map[int64]bool)
	for _, discussion := range context.Discussions {
		if discussion.Restricted {
			continue
		}
		for _, noteID := range discussion.NoteIDs {
			result[noteID] = true
		}
	}
	return result
}

func discussionFindingAssociations(context gitlab.Context, findings []review.Finding) map[int64]review.FindingID {
	notes := make(map[int64]string, len(context.Notes))
	for _, note := range context.Notes {
		if note != nil && !note.System && !note.Internal && !note.Confidential {
			notes[note.ID] = note.Body
		}
	}
	result := make(map[int64]review.FindingID)
	for _, discussion := range context.Discussions {
		if discussion.Restricted {
			continue
		}
		matches := make(map[review.FindingID]struct{})
		for _, noteID := range discussion.NoteIDs {
			if findingID := uniqueFindingTitleMatch(notes[noteID], findings); findingID != "" {
				matches[findingID] = struct{}{}
			}
		}
		if len(matches) != 1 {
			continue
		}
		var findingID review.FindingID
		for id := range matches {
			findingID = id
		}
		for _, noteID := range discussion.NoteIDs {
			result[noteID] = findingID
		}
	}
	return result
}

func uniqueFindingTitleMatch(body string, findings []review.Finding) review.FindingID {
	var matched review.FindingID
	for _, finding := range findings {
		if finding.Title == "" || !strings.Contains(body, finding.Title) {
			continue
		}
		if matched != "" {
			return ""
		}
		matched = finding.ID
	}
	return matched
}

func noteSourceID(id int64, body string, updatedAt *time.Time) review.SourceReferenceID {
	stamp := ""
	if updatedAt != nil {
		stamp = updatedAt.UTC().Format(time.RFC3339Nano)
	}
	digest := sha256.Sum256([]byte(body + "\x00" + stamp))
	return review.SourceReferenceID(fmt.Sprintf("note-%d-%s", id, hex.EncodeToString(digest[:6])))
}

func assignFindingBatches(findings []review.Finding, batches [][]repository.Unit) map[review.FindingID]int {
	unitBatch := make(map[review.ReviewUnitID]int)
	for index, batch := range batches {
		for _, unit := range batch {
			unitBatch[unit.ID] = index
		}
	}
	result := make(map[review.FindingID]int, len(findings))
	for _, finding := range findings {
		assigned := 0
		for _, unitID := range finding.AssignedUnits {
			if index, ok := unitBatch[unitID]; ok {
				assigned = index
				break
			}
		}
		result[finding.ID] = assigned
	}
	return result
}

func integrationUnitID(fingerprint review.Fingerprint) review.ReviewUnitID {
	return review.ReviewUnitID("integration:" + string(fingerprint)[3:19])
}

func stableFindingIDs(accepted review.AcceptedSubmission) map[int]review.FindingID {
	result := make(map[int]review.FindingID, len(accepted.Value.Findings))
	for index := range accepted.Value.Findings {
		digest := sha256.Sum256([]byte(accepted.Receipt.Digest + "\x00" + fmt.Sprint(index)))
		result[index] = review.FindingID("f_" + hex.EncodeToString(digest[:16]))
	}
	return result
}

func acceptanceMetadata(runID review.RunID, batch int, at time.Time) (review.AcceptanceMetadata, error) {
	id, err := randomID(fmt.Sprintf("sub_%d_", batch))
	if err != nil {
		return review.AcceptanceMetadata{}, err
	}
	return review.AcceptanceMetadata{RunID: runID, SubmissionID: review.SubmissionID(id), AcceptedAt: at}, nil
}

func randomID(prefix string) (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}

func dedupeCoverage(items []review.UnitCoverage) []review.UnitCoverage {
	result := make([]review.UnitCoverage, 0, len(items))
	seen := make(map[review.ReviewUnitID]struct{})
	for _, item := range items {
		if _, ok := seen[item.UnitID]; ok {
			continue
		}
		seen[item.UnitID] = struct{}{}
		result = append(result, item)
	}
	return result
}

func mergeTelemetry(left, right review.Telemetry) review.Telemetry {
	left.RequestedModels = appendUnique(left.RequestedModels, right.RequestedModels...)
	models := make(map[string]struct{})
	for _, value := range left.ObservedModels {
		models[value.SessionID+"\x00"+value.Model] = struct{}{}
	}
	for _, value := range right.ObservedModels {
		key := value.SessionID + "\x00" + value.Model
		if _, ok := models[key]; !ok {
			models[key] = struct{}{}
			left.ObservedModels = append(left.ObservedModels, value)
		}
	}
	tools := make(map[string]struct{})
	for _, value := range left.Tools {
		tools[value.SessionID+"\x00"+value.Tool+fmt.Sprint(value.Succeeded)] = struct{}{}
	}
	for _, value := range right.Tools {
		key := value.SessionID + "\x00" + value.Tool + fmt.Sprint(value.Succeeded)
		if _, ok := tools[key]; !ok {
			tools[key] = struct{}{}
			left.Tools = append(left.Tools, value)
		}
	}
	usage := make(map[string]struct{})
	for _, value := range left.Usage {
		usage[value.SessionID+"\x00"+value.EventID] = struct{}{}
	}
	for _, value := range right.Usage {
		key := value.SessionID + "\x00" + value.EventID
		if _, ok := usage[key]; !ok {
			usage[key] = struct{}{}
			left.Usage = append(left.Usage, value)
		}
	}
	left.UsageComplete = left.UsageComplete && right.UsageComplete
	return left
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		seen[v] = struct{}{}
	}
	for _, v := range additions {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			values = append(values, v)
		}
	}
	return values
}
