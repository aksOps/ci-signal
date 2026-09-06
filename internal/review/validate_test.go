package review

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestValidatorAcceptsCompleteSubmissionWithoutFindings(t *testing.T) {
	validator := mustValidator(t)
	assignment := testAssignment()
	raw := []byte(`{
		"verdict":"approved",
		"completion":"complete",
		"findings":[],
		"reassessments":[],
		"acknowledgement_changes":[],
		"coverage":[{"unit_id":"unit-1","outcome":"complete"}],
		"limitations":[]
	}`)
	got, err := validator.Validate(raw, assignment)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got.Verdict != VerdictApproved || got.Completion != SubmissionComplete {
		t.Fatalf("Validate() = %#v", got)
	}
}

func TestValidatorRejectsSchemaAndAssignmentViolations(t *testing.T) {
	validator := mustValidator(t)
	base := map[string]any{
		"verdict":                 "approved",
		"completion":              "complete",
		"findings":                []any{},
		"reassessments":           []any{},
		"acknowledgement_changes": []any{},
		"coverage":                []any{map[string]any{"unit_id": "unit-1", "outcome": "complete"}},
		"limitations":             []any{},
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "unknown field", mutate: func(value map[string]any) { value["labels"] = []any{} }, want: "schema validation"},
		{name: "invalid enum", mutate: func(value map[string]any) { value["verdict"] = "clean" }, want: "schema validation"},
		{name: "foreign coverage", mutate: func(value map[string]any) {
			value["coverage"] = []any{map[string]any{"unit_id": "foreign", "outcome": "complete"}}
		}, want: "foreign unit"},
		{name: "missing coverage", mutate: func(value map[string]any) { value["coverage"] = []any{} }, want: "missing assigned unit"},
		{name: "partial declared complete", mutate: func(value map[string]any) {
			value["coverage"] = []any{map[string]any{"unit_id": "unit-1", "outcome": "partial"}}
		}, want: "complete submission reports"},
		{name: "AI claims checkbox", mutate: func(value map[string]any) {
			value["acknowledgement_changes"] = []any{map[string]any{"finding_id": "finding-1", "action": "acknowledge", "method": "checkbox", "explanation": "Human accepted this.", "source_refs": []any{"discussion-1"}}}
		}, want: "schema validation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cloneMap(t, base)
			test.mutate(value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			_, err = validator.Validate(raw, testAssignment())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidatorRequiresSupportedFreshAcknowledgementEvidence(t *testing.T) {
	validator := mustValidator(t)
	base := `{
		"verdict":"needs_review",
		"completion":"complete",
		"findings":[],
		"reassessments":[{
			"finding_id":"finding-1",
			"assessment":"present",
			"explanation":"The limitation remains present.",
			"evidence":[{"explanation":"The author response discusses this finding.","source_refs":["discussion-1"]}],
			"assigned_units":["unit-1"]
		}],
		"acknowledgement_changes":[{
			"finding_id":"finding-1",
			"action":"acknowledge",
			"method":"ai_discussion",
			"explanation":"The author explicitly accepts the operational limit.",
			"source_refs":["discussion-1"]
		}],
		"coverage":[{"unit_id":"unit-1","outcome":"complete"}],
		"limitations":[]
	}`
	assignment := testAssignmentWithFinding()
	if _, err := validator.Validate([]byte(base), assignment); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	source := assignment.SourceRefs["discussion-1"]
	source.Human = false
	assignment.SourceRefs["discussion-1"] = source
	if _, err := validator.Validate([]byte(base), assignment); err == nil || !strings.Contains(err.Error(), "human discussion") {
		t.Fatalf("Validate() error = %v", err)
	}
	source.Human = true
	source.Consumed = true
	assignment.SourceRefs["discussion-1"] = source
	if _, err := validator.Validate([]byte(base), assignment); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidatorDiscussionAcknowledgementCannotMarkFindingAddressed(t *testing.T) {
	validator := mustValidator(t)
	raw := []byte(`{
		"verdict":"needs_review",
		"completion":"complete",
		"findings":[],
		"reassessments":[{
			"finding_id":"finding-1",
			"assessment":"addressed",
			"explanation":"The author accepts the limitation.",
			"evidence":[{"explanation":"The author response accepts the finding.","source_refs":["discussion-1"]}],
			"assigned_units":["unit-1"]
		}],
		"acknowledgement_changes":[{
			"finding_id":"finding-1",
			"action":"acknowledge",
			"method":"ai_discussion",
			"explanation":"The limitation was accepted.",
			"source_refs":["discussion-1"]
		}],
		"coverage":[{"unit_id":"unit-1","outcome":"complete"}],
		"limitations":[]
	}`)
	if _, err := validator.Validate(raw, testAssignmentWithFinding()); err == nil || !strings.Contains(err.Error(), "cannot mark a finding addressed") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidatorAddressedOpenFindingRequiresBoundCodeChangeAcknowledgement(t *testing.T) {
	validator := mustValidator(t)
	assignment := testAssignmentWithFinding()
	change := assignment.SourceRefs["change-1"]
	change.FindingID = ""
	assignment.SourceRefs["change-1"] = change
	assignment.SourceRefs["change-2"] = SourceReference{ID: "change-2", Kind: SourceRepositorySource}
	base := `{
		"verdict":"approved",
		"completion":"complete",
		"findings":[],
		"reassessments":[{
			"finding_id":"finding-1",
			"assessment":"addressed",
			"explanation":"The code change addresses the finding.",
			"evidence":[{"explanation":"The corrected implementation is present.","source_refs":["change-1"]}],
			"assigned_units":["unit-1"]
		}],
		"acknowledgement_changes":%s,
		"coverage":[{"unit_id":"unit-1","outcome":"complete"}],
		"limitations":[]
	}`

	if _, err := validator.Validate([]byte(fmt.Sprintf(base, `[]`)), assignment); err == nil || !strings.Contains(err.Error(), "requires an ai_code_change acknowledgement") {
		t.Fatalf("missing acknowledgement error = %v", err)
	}
	paired := `[{
		"finding_id":"finding-1",
		"action":"acknowledge",
		"method":"ai_code_change",
		"explanation":"The accepted code evidence addresses the finding.",
		"source_refs":["change-1"]
	}]`
	if _, err := validator.Validate([]byte(fmt.Sprintf(base, paired)), assignment); err != nil {
		t.Fatalf("paired code-change acknowledgement rejected: %v", err)
	}
	unbound := strings.Replace(paired, `"change-1"`, `"change-2"`, 1)
	if _, err := validator.Validate([]byte(fmt.Sprintf(base, unbound)), assignment); err == nil || !strings.Contains(err.Error(), "addressed reassessment evidence") {
		t.Fatalf("unbound acknowledgement error = %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*SourceReference, *KnownFinding)
		want   string
	}{
		{"foreign finding", func(source *SourceReference, _ *KnownFinding) { source.FindingID = "other-finding" }, "associated with finding"},
		{"host consumed", func(source *SourceReference, _ *KnownFinding) { source.Consumed = true }, "already consumed"},
		{"finding consumed", func(_ *SourceReference, known *KnownFinding) {
			known.ConsumedSourceRefs = map[SourceReferenceID]struct{}{"change-1": {}}
		}, "already consumed"},
		{"discussion as code", func(source *SourceReference, _ *KnownFinding) {
			source.Kind = SourceGitLabDiscussion
			source.Human = true
		}, "repository change evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := testAssignmentWithFinding()
			source := candidate.SourceRefs["change-1"]
			source.FindingID = ""
			known := candidate.Findings["finding-1"]
			test.mutate(&source, &known)
			candidate.SourceRefs["change-1"] = source
			candidate.Findings["finding-1"] = known
			if _, err := validator.Validate([]byte(fmt.Sprintf(base, paired)), candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}

	known := assignment.Findings["finding-1"]
	known.State = FindingAcknowledged
	known.Acknowledgement = &Acknowledgement{Method: AcknowledgementCheckbox}
	assignment.Findings["finding-1"] = known
	if _, err := validator.Validate([]byte(fmt.Sprintf(base, `[]`)), assignment); err != nil {
		t.Fatalf("addressed human-acknowledged finding required replacement acknowledgement: %v", err)
	}
}

func TestValidatorReopenRequiresExplicitReassessment(t *testing.T) {
	validator := mustValidator(t)
	raw := []byte(`{
		"verdict":"needs_review",
		"completion":"partial",
		"findings":[],
		"reassessments":[],
		"acknowledgement_changes":[{
			"finding_id":"finding-1",
			"action":"reopen",
			"method":"ai_discussion",
			"explanation":"New evidence makes the earlier acceptance uncertain.",
			"source_refs":["discussion-1"]
		}],
		"coverage":[{"unit_id":"unit-1","outcome":"partial"}],
		"limitations":[]
	}`)
	assignment := testAssignmentWithFinding()
	known := assignment.Findings["finding-1"]
	known.State = FindingAcknowledged
	known.Acknowledgement = &Acknowledgement{Method: AcknowledgementAIDiscussion}
	assignment.Findings["finding-1"] = known
	if _, err := validator.Validate(raw, assignment); err == nil || !strings.Contains(err.Error(), "requires an explicit") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func mustValidator(t *testing.T) *Validator {
	t.Helper()
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func testAssignment() Assignment {
	return Assignment{
		UnitIDs:  map[ReviewUnitID]struct{}{"unit-1": {}},
		Findings: map[FindingID]KnownFinding{},
		SourceRefs: map[SourceReferenceID]SourceReference{
			"discussion-1": {ID: "discussion-1", Kind: SourceGitLabDiscussion, Human: true, FindingID: "finding-1"},
			"change-1":     {ID: "change-1", Kind: SourceRepositoryDiff, FindingID: "finding-1"},
		},
	}
}

func testAssignmentWithFinding() Assignment {
	assignment := testAssignment()
	assignment.Findings["finding-1"] = KnownFinding{ID: "finding-1", State: FindingOpen, ConsumedSourceRefs: map[SourceReferenceID]struct{}{}}
	return assignment
}

func TestValidatorCompleteSubmissionMustReassessKnownFindingsWithOwnedEvidence(t *testing.T) {
	validator := mustValidator(t)
	missing := []byte(`{"verdict":"approved","completion":"complete","findings":[],"reassessments":[],"acknowledgement_changes":[],"coverage":[{"unit_id":"unit-1","outcome":"complete"}],"limitations":[]}`)
	assignment := testAssignmentWithFinding()
	if _, err := validator.Validate(missing, assignment); err == nil || !strings.Contains(err.Error(), "missing reassessment") {
		t.Fatalf("missing reassessment error = %v", err)
	}

	assignment.Findings["finding-2"] = KnownFinding{ID: "finding-2", State: FindingOpen}
	assignment.SourceRefs["other-source"] = SourceReference{ID: "other-source", Kind: SourceRepositoryDiff, FindingID: "finding-2"}
	wrongSource := []byte(`{
		"verdict":"needs_review","completion":"partial","findings":[],
		"reassessments":[{"finding_id":"finding-1","assessment":"present","explanation":"Still present.","evidence":[{"explanation":"Wrong concern.","source_refs":["other-source"]}],"assigned_units":["unit-1"]}],
		"acknowledgement_changes":[],"coverage":[{"unit_id":"unit-1","outcome":"partial"}],"limitations":["Other known finding not assessed."]
	}`)
	if _, err := validator.Validate(wrongSource, assignment); err == nil || !strings.Contains(err.Error(), "associated with finding") {
		t.Fatalf("misassociated source error = %v", err)
	}
}

func TestValidatorRequiresHostCompletedRetrievalAndEnclosingCoverage(t *testing.T) {
	validator := mustValidator(t)
	assignment := testAssignment()
	assignment.UnitIDs["package-1"] = struct{}{}
	assignment.UnitRequirements = map[ReviewUnitID]UnitRequirement{
		"unit-1": {EnclosingUnitID: "package-1"},
	}
	raw := []byte(`{
		"verdict":"needs_review","completion":"partial","findings":[],"reassessments":[],"acknowledgement_changes":[],
		"coverage":[{"unit_id":"unit-1","outcome":"complete"},{"unit_id":"package-1","outcome":"complete"}],"limitations":[]
	}`)
	if _, err := validator.Validate(raw, assignment); err == nil || !strings.Contains(err.Error(), "host retrieval is incomplete") {
		t.Fatalf("incomplete retrieval error = %v", err)
	}

	requirement := assignment.UnitRequirements["unit-1"]
	requirement.RetrievalComplete = true
	assignment.UnitRequirements["unit-1"] = requirement
	raw = []byte(`{
		"verdict":"needs_review","completion":"partial","findings":[],"reassessments":[],"acknowledgement_changes":[],
		"coverage":[{"unit_id":"unit-1","outcome":"complete"},{"unit_id":"package-1","outcome":"partial"}],"limitations":[]
	}`)
	if _, err := validator.Validate(raw, assignment); err == nil || !strings.Contains(err.Error(), "without complete enclosing unit") {
		t.Fatalf("enclosing coverage error = %v", err)
	}
}

func cloneMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
