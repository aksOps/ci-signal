package gitlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ci-signal/internal/markdown"
	"ci-signal/internal/review"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

const ownedStatePrefix = "<!-- ci-signal-state"

var (
	ErrPublicationConflict = errors.New("GitLab review publication conflicts with current state")
	ErrCleanupBlocked      = errors.New("GitLab review publication cleanup is blocked")
	ErrStalePublication    = errors.New("GitLab review publication is stale")
)

type reportCodec interface {
	Encode(review.State) (string, error)
	Decode(string) (review.State, error)
	ReconcileControls(string, review.State, []markdown.Interaction, time.Time) (review.State, bool, error)
}

type Publisher struct {
	client *Client
	codec  reportCodec
}

func NewPublisher(client *Client, codec *markdown.Codec) (*Publisher, error) {
	if client == nil || codec == nil {
		return nil, errors.New("GitLab client and Markdown codec are required")
	}
	return &Publisher{client: client, codec: codec}, nil
}

type RecoveredReport struct {
	NoteID int64
	Source string
	State  review.State
}

type Recovery struct {
	Current    *RecoveredReport
	Superseded []RecoveredReport
	BotUserID  int64
}

type PublicationExpectation struct {
	NoteID     int64
	Generation review.PublicationGeneration
}

type FreshnessCheck func(context.Context, review.State) error

type PublishRequest struct {
	State                review.State
	Generation           review.PublicationGeneration
	Expected             PublicationExpectation
	PublishedAt          time.Time
	MaxReportBytes       int
	RestrictedSourceRefs map[review.SourceReferenceID]struct{}
	SourceKinds          map[review.SourceReferenceID]review.SourceReferenceKind
	Interactions         []markdown.Interaction
	Freshness            FreshnessCheck
	Labels               *LabelPlan
	CreateMissingLabels  bool
}

type PublishResult struct {
	State       review.State
	NoteID      int64
	ReplacedIDs []int64
	Labels      *LabelSyncResult
}

func (p *Publisher) Recover(ctx context.Context) (Recovery, error) {
	user, _, err := p.client.CurrentUser(ctx)
	if err != nil {
		return Recovery{}, fmt.Errorf("identify GitLab report owner: %w", err)
	}
	if user == nil || user.ID < 1 {
		return Recovery{}, errors.New("GitLab API-token user has no valid identity")
	}
	notes, _, err := p.client.ListNotes(ctx)
	if err != nil {
		return Recovery{}, fmt.Errorf("list GitLab review reports: %w", err)
	}
	reports := make([]RecoveredReport, 0)
	for _, note := range notes {
		if note == nil || note.System || note.Author.ID != user.ID || !strings.Contains(note.Body, ownedStatePrefix) {
			continue
		}
		if note.Internal || note.Confidential {
			return Recovery{}, fmt.Errorf("%w: bot-owned report note %d is restricted", ErrPublicationConflict, note.ID)
		}
		state, err := p.codec.Decode(note.Body)
		if err != nil {
			if errors.Is(err, markdown.ErrNotOwned) {
				continue
			}
			return Recovery{}, fmt.Errorf("%w: decode bot-owned report note %d: %v", ErrPublicationConflict, note.ID, err)
		}
		if err := p.validateRecovered(note, state); err != nil {
			return Recovery{}, err
		}
		reports = append(reports, RecoveredReport{NoteID: note.ID, Source: note.Body, State: state})
	}
	current, superseded, err := selectCurrentReport(reports)
	if err != nil {
		return Recovery{}, err
	}
	return Recovery{Current: current, Superseded: superseded, BotUserID: user.ID}, nil
}

