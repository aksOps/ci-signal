package copilot

import (
	"testing"

	"ci-signal/internal/review"
)

func TestAssignmentRegistryRegistersEvidenceIdempotently(t *testing.T) {
	registry, err := NewAssignmentRegistry(review.Assignment{
		UnitIDs:    map[review.ReviewUnitID]struct{}{"unit-1": {}},
		Findings:   map[review.FindingID]review.KnownFinding{},
		SourceRefs: map[review.SourceReferenceID]review.SourceReference{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference := review.SourceReference{ID: "src_1", Kind: review.SourceRepositorySource, Commit: "0123456789abcdef0123456789abcdef01234567", Path: "main.go"}
	if err := registry.RegisterSource(reference); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterSource(reference); err != nil {
		t.Fatalf("identical registration should be idempotent: %v", err)
	}
	conflicting := reference
	conflicting.Path = "other.go"
	if err := registry.RegisterSource(conflicting); err == nil {
		t.Fatal("conflicting duplicate source reference was accepted")
	}
	snapshot := registry.Snapshot()
	if got := snapshot.SourceRefs[reference.ID]; got.Path != "main.go" {
		t.Fatalf("registered reference = %#v", got)
	}
	delete(snapshot.SourceRefs, reference.ID)
	if _, ok := registry.Snapshot().SourceRefs[reference.ID]; !ok {
		t.Fatal("snapshot mutation changed registry state")
	}
}

func TestAssignmentRegistrySeparatesSourcesFromRetrievalCompletion(t *testing.T) {
	registry, err := NewAssignmentRegistry(review.Assignment{
		UnitIDs:          map[review.ReviewUnitID]struct{}{"unit-1": {}},
		UnitRequirements: map[review.ReviewUnitID]review.UnitRequirement{"unit-1": {}},
		Findings:         map[review.FindingID]review.KnownFinding{},
		SourceRefs:       map[review.SourceReferenceID]review.SourceReference{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterSource(review.SourceReference{ID: "source-1", Kind: review.SourceRepositorySource}); err != nil {
		t.Fatal(err)
	}
	if registry.Snapshot().UnitRequirements["unit-1"].RetrievalComplete {
		t.Fatal("registering a citation source marked retrieval complete")
	}
	if err := registry.RecordUnitRetrieval("unit-1", 9, 0, true); err == nil {
		t.Fatal("out-of-order retrieval was accepted")
	}
	if err := registry.RecordUnitRetrieval("unit-1", 0, 10, false); err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordUnitRetrieval("unit-1", 10, 0, true); err != nil {
		t.Fatal(err)
	}
	if !registry.Snapshot().UnitRequirements["unit-1"].RetrievalComplete {
		t.Fatal("host retrieval completion was not retained")
	}
}
