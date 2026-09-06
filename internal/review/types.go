package review

import (
	"encoding/json"
	"time"
)

type Verdict string

const (
	VerdictApproved    Verdict = "approved"
	VerdictNeedsReview Verdict = "needs_review"
)

type Category string

const (
	CategoryBlocker  Category = "blocker"
	CategoryRisk     Category = "risk"
	CategoryQuestion Category = "question"
	CategoryInfo     Category = "info"
)

type Subcategory string

const (
	SubcategorySecurity    Subcategory = "security"
	SubcategoryCorrections Subcategory = "corrections"
	SubcategoryReliability Subcategory = "reliability"
	SubcategoryMaintenance Subcategory = "maintenance"
	SubcategoryOperational Subcategory = "operational"
	SubcategoryInfo        Subcategory = "info"
)

type Assessment string

const (
	AssessmentPresent       Assessment = "present"
	AssessmentAddressed     Assessment = "addressed"
	AssessmentNotApplicable Assessment = "not_applicable"
	AssessmentUnknown       Assessment = "unknown"
)

type FindingState string

const (
	FindingOpen         FindingState = "open"
	FindingAcknowledged FindingState = "acknowledged"
)

type AcknowledgementMethod string

const (
	AcknowledgementCheckbox     AcknowledgementMethod = "checkbox"
	AcknowledgementAIDiscussion AcknowledgementMethod = "ai_discussion"
	AcknowledgementAICodeChange AcknowledgementMethod = "ai_code_change"
)

type AcknowledgementAction string

const (
	AcknowledgementActionAcknowledge AcknowledgementAction = "acknowledge"
	AcknowledgementActionReopen      AcknowledgementAction = "reopen"
)

type SubmissionCompletion string

const (
	SubmissionComplete SubmissionCompletion = "complete"
	SubmissionPartial  SubmissionCompletion = "partial"
)

type CoverageOutcome string

const (
	CoverageComplete         CoverageOutcome = "complete"
	CoveragePartial          CoverageOutcome = "partial"
	CoverageFailed           CoverageOutcome = "failed"
	CoverageExcludedByPolicy CoverageOutcome = "excluded_by_policy"
)

type Relationship string

const (
	RelationshipIntroduced  Relationship = "introduced"
	RelationshipRegressed   Relationship = "regressed"
	RelationshipPreExisting Relationship = "pre_existing"
	RelationshipUnknown     Relationship = "unknown"
)

type Scope string

const (
	ScopeMRImpact    Scope = "mr_impact"
	ScopeFullProject Scope = "full_project"
)

type FindingID string
type ReviewUnitID string
type SourceReferenceID string
type RunID string
type SubmissionID string
type SnapshotID string
type PublicationGeneration string
type Fingerprint string

type SourceReferenceKind string

const (
	SourceRepositorySource SourceReferenceKind = "repository_source"
	SourceRepositoryDiff   SourceReferenceKind = "repository_diff"
	SourceRepositorySearch SourceReferenceKind = "repository_search"
	SourceRepositoryAST    SourceReferenceKind = "repository_ast"
	SourceGitLabDiscussion SourceReferenceKind = "gitlab_discussion"
	SourceGitLabNote       SourceReferenceKind = "gitlab_note"
)

type Location struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

type Evidence struct {
	Explanation string              `json:"explanation"`
	SourceRefs  []SourceReferenceID `json:"source_refs"`
	Locations   []Location          `json:"locations,omitempty"`
}

type SubmittedFinding struct {
	Category      Category       `json:"category"`
	Subcategory   Subcategory    `json:"subcategory"`
	Relationship  Relationship   `json:"relationship"`
	Title         string         `json:"title"`
	Explanation   string         `json:"explanation"`
	Evidence      []Evidence     `json:"evidence"`
	AssignedUnits []ReviewUnitID `json:"assigned_units"`
}

type Reassessment struct {
	FindingID     FindingID      `json:"finding_id"`
	Assessment    Assessment     `json:"assessment"`
	Explanation   string         `json:"explanation"`
	Evidence      []Evidence     `json:"evidence"`
	AssignedUnits []ReviewUnitID `json:"assigned_units"`
}