func (p *Publisher) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if err := p.validateRequest(request); err != nil {
		return PublishResult{}, err
	}
	if err := checkFreshness(ctx, request); err != nil {
		return PublishResult{}, err
	}
	recovery, err := p.Recover(ctx)
	if err != nil {
		return PublishResult{}, err
	}
	if err := validateExpectation(recovery.Current, request.Expected, request.Generation); err != nil {
		return PublishResult{}, err
	}

	state, successor, created, err := p.prepareSuccessor(ctx, request, recovery)
	if err != nil {
		return PublishResult{}, err
	}
	if err := checkFreshness(ctx, PublishRequest{State: state, Freshness: request.Freshness}); err != nil {
		if created {
			if cleanupErr := p.removeUnpublishedSuccessor(ctx, successor.NoteID, recovery.BotUserID); cleanupErr != nil {
				return PublishResult{}, errors.Join(err, cleanupErr)
			}
		}
		return PublishResult{}, err
	}

	var reconciledPredecessorSource string
	state, reconciledPredecessorSource, err = p.reconcileLateControls(ctx, request, recovery, successor, state)
	if err != nil {
		return PublishResult{}, err
	}
	deleteCandidates := reportsToRetire(recovery, successor.NoteID)
	expectedSources := make(map[int64]string, len(deleteCandidates))
	for _, candidate := range deleteCandidates {
		expectedSources[candidate.NoteID] = candidate.Source
	}
	if predecessorID := state.Publication.PredecessorNoteID; predecessorID > 0 && reconciledPredecessorSource != "" {
		expectedSources[predecessorID] = reconciledPredecessorSource
	}
	if err := p.ensureCleanupSafe(ctx, deleteCandidates, recovery.BotUserID, expectedSources, &state, request.SourceKinds); err != nil {
		return PublishResult{}, err
	}
	if err := checkFreshness(ctx, PublishRequest{State: state, Freshness: request.Freshness}); err != nil {
		return PublishResult{}, err
	}
	replaced, err := p.deleteSuperseded(ctx, deleteCandidates, successor.NoteID)
	if err != nil {
		return PublishResult{}, err
	}
	if err := p.verifyOnlyCurrent(ctx, successor.NoteID, request.Generation); err != nil {
		return PublishResult{}, err
	}

	result := PublishResult{State: state, NoteID: successor.NoteID, ReplacedIDs: replaced}
	if request.Labels != nil {
		labels, err := p.SyncPublishedLabels(ctx, PublicationExpectation{NoteID: successor.NoteID, Generation: request.Generation}, state, *request.Labels, request.CreateMissingLabels, request.Freshness)
		if err != nil {
			return result, err
		}
		result.Labels = &labels
	}
	return result, nil
}

func (p *Publisher) SyncPublishedLabels(ctx context.Context, expected PublicationExpectation, state review.State, plan LabelPlan, createMissing bool, freshness FreshnessCheck) (LabelSyncResult, error) {
	if freshness == nil {
		return LabelSyncResult{}, errors.New("publication freshness check is required")
	}
	if err := p.verifyCurrentExpectation(ctx, expected); err != nil {
		return LabelSyncResult{}, err
	}
	if err := freshness(ctx, state); err != nil {
		staleErr := fmt.Errorf("%w: %v", ErrStalePublication, err)
		return LabelSyncResult{}, errors.Join(staleErr, p.clearStaleManagedLabels(ctx, plan))
	}
	mergeRequest, _, err := p.client.GetMergeRequest(ctx)
	if err != nil {
		return LabelSyncResult{}, fmt.Errorf("read labels for publication generation: %w", err)
	}
	result, err := p.client.SyncLabels(ctx, mergeRequest.Labels, plan, createMissing)
	if err != nil {
		return LabelSyncResult{}, err
	}
	if err := freshness(ctx, state); err != nil {
		staleErr := fmt.Errorf("%w after label update: %v", ErrStalePublication, err)
		if generationErr := p.verifyCurrentExpectation(ctx, expected); generationErr != nil {
			return LabelSyncResult{}, errors.Join(staleErr, generationErr)
		}
		return LabelSyncResult{}, errors.Join(staleErr, p.clearStaleManagedLabels(ctx, plan))
	}
	if err := p.verifyCurrentExpectation(ctx, expected); err != nil {
		return LabelSyncResult{}, err
	}
	return result, nil
}

func (p *Publisher) clearStaleManagedLabels(ctx context.Context, plan LabelPlan) error {
	mergeRequest, _, err := p.client.GetMergeRequest(ctx)
	if err != nil {
		return fmt.Errorf("clear stale managed labels: %w", err)
	}
	plan.Desired = nil
	if _, err := p.client.SyncLabels(ctx, mergeRequest.Labels, plan, false); err != nil {
		return fmt.Errorf("clear stale managed labels: %w", err)
	}
	return nil
}

