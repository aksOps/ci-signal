package copilot

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"ci-signal/internal/review"
)

// AssignmentRegistry owns the validation boundary shared by repository tools and
// submit_review. Repository tools register only the source references they actually
// returned to the model; Acceptor receives an immutable snapshot at submission time.
type AssignmentRegistry struct {
	mu               sync.RWMutex
	assignment       review.Assignment
	retrievalOffsets map[review.ReviewUnitID]int64
}

func NewAssignmentRegistry(assignment review.Assignment) (*AssignmentRegistry, error) {
	if assignment.UnitIDs == nil || assignment.Findings == nil || assignment.SourceRefs == nil {
		return nil, errors.New("assignment units, findings, and source references must be initialized")
	}
	return &AssignmentRegistry{assignment: copyAssignment(assignment), retrievalOffsets: make(map[review.ReviewUnitID]int64)}, nil
}

func (r *AssignmentRegistry) RegisterSource(reference review.SourceReference) error {
	if r == nil {
		return errors.New("assignment registry is required")
	}
	if reference.ID == "" {
		return errors.New("source reference ID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.assignment.SourceRefs[reference.ID]; ok {
		if reflect.DeepEqual(existing, reference) {
			return nil
		}
		return fmt.Errorf("source reference %q already exists with different content", reference.ID)
	}
	r.assignment.SourceRefs[reference.ID] = reference
	return nil
}

// RecordUnitRetrieval records a sequential bounded source read. Completion is
// host-owned and becomes true only after continuations starting at offset zero.
func (r *AssignmentRegistry) RecordUnitRetrieval(unitID review.ReviewUnitID, offset, nextOffset int64, complete bool) error {
	if r == nil {
		return errors.New("assignment registry is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	requirement, ok := r.assignment.UnitRequirements[unitID]
	if !ok {
		return fmt.Errorf("unit %q has no host retrieval requirement", unitID)
	}
	expected := r.retrievalOffsets[unitID]
	if offset != expected {
		return fmt.Errorf("unit %q retrieval offset %d does not follow %d", unitID, offset, expected)
	}
	if complete {
		requirement.RetrievalComplete = true
		r.assignment.UnitRequirements[unitID] = requirement
		delete(r.retrievalOffsets, unitID)
		return nil
	}
	if nextOffset <= offset {
		return fmt.Errorf("unit %q incomplete retrieval has invalid continuation %d", unitID, nextOffset)
	}
	r.retrievalOffsets[unitID] = nextOffset
	return nil
}

func (r *AssignmentRegistry) Snapshot() review.Assignment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return copyAssignment(r.assignment)
}

func copyAssignment(value review.Assignment) review.Assignment {
	result := review.Assignment{
		UnitIDs:          make(map[review.ReviewUnitID]struct{}, len(value.UnitIDs)),
		UnitRequirements: make(map[review.ReviewUnitID]review.UnitRequirement, len(value.UnitRequirements)),
		Findings:         make(map[review.FindingID]review.KnownFinding, len(value.Findings)),
		SourceRefs:       make(map[review.SourceReferenceID]review.SourceReference, len(value.SourceRefs)),
	}
	for id := range value.UnitIDs {
		result.UnitIDs[id] = struct{}{}
	}
	for id, requirement := range value.UnitRequirements {
		result.UnitRequirements[id] = requirement
	}
	for id, finding := range value.Findings {
		copyFinding := finding
		copyFinding.ConsumedSourceRefs = make(map[review.SourceReferenceID]struct{}, len(finding.ConsumedSourceRefs))
		for sourceID := range finding.ConsumedSourceRefs {
			copyFinding.ConsumedSourceRefs[sourceID] = struct{}{}
		}
		result.Findings[id] = copyFinding
	}
	for id, reference := range value.SourceRefs {
		result.SourceRefs[id] = reference
	}
	return result
}
