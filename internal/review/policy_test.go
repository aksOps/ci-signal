package review

import "testing"

func TestDeriveVerdictIsConservative(t *testing.T) {
	policy := DefaultVerdictPolicy()
	tests := []struct {
		name  string
		input VerdictInput
		want  Verdict
	}{
		{name: "clean complete", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Coverage: []UnitCoverage{{Outcome: CoverageComplete}}}, want: VerdictApproved},
		{name: "partial", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionPartial}, want: VerdictNeedsReview},
		{name: "failed coverage", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Coverage: []UnitCoverage{{Outcome: CoverageFailed}}}, want: VerdictNeedsReview},
		{name: "stale", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Stale: true}, want: VerdictNeedsReview},
		{name: "model rejects", input: VerdictInput{ProposedVerdict: VerdictNeedsReview, Completion: SubmissionComplete}, want: VerdictNeedsReview},
		{name: "acknowledged risk still gates", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Findings: []Finding{{Category: CategoryRisk, Assessment: AssessmentPresent, State: FindingAcknowledged}}}, want: VerdictNeedsReview},
		{name: "unknown question gates", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Findings: []Finding{{Category: CategoryQuestion, Assessment: AssessmentUnknown}}}, want: VerdictNeedsReview},
		{name: "info alone", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Findings: []Finding{{Category: CategoryInfo, Assessment: AssessmentPresent}}}, want: VerdictApproved},
		{name: "addressed risk", input: VerdictInput{ProposedVerdict: VerdictApproved, Completion: SubmissionComplete, Findings: []Finding{{Category: CategoryRisk, Assessment: AssessmentAddressed}}}, want: VerdictApproved},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DeriveVerdict(test.input, policy).Verdict; got != test.want {
				t.Fatalf("DeriveVerdict() = %q, want %q", got, test.want)
			}
		})
	}
}