func (p *Publisher) validateRequest(request PublishRequest) error {
	if strings.TrimSpace(string(request.Generation)) == "" || strings.ContainsAny(string(request.Generation), "\r\n") {
		return errors.New("publication generation is required")
	}
	if request.PublishedAt.IsZero() || request.MaxReportBytes < 1 || request.Freshness == nil {
		return errors.New("publication timestamp, positive report limit, and freshness check are required")
	}
	if !sameMR(request.State.MR, p.client.TargetIdentity()) {
		return fmt.Errorf("%w: requested state is bound to another merge request", ErrPublicationConflict)
	}
	if request.Expected.NoteID < 0 || (request.Expected.NoteID == 0) != (request.Expected.Generation == "") {
		return errors.New("publication expectation requires both note ID and generation, or neither")
	}
	if err := validateVisibleSources(request.State, request.Interactions, request.RestrictedSourceRefs); err != nil {
		return err
	}
	return nil
}

func (p *Publisher) validateRecovered(note *gitlabapi.Note, state review.State) error {
	if note.ID < 1 || (note.NoteableIID != 0 && note.NoteableIID != p.client.mrIID) {
		return fmt.Errorf("%w: report note is bound to another merge request", ErrPublicationConflict)
	}
	if !sameMR(state.MR, p.client.TargetIdentity()) {
		return fmt.Errorf("%w: report note %d has a foreign state binding", ErrPublicationConflict, note.ID)
	}
	publication := state.Publication
	if publication == nil || publication.Generation == "" || publication.StateDigest == "" || publication.PublishedAt.IsZero() {
		return fmt.Errorf("%w: report note %d lacks publication metadata", ErrPublicationConflict, note.ID)
	}
	if publication.NoteID != 0 && publication.NoteID != note.ID {
		return fmt.Errorf("%w: report note %d claims note %d", ErrPublicationConflict, note.ID, publication.NoteID)
	}
	if err := validateStateDigest(p.codec, state); err != nil {
		return fmt.Errorf("%w: report note %d has invalid publication digest: %v", ErrPublicationConflict, note.ID, err)
	}
	return nil
}

func (p *Publisher) prepareSuccessor(ctx context.Context, request PublishRequest, recovery Recovery) (review.State, RecoveredReport, bool, error) {
	predecessorID := int64(0)
	if recovery.Current != nil {
		predecessorID = recovery.Current.NoteID
		if recovery.Current.State.Publication.Generation == request.Generation {
			return p.resumeSuccessor(ctx, request, recovery)
		}
	}
	state := cloneReviewState(request.State)
	state.Publication = &review.Publication{
		Generation:        request.Generation,
		PredecessorNoteID: predecessorID,
		PublishedAt:       request.PublishedAt.UTC(),
	}
	if recovery.Current != nil {
		var err error
		state, _, err = p.codec.ReconcileControls(recovery.Current.Source, state, request.Interactions, request.PublishedAt)
		if err != nil {
			return review.State{}, RecoveredReport{}, false, fmt.Errorf("reconcile predecessor controls: %w", err)
		}
	}
	digest, err := calculateStateDigest(p.codec, state)
	if err != nil {
		return review.State{}, RecoveredReport{}, false, fmt.Errorf("digest successor report: %w", err)
	}
	state.Publication.StateDigest = digest
	if err := p.checkReportSize(state, request.MaxReportBytes); err != nil {
		return review.State{}, RecoveredReport{}, false, err
	}
	body, err := p.codec.Encode(state)
	if err != nil {
		return review.State{}, RecoveredReport{}, false, fmt.Errorf("encode successor report: %w", err)
	}
	if err := checkFreshness(ctx, request); err != nil {
		return review.State{}, RecoveredReport{}, false, err
	}
	latest, err := p.Recover(ctx)
	if err != nil {
		return review.State{}, RecoveredReport{}, false, err
	}
	if !sameRecovered(latest.Current, recovery.Current) {
		return review.State{}, RecoveredReport{}, false, fmt.Errorf("%w: report predecessor changed before create", ErrStalePublication)
	}
	note, createErr := p.client.CreateNote(ctx, body)
	if createErr != nil {
		candidate, recoverErr := p.findGeneration(ctx, request.Generation, predecessorID, state.Publication.StateDigest)
		if recoverErr != nil || candidate == nil {
			return review.State{}, RecoveredReport{}, false, createErr
		}
		note = &gitlabapi.Note{ID: candidate.NoteID, Body: candidate.Source}
	}
	if note == nil || note.ID < 1 {
		return review.State{}, RecoveredReport{}, false, errors.New("GitLab created a report without a note ID")
	}
	state.Publication.NoteID = note.ID
	finalBody, err := p.codec.Encode(state)
	if err != nil {
		return review.State{}, RecoveredReport{}, true, fmt.Errorf("encode finalized successor report: %w", err)
	}
	if len(finalBody) > request.MaxReportBytes {
		return review.State{}, RecoveredReport{}, true, errors.New("final review report exceeds configured size limit")
	}
	if note.Body != finalBody {
		if _, err := p.client.UpdateNote(ctx, note.ID, finalBody); err != nil {
			return review.State{}, RecoveredReport{}, true, err
		}
	}
	verified, err := p.verifyReport(ctx, note.ID, recovery.BotUserID, state, finalBody)
	if err != nil {
		return review.State{}, RecoveredReport{}, true, err
	}
	return state, verified, true, nil
}

