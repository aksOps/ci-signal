package coordinator

import (
	"context"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/copilot"
	"ci-signal/internal/gitlab"
	"ci-signal/internal/repository"
	"ci-signal/internal/review"
)

type Capture struct {
	Context       gitlab.Context
	Snapshot      repository.Snapshot
	Inventory     repository.Inventory
	Guidance      repository.GuidanceSnapshot
	RelevantFiles map[string][]byte
	SourceRefs    map[review.SourceReferenceID]review.SourceReference
}

type BatchRequest struct {
	Fingerprint review.Fingerprint
	Snapshot    repository.Snapshot
	Guidance    repository.GuidanceSnapshot
	Units       []repository.Unit
	Assignment  review.Assignment
	Prompt      string
	Acceptance  review.AcceptanceMetadata
	Scans       []config.StructuralScan
}

type PublicationRequest struct {
	State      review.State
	Recovery   gitlab.Recovery
	Generation review.PublicationGeneration
	Labels     gitlab.LabelPlan
	Captured   Capture
}

// Hooks are the three external boundaries exercised by coordinator fixtures.
// Production wiring uses the concrete GitLab, repository, Copilot, and publisher packages.
type Hooks struct {
	Capture      func(context.Context, time.Time) (Capture, error)
	Recover      func(context.Context) (gitlab.Recovery, error)
	Review       func(context.Context, BatchRequest) (copilot.Result, error)
	Recheck      func(context.Context, Capture) error
	Publish      func(context.Context, PublicationRequest) (review.State, error)
	HandleStale  func(context.Context, PublicationRequest) error
	RepairLabels func(context.Context, review.State, gitlab.LabelPlan, Capture) error
}

type Result struct {
	State       review.State
	Verdict     review.Verdict
	Completion  review.SubmissionCompletion
	UsedAI      bool
	Published   bool
	Fingerprint review.Fingerprint
}