type AcknowledgementTransition struct {
	FindingID   FindingID             `json:"finding_id"`
	Action      AcknowledgementAction `json:"action"`
	Method      AcknowledgementMethod `json:"method"`
	Explanation string                `json:"explanation"`
	SourceRefs  []SourceReferenceID   `json:"source_refs"`
}

type UnitCoverage struct {
	UnitID      ReviewUnitID    `json:"unit_id"`
	Outcome     CoverageOutcome `json:"outcome"`
	Explanation string          `json:"explanation,omitempty"`
}

type Submission struct {
	Verdict                Verdict                     `json:"verdict"`
	Completion             SubmissionCompletion        `json:"completion"`
	Findings               []SubmittedFinding          `json:"findings"`
	Reassessments          []Reassessment              `json:"reassessments"`
	AcknowledgementChanges []AcknowledgementTransition `json:"acknowledgement_changes"`
	Coverage               []UnitCoverage              `json:"coverage"`
	Limitations            []string                    `json:"limitations"`
}

type KnownFinding struct {
	ID                 FindingID
	State              FindingState
	Acknowledgement    *Acknowledgement
	ConsumedSourceRefs map[SourceReferenceID]struct{}
}

type Assignment struct {
	UnitIDs          map[ReviewUnitID]struct{}
	UnitRequirements map[ReviewUnitID]UnitRequirement
	Findings         map[FindingID]KnownFinding
	SourceRefs       map[SourceReferenceID]SourceReference
}

// UnitRequirement is host-owned coverage state. The model cannot satisfy a
// retrieval requirement through submit_review arguments alone.
type UnitRequirement struct {
	RetrievalComplete bool
	EnclosingUnitID   ReviewUnitID
}

type SubmissionReceipt struct {
	RunID        RunID        `json:"run_id"`
	SubmissionID SubmissionID `json:"submission_id"`
	Digest       string       `json:"sha256"`
	AcceptedAt   time.Time    `json:"accepted_at"`
}

type AcceptedSubmission struct {
	Receipt SubmissionReceipt `json:"receipt"`
	Value   Submission        `json:"submission"`
	Raw     json.RawMessage   `json:"raw"`
}

type AcceptanceMetadata struct {
	RunID        RunID
	SubmissionID SubmissionID
	AcceptedAt   time.Time
}

type MRIdentity struct {
	BaseURL string `json:"base_url"`
	Project string `json:"project"`
	MRIID   int    `json:"mr_iid"`
}

type Snapshot struct {
	ID         SnapshotID `json:"id"`
	BaseCommit string     `json:"base_commit"`
	HeadCommit string     `json:"head_commit"`
	SourceTree string     `json:"source_tree"`
	CapturedAt time.Time  `json:"captured_at"`
}

type ReviewUnit struct {
	ID          ReviewUnitID `json:"id"`
	Kind        string       `json:"kind"`
	Path        string       `json:"path"`
	Symbol      string       `json:"symbol,omitempty"`
	IdentityKey string       `json:"identity_key"`
}

type SourceReference struct {
	ID        SourceReferenceID   `json:"id"`
	Kind      SourceReferenceKind `json:"kind"`
	GitLabID  int64               `json:"gitlab_id,omitempty"`
	Commit    string              `json:"commit,omitempty"`
	Path      string              `json:"path,omitempty"`
	Author    string              `json:"author,omitempty"`
	Human     bool                `json:"human,omitempty"`
	FindingID FindingID           `json:"finding_id,omitempty"`
	Consumed  bool                `json:"consumed,omitempty"`
}

// SourceRecord persists eligible public evidence that would otherwise be lost
// when a bot-owned predecessor note is retired. Restricted content is never stored.
type SourceRecord struct {
	ID       SourceReferenceID   `json:"id"`
	Kind     SourceReferenceKind `json:"kind"`
	GitLabID int64               `json:"gitlab_id,omitempty"`
	Author   string              `json:"author,omitempty"`
	Human    bool                `json:"human,omitempty"`
	Body     string              `json:"body"`
	At       time.Time           `json:"at,omitempty"`
}