func (p *Publisher) resumeSuccessor(ctx context.Context, request PublishRequest, recovery Recovery) (review.State, RecoveredReport, bool, error) {
	current := *recovery.Current
	state := cloneReviewState(request.State)
	state.Publication = &review.Publication{
		Generation:        request.Generation,
		NoteID:            current.NoteID,
		PredecessorNoteID: current.State.Publication.PredecessorNoteID,
		PublishedAt:       current.State.Publication.PublishedAt,
	}
	digest, err := calculateStateDigest(p.codec, state)
	if err != nil {
		return review.State{}, RecoveredReport{}, false, fmt.Errorf("digest resumed report: %w", err)
	}
	state.Publication.StateDigest = digest
	currentDigest, err := calculateStateDigest(p.codec, current.State)
	if err != nil || state.Publication.StateDigest != currentDigest {
		return review.State{}, RecoveredReport{}, false, fmt.Errorf("%w: generation %q has different review state", ErrPublicationConflict, request.Generation)
	}
	body, err := p.codec.Encode(state)
	if err != nil {
		return review.State{}, RecoveredReport{}, false, err
	}
	if len(body) > request.MaxReportBytes {
		return review.State{}, RecoveredReport{}, false, errors.New("review report exceeds configured size limit")
	}
	if current.Source != body || current.State.Publication.NoteID == 0 {
		if _, err := p.client.UpdateNote(ctx, current.NoteID, body); err != nil {
			return review.State{}, RecoveredReport{}, false, err
		}
	}
	verified, err := p.verifyReport(ctx, current.NoteID, recovery.BotUserID, state, body)
	return state, verified, false, err
}

func (p *Publisher) reconcileLateControls(ctx context.Context, request PublishRequest, recovery Recovery, successor RecoveredReport, state review.State) (review.State, string, error) {
	predecessorID := state.Publication.PredecessorNoteID
	if predecessorID == 0 {
		return state, "", nil
	}
	predecessor, _, err := p.client.GetNote(ctx, predecessorID)
	if err != nil {
		return review.State{}, "", fmt.Errorf("reread retiring report: %w", err)
	}
	prior := reportByID(recovery, predecessorID)
	if prior == nil || predecessor == nil || predecessor.Author.ID != recovery.BotUserID || predecessor.Internal || predecessor.Confidential {
		return review.State{}, "", fmt.Errorf("%w: retiring report ownership changed", ErrPublicationConflict)
	}
	decoded, err := p.codec.Decode(predecessor.Body)
	if err != nil || decoded.Publication == nil || (decoded.Publication.NoteID != 0 && decoded.Publication.NoteID != predecessorID) {
		return review.State{}, "", fmt.Errorf("%w: retiring report state changed invalidly", ErrPublicationConflict)
	}
	state, changed, err := p.codec.ReconcileControls(predecessor.Body, state, request.Interactions, request.PublishedAt)
	if err != nil {
		return review.State{}, "", fmt.Errorf("reconcile late report controls: %w", err)
	}
	if !changed {
		return state, predecessor.Body, nil
	}
	digest, err := calculateStateDigest(p.codec, state)
	if err != nil {
		return review.State{}, "", fmt.Errorf("digest late control state: %w", err)
	}
	state.Publication.StateDigest = digest
	body, err := p.codec.Encode(state)
	if err != nil {
		return review.State{}, "", err
	}
	if len(body) > request.MaxReportBytes {
		return review.State{}, "", errors.New("late control reconciliation exceeds configured report size limit")
	}
	if _, err := p.client.UpdateNote(ctx, successor.NoteID, body); err != nil {
		return review.State{}, "", err
	}
	if _, err := p.verifyReport(ctx, successor.NoteID, recovery.BotUserID, state, body); err != nil {
		return review.State{}, "", err
	}
	return state, predecessor.Body, nil
}

