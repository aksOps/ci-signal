package markdown

import (
	"fmt"
	"strings"

	"ci-signal/internal/review"
)

// EncodeLegacyPublication reproduces the original report bytes only to verify
// historical publication digests. New reports use Encode and state-only digests.
func (c *Codec) EncodeLegacyPublication(state review.State) (string, error) {
	state = Canonicalize(state)
	if err := validateState(state); err != nil {
		return "", err
	}

	var out strings.Builder
	verdict := deriveVerdict(state)
	fmt.Fprintf(&out, "## %s\n\n", legacyVerdictLabel(verdict))
	open, acknowledged := findingCounts(state.Findings)
	fmt.Fprintf(&out, "**Open:** %d · **Acknowledged:** %d · **Coverage:** %s\n\n", open, acknowledged, legacyCoverageLabel(state.Runs))
	out.WriteString("## Open findings\n\n")
	if open == 0 {
		out.WriteString("No open findings.\n")
	} else {
		for i := range state.Findings {
			if state.Findings[i].State == review.FindingOpen {
				renderFindingFormat(&out, state.Findings[i], false, true)
			}
		}
	}

	if acknowledged > 0 {
		out.WriteString("\n<details>\n<summary>Acknowledged (" + fmt.Sprint(acknowledged) + ")</summary>\n\n")
		for i := range state.Findings {
			if state.Findings[i].State == review.FindingAcknowledged {
				renderFindingFormat(&out, state.Findings[i], true, true)
			}
		}
		out.WriteString("</details>\n")
	}

	out.WriteString("\n" + disclaimer + "\n\n")
	marker, err := encodeStateMarker(state)
	if err != nil {
		return "", err
	}
	out.WriteString(marker + "\n")
	return out.String(), nil
}

func legacyVerdictLabel(value review.Verdict) string {
	if value == review.VerdictApproved {
		return "✅ Approved"
	}
	return "⚠️ Needs review"
}

func legacyCoverageLabel(runs []review.Run) string {
	if len(runs) == 0 {
		return "unknown"
	}
	counts := map[review.CoverageOutcome]int{}
	for _, unit := range runs[len(runs)-1].Coverage {
		counts[unit.Outcome]++
	}
	return fmt.Sprintf("complete %d, partial %d, failed %d, excluded %d",
		counts[review.CoverageComplete], counts[review.CoveragePartial], counts[review.CoverageFailed], counts[review.CoverageExcludedByPolicy])
}
