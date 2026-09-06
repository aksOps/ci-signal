package review

type VerdictPolicy struct {
	GatingCategories map[Category]struct{}
}

func DefaultVerdictPolicy() VerdictPolicy {
	return VerdictPolicy{GatingCategories: map[Category]struct{}{
		CategoryBlocker:  {},
		CategoryRisk:     {},
		CategoryQuestion: {},
	}}
}

type VerdictInput struct {
	ProposedVerdict Verdict
	Completion      SubmissionCompletion
	Coverage        []UnitCoverage
	Findings        []Finding
	Stale           bool
}

type VerdictDecision struct {
	Verdict Verdict
	Reasons []string
}

func DeriveVerdict(input VerdictInput, policy VerdictPolicy) VerdictDecision {
	decision := VerdictDecision{Verdict: VerdictApproved}
	if input.Stale {
		decision.Verdict = VerdictNeedsReview
		decision.Reasons = append(decision.Reasons, "review snapshot is stale")
	}
	if input.Completion != SubmissionComplete {
		decision.Verdict = VerdictNeedsReview
		decision.Reasons = append(decision.Reasons, "review submission is incomplete")
	}
	for _, coverage := range input.Coverage {
		if coverage.Outcome == CoveragePartial || coverage.Outcome == CoverageFailed {
			decision.Verdict = VerdictNeedsReview
			decision.Reasons = append(decision.Reasons, "review coverage is incomplete")
			break
		}
	}
	if input.ProposedVerdict != VerdictApproved {
		decision.Verdict = VerdictNeedsReview
		decision.Reasons = append(decision.Reasons, "model proposed needs_review")
	}
	for _, finding := range input.Findings {
		if _, gates := policy.GatingCategories[finding.Category]; !gates {
			continue
		}
		if finding.Assessment == AssessmentPresent || finding.Assessment == AssessmentUnknown {
			decision.Verdict = VerdictNeedsReview
			decision.Reasons = append(decision.Reasons, "a gating finding is present or unknown")
			break
		}
	}
	return decision
}