func (p *Publisher) checkReportSize(state review.State, maximum int) error {
	probe := cloneReviewState(state)
	probe.Publication.NoteID = math.MaxInt64
	body, err := p.codec.Encode(probe)
	if err != nil {
		return fmt.Errorf("encode report size probe: %w", err)
	}
	if len(body) > maximum {
		return fmt.Errorf("review report is %d bytes and exceeds configured %d-byte limit", len(body), maximum)
	}
	return nil
}

func (p *Publisher) verifyReport(ctx context.Context, noteID, botID int64, expected review.State, body string) (RecoveredReport, error) {
	note, _, err := p.client.GetNote(ctx, noteID)
	if err != nil {
		return RecoveredReport{}, fmt.Errorf("verify successor report: %w", err)
	}
	if note == nil || note.Author.ID != botID || note.System || note.Internal || note.Confidential || !sameGitLabNoteBody(note.Body, body) {
		return RecoveredReport{}, fmt.Errorf("%w: successor report readback differs", ErrPublicationConflict)
	}
	state, err := p.codec.Decode(note.Body)
	if err != nil {
		return RecoveredReport{}, fmt.Errorf("%w: decode successor report: %v", ErrPublicationConflict, err)
	}
	if state.Publication == nil {
		return RecoveredReport{}, fmt.Errorf("%w: successor lacks publication metadata", ErrPublicationConflict)
	}
	digest, digestErr := calculateStateDigest(p.codec, state)
	if digestErr != nil || digest != state.Publication.StateDigest || state.Publication.NoteID != noteID || expected.Publication == nil || state.Publication.Generation != expected.Publication.Generation || !sameMR(state.MR, expected.MR) {
		return RecoveredReport{}, fmt.Errorf("%w: successor publication metadata differs", ErrPublicationConflict)
	}
	return RecoveredReport{NoteID: noteID, Source: note.Body, State: state}, nil
}

func sameGitLabNoteBody(actual, expected string) bool {
	if actual == expected {
		return true
	}
	// GitLab note writes may remove the renderer's one terminal line feed.
	// Keep every other byte under the existing strict readback check.
	return strings.HasSuffix(expected, "\n") && actual == strings.TrimSuffix(expected, "\n")
}

func (p *Publisher) findGeneration(ctx context.Context, generation review.PublicationGeneration, predecessorID int64, digest string) (*RecoveredReport, error) {
	recovery, err := p.Recover(ctx)
	if err != nil {
		return nil, err
	}
	all := append([]RecoveredReport(nil), recovery.Superseded...)
	if recovery.Current != nil {
		all = append(all, *recovery.Current)
	}
	var candidates []RecoveredReport
	for _, report := range all {
		publication := report.State.Publication
		if publication.Generation == generation && publication.PredecessorNoteID == predecessorID && publication.StateDigest == digest {
			candidates = append(candidates, report)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].NoteID > candidates[j].NoteID })
	return &candidates[0], nil
}