type Acknowledgement struct {
	Method     AcknowledgementMethod `json:"method"`
	SourceRefs []SourceReferenceID   `json:"source_refs"`
	At         time.Time             `json:"at"`
}

type FindingEventKind string

const (
	FindingEventCreated      FindingEventKind = "created"
	FindingEventReassessed   FindingEventKind = "reassessed"
	FindingEventAcknowledged FindingEventKind = "acknowledged"
	FindingEventReopened     FindingEventKind = "reopened"
)

type FindingEvent struct {
	Kind       FindingEventKind      `json:"kind"`
	At         time.Time             `json:"at"`
	RunID      RunID                 `json:"run_id,omitempty"`
	Assessment Assessment            `json:"assessment,omitempty"`
	Method     AcknowledgementMethod `json:"method,omitempty"`
	SourceRefs []SourceReferenceID   `json:"source_refs,omitempty"`
}

type Finding struct {
	ID                   FindingID        `json:"id"`
	IdentityKey          string           `json:"identity_key"`
	Category             Category         `json:"category"`
	Subcategory          Subcategory      `json:"subcategory"`
	Relationship         Relationship     `json:"relationship"`
	Title                string           `json:"title"`
	Explanation          string           `json:"explanation"`
	Evidence             []Evidence       `json:"evidence"`
	AssignedUnits        []ReviewUnitID   `json:"assigned_units"`
	Assessment           Assessment       `json:"assessment"`
	State                FindingState     `json:"state"`
	Acknowledgement      *Acknowledgement `json:"acknowledgement,omitempty"`
	LastRenderedCheckbox *bool            `json:"last_rendered_checkbox,omitempty"`
	History              []FindingEvent   `json:"history"`
	FirstSeenAt          time.Time        `json:"first_seen_at"`
	LastSeenAt           time.Time        `json:"last_seen_at"`
}

type ModelObservation struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
}

type ToolExecution struct {
	SessionID string `json:"session_id"`
	Tool      string `json:"tool"`
	Succeeded bool   `json:"succeeded"`
}

type UsageEvent struct {
	EventID      string `json:"event_id"`
	SessionID    string `json:"session_id"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
	Cumulative   bool   `json:"cumulative"`
}

type Telemetry struct {
	RequestedModels []string           `json:"requested_models"`
	ObservedModels  []ModelObservation `json:"observed_models"`
	Tools           []ToolExecution    `json:"tools"`
	Usage           []UsageEvent       `json:"usage"`
	UsageComplete   bool               `json:"usage_complete"`
}

type Run struct {
	ID          RunID                `json:"id"`
	Fingerprint Fingerprint          `json:"fingerprint"`
	StartedAt   time.Time            `json:"started_at"`
	CompletedAt time.Time            `json:"completed_at,omitempty"`
	Completion  SubmissionCompletion `json:"completion"`
	Verdict     Verdict              `json:"verdict,omitempty"`
	Coverage    []UnitCoverage       `json:"coverage"`
	Telemetry   Telemetry            `json:"telemetry"`
}

type Publication struct {
	Generation        PublicationGeneration `json:"generation"`
	NoteID            int64                 `json:"note_id"`
	PredecessorNoteID int64                 `json:"predecessor_note_id,omitempty"`
	StateDigest       string                `json:"state_digest"`
	PublishedAt       time.Time             `json:"published_at"`
}

type State struct {
	SchemaVersion int            `json:"schema_version"`
	MR            MRIdentity     `json:"mr"`
	Snapshot      Snapshot       `json:"snapshot"`
	Fingerprint   Fingerprint    `json:"fingerprint"`
	Findings      []Finding      `json:"findings"`
	Sources       []SourceRecord `json:"sources,omitempty"`
	Runs          []Run          `json:"runs"`
	Publication   *Publication   `json:"publication,omitempty"`
}
