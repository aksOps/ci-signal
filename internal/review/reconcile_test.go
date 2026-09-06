package review

import (
	"testing"
	"time"
)

func TestFindingIdentityIgnoresWordingAndLineNumbers(t *testing.T) {
	first := SubmittedFinding{Category: CategoryRisk, Subcategory: SubcategoryCorrections, Title: "Old wording", AssignedUnits: []ReviewUnitID{"b", "a"}, Evidence: []Evidence{{Locations: []Location{{Path: "a.go", StartLine: 10}}}}}
	second := first
	second.Title = "New wording"
	second.Evidence = []Evidence{{Locations: []Location{{Path: "renamed.go", StartLine: 900}}}}
	second.AssignedUnits = []ReviewUnitID{"a", "b"}
	if FindingIdentityKey(first) != FindingIdentityKey(second) {
		t.Fatal("identity changed with wording, location, or unit order")
	}
}

func TestReconcilePreservesMissingAndConfidentIdentityOnly(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	prior := Finding{ID: "finding-old", IdentityKey: FindingIdentityKey(SubmittedFinding{Category: CategoryRisk, Subcategory: SubcategoryCorrections, AssignedUnits: []ReviewUnitID{"unit-1"}}), Category: CategoryRisk, Subcategory: SubcategoryCorrections, Assessment: AssessmentPresent, State: FindingOpen, Title: "Earlier"}
	reconciler := &Reconciler{newFindingID: func() (FindingID, error) { return "finding-new", nil }}
	result, err := reconciler.Reconcile([]Finding{prior}, Submission{}, ReconcileMetadata{RunID: "run-1", At: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].ID != prior.ID || result[0].Assessment != AssessmentPresent {
		t.Fatalf("missing finding was changed: %#v", result)
	}

	submitted := SubmittedFinding{Category: CategoryRisk, Subcategory: SubcategoryCorrections, Title: "Updated", Explanation: "Still fails.", AssignedUnits: []ReviewUnitID{"unit-1"}, Evidence: []Evidence{{Explanation: "Observed."}}}
	result, err = reconciler.Reconcile([]Finding{prior}, Submission{Findings: []SubmittedFinding{submitted}}, ReconcileMetadata{RunID: "run-2", At: now, ConfidentMatches: map[int]FindingID{0: "finding-old"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].ID != prior.ID || result[0].Title != "Updated" {
		t.Fatalf("confident match did not preserve identity: %#v", result)
	}
}

func TestReconcileDoesNotMergeAmbiguousCandidates(t *testing.T) {
	next := 0
	reconciler := &Reconciler{newFindingID: func() (FindingID, error) {
		next++
		return FindingID(string(rune('a' + next))), nil
	}}
	submitted := SubmittedFinding{Category: CategoryRisk, Subcategory: SubcategoryReliability, AssignedUnits: []ReviewUnitID{"unit-1"}}
	result, err := reconciler.Reconcile(nil, Submission{Findings: []SubmittedFinding{submitted, submitted}}, ReconcileMetadata{RunID: "run-1", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].ID == result[1].ID {
		t.Fatalf("ambiguous candidates were merged: %#v", result)
	}
}

func TestReconcileDoesNotInferMatchFromIdentityKey(t *testing.T) {
	next := 0
	reconciler := &Reconciler{newFindingID: func() (FindingID, error) {
		next++
		return "finding-new", nil
	}}
	submitted := SubmittedFinding{Category: CategoryRisk, Subcategory: SubcategoryReliability, AssignedUnits: []ReviewUnitID{"unit-1"}}
	prior := Finding{ID: "finding-old", IdentityKey: FindingIdentityKey(submitted), Category: submitted.Category, Subcategory: submitted.Subcategory}
	result, err := reconciler.Reconcile([]Finding{prior}, Submission{Findings: []SubmittedFinding{submitted}}, ReconcileMetadata{RunID: "run-1", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].ID != "finding-old" || result[1].ID != "finding-new" {
		t.Fatalf("heuristic identity caused a merge: %#v", result)
	}
}

func TestReconcileHistoryRetainsEvidenceProvenance(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	reconciler := &Reconciler{newFindingID: func() (FindingID, error) { return "finding-1", nil }}
	submitted := SubmittedFinding{Category: CategoryRisk, Subcategory: SubcategoryCorrections, AssignedUnits: []ReviewUnitID{"unit-1"}, Evidence: []Evidence{{SourceRefs: []SourceReferenceID{"note-v1"}}}}
	findings, err := reconciler.Reconcile(nil, Submission{Findings: []SubmittedFinding{submitted}}, ReconcileMetadata{RunID: "run-1", At: now})
	if err != nil {
		t.Fatal(err)
	}
	reassessment := Reassessment{FindingID: "finding-1", Assessment: AssessmentPresent, AssignedUnits: []ReviewUnitID{"unit-1"}, Evidence: []Evidence{{SourceRefs: []SourceReferenceID{"note-v2"}}}}
	findings, err = reconciler.Reconcile(findings, Submission{Reassessments: []Reassessment{reassessment}}, ReconcileMetadata{RunID: "run-2", At: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings[0].History) != 2 || len(findings[0].History[0].SourceRefs) != 1 || findings[0].History[0].SourceRefs[0] != "note-v1" || findings[0].History[1].SourceRefs[0] != "note-v2" {
		t.Fatalf("history provenance = %#v", findings[0].History)
	}
}