func (p *Publisher) ensureCleanupSafe(ctx context.Context, candidates []RecoveredReport, botID int64, expectedSources map[int64]string, retainedState *review.State, sourceKinds map[review.SourceReferenceID]review.SourceReferenceKind) error {
	if len(candidates) == 0 {
		return nil
	}
	discussions, _, err := p.client.ListDiscussions(ctx)
	if err != nil {
		return fmt.Errorf("inspect retiring report discussions: %w", err)
	}
	for _, candidate := range candidates {
		note, _, err := p.client.GetNote(ctx, candidate.NoteID)
		if err != nil {
			return fmt.Errorf("reread retiring report %d: %w", candidate.NoteID, err)
		}
		if note == nil || note.Author.ID != botID || note.System || note.Internal || note.Confidential {
			return fmt.Errorf("%w: retiring note %d is no longer an owned public report", ErrCleanupBlocked, candidate.NoteID)
		}
		if expected, ok := expectedSources[candidate.NoteID]; ok && note.Body != expected {
			return fmt.Errorf("%w: retiring note %d changed after control reconciliation", ErrCleanupBlocked, candidate.NoteID)
		}
		state, err := p.codec.Decode(note.Body)
		if err != nil || state.Publication == nil || (state.Publication.NoteID != 0 && state.Publication.NoteID != candidate.NoteID) || !sameMR(state.MR, p.client.TargetIdentity()) {
			return fmt.Errorf("%w: retiring note %d no longer has verified state", ErrCleanupBlocked, candidate.NoteID)
		}
		if retainedState != nil {
			if err := validateRetainedSourceRecords(state, *retainedState, sourceKinds); err != nil {
				return fmt.Errorf("%w: retiring note %d: %v", ErrCleanupBlocked, candidate.NoteID, err)
			}
		}
		if discussionHasReply(discussions, candidate.NoteID) {
			return fmt.Errorf("%w: retiring note %d has replies", ErrCleanupBlocked, candidate.NoteID)
		}
	}
	return nil
}

// deleteSuperseded follows a final source-body and reply check. GitLab 18.9
// exposes no conditional note delete, so an edit can still race that DELETE;
// the next recovery run detects any resulting duplicate or chain conflict.
func (p *Publisher) deleteSuperseded(ctx context.Context, candidates []RecoveredReport, successorID int64) ([]int64, error) {
	deleted := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.NoteID == successorID {
			continue
		}
		if err := p.client.DeleteNote(ctx, candidate.NoteID); err != nil {
			notes, _, readErr := p.client.ListNotes(ctx)
			if readErr != nil || noteExists(notes, candidate.NoteID) {
				return deleted, err
			}
		}
		deleted = append(deleted, candidate.NoteID)
	}
	return deleted, nil
}

func (p *Publisher) verifyOnlyCurrent(ctx context.Context, noteID int64, generation review.PublicationGeneration) error {
	recovery, err := p.Recover(ctx)
	if err != nil {
		return err
	}
	if recovery.Current == nil || recovery.Current.NoteID != noteID || recovery.Current.State.Publication.Generation != generation || len(recovery.Superseded) != 0 {
		return fmt.Errorf("%w: report replacement did not converge to one current generation", ErrPublicationConflict)
	}
	return nil
}

func (p *Publisher) verifyCurrentExpectation(ctx context.Context, expected PublicationExpectation) error {
	recovery, err := p.Recover(ctx)
	if err != nil {
		return err
	}
	if recovery.Current == nil || recovery.Current.NoteID != expected.NoteID || recovery.Current.State.Publication.Generation != expected.Generation {
		return fmt.Errorf("%w: published report generation changed", ErrStalePublication)
	}
	return nil
}

func (p *Publisher) removeUnpublishedSuccessor(ctx context.Context, noteID, botID int64) error {
	recovery := Recovery{BotUserID: botID, Current: &RecoveredReport{NoteID: noteID}}
	candidates := []RecoveredReport{{NoteID: noteID}}
	if err := p.ensureCleanupSafe(ctx, candidates, botID, nil, nil, nil); err != nil {
		return err
	}
	_, err := p.deleteSuperseded(ctx, reportsToRetire(recovery, 0), 0)
	return err
}

func selectCurrentReport(reports []RecoveredReport) (*RecoveredReport, []RecoveredReport, error) {
	if len(reports) == 0 {
		return nil, nil, nil
	}
	referenced := make(map[int64]struct{}, len(reports))
	for _, report := range reports {
		if predecessor := report.State.Publication.PredecessorNoteID; predecessor > 0 {
			referenced[predecessor] = struct{}{}
		}
	}
	var heads []RecoveredReport
	for _, report := range reports {
		if _, superseded := referenced[report.NoteID]; !superseded {
			heads = append(heads, report)
		}
	}
	if len(heads) == 0 {
		return nil, nil, fmt.Errorf("%w: report publication chain has no current generation", ErrPublicationConflict)
	}
	if len(heads) > 1 {
		first := heads[0].State.Publication
		for _, head := range heads[1:] {
			publication := head.State.Publication
			if publication.Generation != first.Generation || publication.PredecessorNoteID != first.PredecessorNoteID || publication.StateDigest != first.StateDigest {
				return nil, nil, fmt.Errorf("%w: multiple unrelated current report generations", ErrPublicationConflict)
			}
		}
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].NoteID > heads[j].NoteID })
	current := heads[0]
	superseded := make([]RecoveredReport, 0, len(reports)-1)
	for _, report := range reports {
		if report.NoteID != current.NoteID {
			superseded = append(superseded, report)
		}
	}
	sort.Slice(superseded, func(i, j int) bool { return superseded[i].NoteID < superseded[j].NoteID })
	return &current, superseded, nil
}

