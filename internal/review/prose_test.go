package review

import (
	"strings"
	"testing"
)

func TestReviewTextRejectsCodeBlocksAndPatches(t *testing.T) {
	for _, value := range []string{
		"Use this:\n\n```go\nreturn 1\n```", "~~~python\nprint(1)\n~~~", "    return 1", "> ```go\n> return 1\n> ```", "<pre>return 1</pre>", "diff --git a/main.go b/main.go\n+return 1", "@@ -1 +1 @@\n-return 0\n+return 1",
	} {
		if err := ValidateProse(value); err == nil {
			t.Errorf("code accepted: %q", value)
		}
	}
	for _, value := range []string{"The `FreeShippingMinimumCents` value is in the wrong unit.", "Review `src/main.go` and keep the existing return unit.", "> The author accepts the documented limit.", "Use the existing validator.\n\n- Preserve the input contract.\n- Explain invalid inputs."} {
		if err := ValidateProse(value); err != nil {
			t.Errorf("prose rejected: %q: %v", value, err)
		}
	}
}

func TestSubmissionValidatesEveryProseField(t *testing.T) {
	for _, field := range []string{"title", "finding", "evidence", "reassessment", "acknowledgement", "coverage", "limitation"} {
		t.Run(field, func(t *testing.T) {
			code := "```go\nreturn 1\n```"
			submission := Submission{Completion: SubmissionPartial, Coverage: []UnitCoverage{{UnitID: "unit-1", Outcome: CoveragePartial}}}
			switch field {
			case "title":
				submission.Findings = []SubmittedFinding{{Title: code}}
			case "finding":
				submission.Findings = []SubmittedFinding{{Explanation: code}}
			case "evidence":
				submission.Findings = []SubmittedFinding{{AssignedUnits: []ReviewUnitID{"unit-1"}, Evidence: []Evidence{{Explanation: code}}}}
			case "reassessment":
				submission.Reassessments = []Reassessment{{Explanation: code}}
			case "acknowledgement":
				submission.AcknowledgementChanges = []AcknowledgementTransition{{Explanation: code}}
			case "coverage":
				submission.Coverage[0].Explanation = code
			case "limitation":
				submission.Limitations = []string{code}
			}
			if err := validateSubmission(submission, testAssignment()); err == nil || !strings.Contains(err.Error(), "prose summary") {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}
