package repository

import (
	"errors"
	"fmt"
	"path"
	"time"

	"ci-signal/internal/review"
)

var (
	ErrMissingHistory    = errors.New("required Git history is missing")
	ErrOutputLimit       = errors.New("command output exceeded its configured limit")
	ErrUnsupportedObject = errors.New("unsupported Git object")
	ErrStaleSnapshot     = errors.New("snapshot does not belong to this analyzer")
)

type Side string

const (
	SideBase Side = "base"
	SideHead Side = "head"
)

type Snapshot struct {
	ID                     review.SnapshotID `json:"id"`
	RepositoryRoot         string            `json:"repository_root"`
	BaseCommit             string            `json:"base_commit"`
	HeadCommit             string            `json:"head_commit"`
	CheckedOutCommit       string            `json:"checked_out_commit"`
	CheckedOutIsSourceHead bool              `json:"checked_out_is_source_head"`
	ObjectFormat           string            `json:"object_format"`
	Shallow                bool              `json:"shallow"`
	CapturedAt             time.Time         `json:"captured_at"`
}

type ChangeKind string

const (
	ChangeAdded     ChangeKind = "added"
	ChangeModified  ChangeKind = "modified"
	ChangeDeleted   ChangeKind = "deleted"
	ChangeRenamed   ChangeKind = "renamed"
	ChangeType      ChangeKind = "type_changed"
	ChangeUnchanged ChangeKind = "unchanged"
)

type FileChange struct {
	Kind       ChangeKind `json:"kind"`
	BasePath   string     `json:"base_path,omitempty"`
	HeadPath   string     `json:"head_path,omitempty"`
	Similarity int        `json:"similarity,omitempty"`
	Binary     bool       `json:"binary"`
}

type UnitKind string

const (
	UnitDeclaration UnitKind = "declaration"
	UnitFile        UnitKind = "file"
	UnitPackage     UnitKind = "package"
)

type Span struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type Unit struct {
	ID             review.ReviewUnitID `json:"id"`
	Kind           UnitKind            `json:"kind"`
	Language       string              `json:"language,omitempty"`
	Symbol         string              `json:"symbol,omitempty"`
	Base           *Span               `json:"base,omitempty"`
	Head           *Span               `json:"head,omitempty"`
	Change         ChangeKind          `json:"change"`
	Fallback       bool                `json:"fallback"`
	Oversized      bool                `json:"oversized"`
	NeedsEnclosing bool                `json:"needs_enclosing_assessment"`
}

type Declaration struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Bytes     int    `json:"bytes"`
}

type ExclusionReason string

const (
	ExclusionVendor               ExclusionReason = "vendor"
	ExclusionGenerated            ExclusionReason = "generated"
	ExclusionBinary               ExclusionReason = "binary"
	ExclusionUnsupportedStructure ExclusionReason = "unsupported_structure"
	ExclusionExtractionFailed     ExclusionReason = "extraction_failed"
	ExclusionSymlink              ExclusionReason = "symlink"
	ExclusionSubmodule            ExclusionReason = "submodule"
)

type Exclusion struct {
	Path           string              `json:"path"`
	Reason         ExclusionReason     `json:"reason"`
	Detail         string              `json:"detail,omitempty"`
	Excluded       bool                `json:"excluded"`
	FallbackUnitID review.ReviewUnitID `json:"fallback_unit_id,omitempty"`
}

type Inventory struct {
	SnapshotID review.SnapshotID `json:"snapshot_id"`
	Scope      review.Scope      `json:"scope"`
	Changes    []FileChange      `json:"changes"`
	Units      []Unit            `json:"units"`
	Exclusions []Exclusion       `json:"exclusions"`
}

// UnitRequirements converts host-known retrieval limits into submit_review
// coverage requirements. Citation sources are registered independently.
func (i Inventory) UnitRequirements() (map[review.ReviewUnitID]review.UnitRequirement, error) {
	packages := make(map[string]review.ReviewUnitID)
	for _, unit := range i.Units {
		if unit.Kind == UnitPackage {
			packages[unit.Symbol] = unit.ID
		}
	}
	result := make(map[review.ReviewUnitID]review.UnitRequirement)
	for _, unit := range i.Units {
		if !unit.Oversized && !unit.NeedsEnclosing {
			continue
		}
		requirement := review.UnitRequirement{RetrievalComplete: !unit.Oversized}
		if unit.NeedsEnclosing {
			unitPath := ""
			if unit.Head != nil {
				unitPath = unit.Head.Path
			} else if unit.Base != nil {
				unitPath = unit.Base.Path
			}
			enclosingID, ok := packages[path.Dir(unitPath)]
			if !ok {
				return nil, fmt.Errorf("unit %q needs an enclosing package assessment for %q", unit.ID, unitPath)
			}
			requirement.EnclosingUnitID = enclosingID
		}
		result[unit.ID] = requirement
	}
	return result, nil
}

type Chunk struct {
	Data       []byte `json:"data"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset,omitempty"`
	Complete   bool   `json:"complete"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
}

type EvidenceChunk struct {
	Chunk     Chunk                  `json:"chunk"`
	Reference review.SourceReference `json:"source_reference"`
}

type DeclarationEvidence struct {
	Declarations []Declaration          `json:"declarations"`
	Reference    review.SourceReference `json:"source_reference"`
}

type GuidanceSnapshot struct {
	RuntimeDirectory       string   `json:"runtime_directory"`
	InstructionDirectories []string `json:"instruction_directories"`
	SkillDirectories       []string `json:"skill_directories"`
	InstructionFiles       []string `json:"instruction_files"`
	Files                  []string `json:"files"`
}