func validateExpectation(current *RecoveredReport, expected PublicationExpectation, target review.PublicationGeneration) error {
	if current == nil {
		if expected.NoteID != 0 {
			return fmt.Errorf("%w: expected predecessor is missing", ErrStalePublication)
		}
		return nil
	}
	publication := current.State.Publication
	if current.NoteID == expected.NoteID && publication.Generation == expected.Generation {
		return nil
	}
	if publication.Generation == target && publication.PredecessorNoteID == expected.NoteID {
		return nil
	}
	return fmt.Errorf("%w: current report differs from expected predecessor", ErrStalePublication)
}

func validateVisibleSources(state review.State, interactions []markdown.Interaction, restricted map[review.SourceReferenceID]struct{}) error {
	if len(restricted) == 0 {
		return nil
	}
	check := func(ref review.SourceReferenceID) error {
		if _, denied := restricted[ref]; denied {
			return fmt.Errorf("restricted source reference %q cannot enter a public report", ref)
		}
		return nil
	}
	for _, finding := range state.Findings {
		for _, evidence := range finding.Evidence {
			for _, ref := range evidence.SourceRefs {
				if err := check(ref); err != nil {
					return err
				}
			}
		}
		if finding.Acknowledgement != nil {
			for _, ref := range finding.Acknowledgement.SourceRefs {
				if err := check(ref); err != nil {
					return err
				}
			}
		}
		for _, event := range finding.History {
			for _, ref := range event.SourceRefs {
				if err := check(ref); err != nil {
					return err
				}
			}
		}
	}
	for _, source := range state.Sources {
		if err := check(source.ID); err != nil {
			return err
		}
	}
	for _, interaction := range interactions {
		if err := check(interaction.SourceRefID); err != nil {
			return err
		}
	}
	return nil
}

func validateRetainedSourceRecords(previous, successor review.State, sourceKinds map[review.SourceReferenceID]review.SourceReferenceKind) error {
	previousRecords := make(map[review.SourceReferenceID]review.SourceRecord, len(previous.Sources))
	for _, source := range previous.Sources {
		previousRecords[source.ID] = source
	}
	successorRecords := make(map[review.SourceReferenceID]review.SourceRecord, len(successor.Sources))
	for _, source := range successor.Sources {
		successorRecords[source.ID] = source
	}
	for ref := range findingSourceReferences(previous.Findings) {
		prior, durable := previousRecords[ref]
		if !durable {
			switch sourceKinds[ref] {
			case review.SourceRepositorySource, review.SourceRepositoryDiff, review.SourceRepositorySearch, review.SourceRepositoryAST:
				continue
			case review.SourceGitLabNote, review.SourceGitLabDiscussion:
				return fmt.Errorf("GitLab source reference %q lacks a durable predecessor record", ref)
			default:
				return fmt.Errorf("source reference %q has unknown provenance and cannot be discarded safely", ref)
			}
		}
		current, retained := successorRecords[ref]
		if !retained {
			return fmt.Errorf("durable source record %q was not retained", ref)
		}
		if current != prior {
			return fmt.Errorf("durable source record %q changed text or attribution", ref)
		}
	}
	return nil
}

func findingSourceReferences(findings []review.Finding) map[review.SourceReferenceID]struct{} {
	result := make(map[review.SourceReferenceID]struct{})
	for _, finding := range findings {
		for _, evidence := range finding.Evidence {
			for _, ref := range evidence.SourceRefs {
				result[ref] = struct{}{}
			}
		}
		if finding.Acknowledgement != nil {
			for _, ref := range finding.Acknowledgement.SourceRefs {
				result[ref] = struct{}{}
			}
		}
		for _, event := range finding.History {
			for _, ref := range event.SourceRefs {
				result[ref] = struct{}{}
			}
		}
	}
	return result
}

const stateDigestPrefix = "state-v1:"

func publicationDigestState(state review.State) review.State {
	copy := markdown.Canonicalize(state)
	if copy.Publication != nil {
		copy.Publication.NoteID = 0
		copy.Publication.StateDigest = ""
		copy.Publication.PublishedAt = time.Time{}
	}
	return copy
}

func calculateStateDigest(codec reportCodec, state review.State) (string, error) {
	copy := publicationDigestState(state)
	// Keep codec validation, but exclude presentation from the digest.
	if _, err := codec.Encode(copy); err != nil {
		return "", err
	}
	payload, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return stateDigestPrefix + hex.EncodeToString(digest[:]), nil
}

func validateStateDigest(codec reportCodec, state review.State) error {
	stored := state.Publication.StateDigest
	if strings.HasPrefix(stored, stateDigestPrefix) {
		digest, err := calculateStateDigest(codec, state)
		if err != nil {
			return err
		}
		if digest != stored {
			return errors.New("canonical publication digest mismatch")
		}
		return nil
	}
	// Unprefixed SHA-256 is the historical rendered-report format. Verify the
	// exact original renderer, never substitute the current layout or skip it.
	if raw, err := hex.DecodeString(stored); err != nil || len(raw) != sha256.Size {
		return errors.New("unsupported publication digest version")
	}
	payload, err := markdown.NewCodec().EncodeLegacyPublication(publicationDigestState(state))
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(payload))
	if hex.EncodeToString(digest[:]) != stored {
		return errors.New("historical publication digest mismatch")
	}
	return nil
}

func cloneReviewState(state review.State) review.State {
	payload, _ := json.Marshal(state)
	var clone review.State
	_ = json.Unmarshal(payload, &clone)
	return clone
}

func sameMR(left, right review.MRIdentity) bool {
	return strings.TrimSuffix(left.BaseURL, "/") == strings.TrimSuffix(right.BaseURL, "/") && left.Project == right.Project && left.MRIID == right.MRIID
}

func checkFreshness(ctx context.Context, request PublishRequest) error {
	if request.Freshness == nil {
		return errors.New("publication freshness check is required")
	}
	if err := request.Freshness(ctx, request.State); err != nil {
		return fmt.Errorf("%w: %v", ErrStalePublication, err)
	}
	return nil
}

func sameRecovered(left, right *RecoveredReport) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.NoteID == right.NoteID && left.State.Publication.Generation == right.State.Publication.Generation && left.State.Publication.StateDigest == right.State.Publication.StateDigest
}

func reportByID(recovery Recovery, noteID int64) *RecoveredReport {
	if recovery.Current != nil && recovery.Current.NoteID == noteID {
		return recovery.Current
	}
	for i := range recovery.Superseded {
		if recovery.Superseded[i].NoteID == noteID {
			return &recovery.Superseded[i]
		}
	}
	return nil
}

func reportsToRetire(recovery Recovery, successorID int64) []RecoveredReport {
	result := append([]RecoveredReport(nil), recovery.Superseded...)
	if recovery.Current != nil && recovery.Current.NoteID != successorID {
		result = append(result, *recovery.Current)
	}
	seen := make(map[int64]struct{}, len(result))
	unique := result[:0]
	for _, report := range result {
		if report.NoteID < 1 || report.NoteID == successorID {
			continue
		}
		if _, exists := seen[report.NoteID]; !exists {
			seen[report.NoteID] = struct{}{}
			unique = append(unique, report)
		}
	}
	return unique
}

func discussionHasReply(discussions []*gitlabapi.Discussion, noteID int64) bool {
	for _, discussion := range discussions {
		if discussion == nil || len(discussion.Notes) == 0 {
			continue
		}
		contains := false
		for _, note := range discussion.Notes {
			if note != nil && note.ID == noteID {
				contains = true
				break
			}
		}
		if contains && len(discussion.Notes) > 1 {
			return true
		}
	}
	return false
}

func noteExists(notes []*gitlabapi.Note, noteID int64) bool {
	for _, note := range notes {
		if note != nil && note.ID == noteID {
			return true
		}
	}
	return false
}
