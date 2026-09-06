package markdown

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"ci-signal/internal/review"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
)

const (
	stateVersion   = 1
	statePrefix    = "<!-- ci-signal-state"
	findingPrefix  = "<!-- ci-signal-finding:"
	maxStateLength = 8 << 20

	disclaimer = "AI-generated review. Findings and assessments may be incorrect or incomplete. ‘Approved’ is an AI reviewer verdict, not a human approval or a guarantee of correctness."
)

var (
	ErrNotOwned = errors.New("not a ci-signal review report")
	ErrConflict = errors.New("ci-signal review report conflicts with its durable state")

	stateMarkerPattern = regexp.MustCompile(`^<!-- ci-signal-state v=([0-9]+) sha256=([a-f0-9]{64}) data=([A-Za-z0-9+/]+={0,2}) -->$`)
	categoryOrder      = []review.Category{review.CategoryBlocker, review.CategoryRisk, review.CategoryQuestion, review.CategoryInfo}
	subcategoryOrder   = []review.Subcategory{review.SubcategorySecurity, review.SubcategoryCorrections, review.SubcategoryReliability, review.SubcategoryMaintenance, review.SubcategoryOperational, review.SubcategoryInfo}
)

// Interaction supplies host-validated provenance for a human control change.
// Body text is deliberately absent: this codec does not infer AI acknowledgement.
type Interaction struct {
	SourceRefID review.SourceReferenceID
	GitLabID    int64
	Author      string
	Human       bool
	FindingID   review.FindingID
	At          time.Time
}

type Codec struct {
	parser parser.Parser
}

func NewCodec() *Codec {
	return &Codec{parser: parser.New(parser.WithExtensions(extension.GFMParser))}
}

// Canonicalize returns the exact durable state encoded by Encode. It copies
// all slices and normalizes timestamps and rendered checkbox memory.
func Canonicalize(state review.State) review.State {
	encoded, _ := json.Marshal(state)
	var out review.State
	_ = json.Unmarshal(encoded, &out)
	normalizeTimes(&out)
	for i := range out.Findings {
		finding := &out.Findings[i]
		switch {
		case finding.State == review.FindingOpen:
			value := false
			finding.LastRenderedCheckbox = &value
		case finding.Acknowledgement != nil && finding.Acknowledgement.Method == review.AcknowledgementCheckbox:
			value := true
			finding.LastRenderedCheckbox = &value
		default:
			finding.LastRenderedCheckbox = nil
		}
	}
	return out
}

func (c *Codec) Encode(state review.State) (string, error) {
	state = Canonicalize(state)
	if err := validateState(state); err != nil {
		return "", err
	}

	var out strings.Builder
	verdict := deriveVerdict(state)
	fmt.Fprintf(&out, "## Verdict: %s\n\n", verdictLabel(verdict))
	open, acknowledged := findingCounts(state.Findings)
	out.WriteString("## Stats\n\n")
	fmt.Fprintf(&out, "**Findings:** open %d · acknowledged %d\n\n", open, acknowledged)
	fmt.Fprintf(&out, "**Assigned review units:** %s\n\n", coverageLabel(state.Runs))
	out.WriteString("Coverage counts accepted outcomes for host-assigned review work, not tests or full-project coverage.\n\n")
	out.WriteString("## Open\n\n")
	if open == 0 {
		out.WriteString("No open findings.\n")
	} else {
		renderFindingGroups(&out, state.Findings, review.FindingOpen)
	}

	if acknowledged > 0 {
		out.WriteString("\n<details>\n<summary>Acknowledged (" + fmt.Sprint(acknowledged) + ")</summary>\n\n")
		renderFindingGroups(&out, state.Findings, review.FindingAcknowledged)
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

func (c *Codec) Decode(source string) (review.State, error) {
	if c == nil || c.parser == nil {
		return review.State{}, errors.New("markdown codec is not initialized")
	}
	state, err := decodeStateMarker(source)
	if err != nil {
		return review.State{}, err
	}
	if err := validateState(state); err != nil {
		return review.State{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	controls, err := c.controls(source)
	if err != nil {
		return review.State{}, err
	}
	if err := validateControlBindings(state, controls); err != nil {
		return review.State{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return state, nil
}

func (c *Codec) ReconcileControls(source string, state review.State, interactions []Interaction, at time.Time) (review.State, bool, error) {
	embedded, err := c.Decode(source)
	if err != nil {
		return state, false, err
	}
	if at.IsZero() {
		return state, false, errors.New("control reconciliation timestamp is required")
	}
	if embedded.MR != state.MR || !containsFindings(state.Findings, embedded.Findings) {
		return state, false, fmt.Errorf("%w: supplied state does not match report binding", ErrConflict)
	}
	controls, err := c.controls(source)
	if err != nil {
		return state, false, err
	}

	result := cloneState(state)
	byID := make(map[review.FindingID]control, len(controls))
	for _, value := range controls {
		byID[value.FindingID] = value
	}
	changed := false
	for i := range result.Findings {
		finding := &result.Findings[i]
		observed := byID[finding.ID]
		prior := findFinding(embedded.Findings, finding.ID)
		if prior == nil {
			continue
		}
		// The predecessor's rendered control determines whether a checkbox is
		// designated. A checkbox inserted beside a predecessor AI robot is
		// inert. A late human edit to a real predecessor checkbox wins over a
		// successor AI transition based on older evidence.
		if prior.LastRenderedCheckbox == nil {
			continue
		}
		baseline := finding.LastRenderedCheckbox
		if baseline == nil || (finding.Acknowledgement != nil && finding.Acknowledgement.Method != review.AcknowledgementCheckbox) {
			baseline = prior.LastRenderedCheckbox
		}
		if !observed.Task || baseline == nil || observed.Checked == *baseline {
			continue
		}

		sourceRefs := interactionSources(interactions, finding.ID, at)
		if observed.Checked {
			finding.State = review.FindingAcknowledged
			finding.Acknowledgement = &review.Acknowledgement{Method: review.AcknowledgementCheckbox, SourceRefs: sourceRefs, At: at.UTC()}
			value := true
			finding.LastRenderedCheckbox = &value
			finding.History = append(finding.History, review.FindingEvent{Kind: review.FindingEventAcknowledged, At: at.UTC(), Method: review.AcknowledgementCheckbox, SourceRefs: sourceRefs})
		} else {
			finding.State = review.FindingOpen
			finding.Acknowledgement = nil
			value := false
			finding.LastRenderedCheckbox = &value
			finding.History = append(finding.History, review.FindingEvent{Kind: review.FindingEventReopened, At: at.UTC(), Method: review.AcknowledgementCheckbox, SourceRefs: sourceRefs})
		}
		changed = true
	}
	return result, changed, nil
}

func encodeStateMarker(state review.State) (string, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("encode report state: %w", err)
	}
	if len(payload) > maxStateLength {
		return "", errors.New("encoded report state exceeds the codec limit")
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("<!-- ci-signal-state v=%d sha256=%s data=%s -->", stateVersion, hex.EncodeToString(digest[:]), base64.StdEncoding.EncodeToString(payload)), nil
}

func decodeStateMarker(source string) (review.State, error) {
	count := strings.Count(source, statePrefix)
	if count == 0 {
		return review.State{}, ErrNotOwned
	}
	if count != 1 {
		return review.State{}, fmt.Errorf("%w: duplicate owned state markers", ErrConflict)
	}
	start := strings.Index(source, statePrefix)
	endOffset := strings.Index(source[start:], "-->")
	if endOffset < 0 {
		return review.State{}, fmt.Errorf("%w: unterminated owned state marker", ErrConflict)
	}
	marker := source[start : start+endOffset+3]
	match := stateMarkerPattern.FindStringSubmatch(marker)
	if match == nil {
		return review.State{}, fmt.Errorf("%w: malformed owned state marker", ErrConflict)
	}
	if match[1] != fmt.Sprint(stateVersion) {
		return review.State{}, fmt.Errorf("%w: unknown state marker version %s", ErrConflict, match[1])
	}
	payload, err := base64.StdEncoding.DecodeString(match[3])
	if err != nil || len(payload) > maxStateLength {
		return review.State{}, fmt.Errorf("%w: invalid owned state payload", ErrConflict)
	}
	digest := sha256.Sum256(payload)
	if !bytes.Equal([]byte(match[2]), []byte(hex.EncodeToString(digest[:]))) {
		return review.State{}, fmt.Errorf("%w: owned state checksum mismatch", ErrConflict)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var state review.State
	if err := decoder.Decode(&state); err != nil {
		return review.State{}, fmt.Errorf("%w: invalid owned state: %v", ErrConflict, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return review.State{}, fmt.Errorf("%w: trailing owned state data", ErrConflict)
	}
	return state, nil
}

type control struct {
	FindingID review.FindingID
	Task      bool
	Checked   bool
}

func (c *Codec) controls(source string) ([]control, error) {
	document := c.parser.Parse([]byte(source))
	var controls []control
	err := ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		item, ok := node.(*ast.ListItem)
		if !ok || insideBlockquote(item) {
			return ast.WalkContinue, nil
		}
		ids := findingMarkers(item, []byte(source))
		if len(ids) == 0 {
			return ast.WalkContinue, nil
		}
		if len(ids) != 1 {
			return ast.WalkStop, fmt.Errorf("%w: list item has duplicate finding markers", ErrConflict)
		}
		status, task := extension.TaskStatusOf(item)
		controls = append(controls, control{FindingID: ids[0], Task: task, Checked: status == extension.TaskStatusCompleted})
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return nil, err
	}
	return controls, nil
}

func findingMarkers(item *ast.ListItem, source []byte) []review.FindingID {
	var ids []review.FindingID
	_ = ast.Walk(item, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		raw, ok := node.(*ast.RawHTML)
		if !ok {
			return ast.WalkContinue, nil
		}
		value := raw.Value.Str(source)
		if id, ok := decodeFindingMarker(value); ok {
			ids = append(ids, id)
		}
		return ast.WalkContinue, nil
	})
	return ids
}

func decodeFindingMarker(value string) (review.FindingID, bool) {
	if !strings.HasPrefix(value, findingPrefix) || !strings.HasSuffix(value, " -->") {
		return "", false
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(value, findingPrefix), " -->")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return review.FindingID(decoded), true
}

func validateControlBindings(state review.State, controls []control) error {
	expected := make(map[review.FindingID]review.Finding, len(state.Findings))
	for _, finding := range state.Findings {
		expected[finding.ID] = finding
	}
	seen := make(map[review.FindingID]struct{}, len(controls))
	for _, value := range controls {
		finding, ok := expected[value.FindingID]
		if !ok {
			return fmt.Errorf("finding marker %q is foreign", value.FindingID)
		}
		if _, duplicate := seen[value.FindingID]; duplicate {
			return fmt.Errorf("finding marker %q is duplicated", value.FindingID)
		}
		seen[value.FindingID] = struct{}{}
		if finding.Acknowledgement != nil && finding.Acknowledgement.Method != review.AcknowledgementCheckbox && value.Task {
			// An inserted checkbox on an AI acknowledgement is inert, so it is
			// accepted and ignored during reconciliation.
			continue
		}
		if finding.LastRenderedCheckbox != nil && !value.Task {
			return fmt.Errorf("finding %q lost its designated checkbox", finding.ID)
		}
	}
	for id := range expected {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("finding %q has no visible stable marker", id)
		}
	}
	return nil
}

func validateState(state review.State) error {
	if state.SchemaVersion != stateVersion {
		return fmt.Errorf("unsupported state schema version %d", state.SchemaVersion)
	}
	if state.MR.BaseURL == "" || state.MR.Project == "" || state.MR.MRIID <= 0 {
		return errors.New("MR binding is incomplete")
	}
	seen := make(map[review.FindingID]struct{}, len(state.Findings))
	sources := make(map[review.SourceReferenceID]struct{}, len(state.Sources))
	for _, source := range state.Sources {
		if source.ID == "" || source.Body == "" {
			return errors.New("durable public source record is incomplete")
		}
		if _, duplicate := sources[source.ID]; duplicate {
			return fmt.Errorf("durable source ID %q is duplicated", source.ID)
		}
		sources[source.ID] = struct{}{}
		if source.Kind != review.SourceGitLabDiscussion && source.Kind != review.SourceGitLabNote {
			return fmt.Errorf("durable source %q has unsupported kind %q", source.ID, source.Kind)
		}
	}
	for _, finding := range state.Findings {
		if finding.ID == "" {
			return errors.New("finding ID is empty")
		}
		if _, duplicate := seen[finding.ID]; duplicate {
			return fmt.Errorf("finding ID %q is duplicated", finding.ID)
		}
		seen[finding.ID] = struct{}{}
		if !validCategory(finding.Category) || !validSubcategory(finding.Subcategory) || !validAssessment(finding.Assessment) || !validRelationship(finding.Relationship) {
			return fmt.Errorf("finding %q contains an invalid bounded domain value", finding.ID)
		}
		switch finding.State {
		case review.FindingOpen:
			if finding.Acknowledgement != nil {
				return fmt.Errorf("open finding %q has acknowledgement metadata", finding.ID)
			}
		case review.FindingAcknowledged:
			if finding.Acknowledgement == nil {
				return fmt.Errorf("acknowledged finding %q lacks acknowledgement metadata", finding.ID)
			}
			if !validAcknowledgementMethod(finding.Acknowledgement.Method) {
				return fmt.Errorf("finding %q has invalid acknowledgement method %q", finding.ID, finding.Acknowledgement.Method)
			}
		default:
			return fmt.Errorf("finding %q has invalid state %q", finding.ID, finding.State)
		}
	}
	for _, run := range state.Runs {
		if run.Completion != review.SubmissionComplete && run.Completion != review.SubmissionPartial {
			return fmt.Errorf("run %q has invalid completion %q", run.ID, run.Completion)
		}
		if run.Verdict != "" && run.Verdict != review.VerdictApproved && run.Verdict != review.VerdictNeedsReview {
			return fmt.Errorf("run %q has invalid verdict %q", run.ID, run.Verdict)
		}
		for _, coverage := range run.Coverage {
			if !validCoverage(coverage.Outcome) {
				return fmt.Errorf("run %q has invalid coverage outcome %q", run.ID, coverage.Outcome)
			}
		}
	}
	return nil
}

func validCategory(value review.Category) bool {
	switch value {
	case review.CategoryBlocker, review.CategoryRisk, review.CategoryQuestion, review.CategoryInfo:
		return true
	default:
		return false
	}
}

func validSubcategory(value review.Subcategory) bool {
	switch value {
	case review.SubcategorySecurity, review.SubcategoryCorrections, review.SubcategoryReliability, review.SubcategoryMaintenance, review.SubcategoryOperational, review.SubcategoryInfo:
		return true
	default:
		return false
	}
}

func validAssessment(value review.Assessment) bool {
	switch value {
	case review.AssessmentPresent, review.AssessmentAddressed, review.AssessmentNotApplicable, review.AssessmentUnknown:
		return true
	default:
		return false
	}
}

func validRelationship(value review.Relationship) bool {
	switch value {
	case review.RelationshipIntroduced, review.RelationshipRegressed, review.RelationshipPreExisting, review.RelationshipUnknown:
		return true
	default:
		return false
	}
}

func validAcknowledgementMethod(value review.AcknowledgementMethod) bool {
	switch value {
	case review.AcknowledgementCheckbox, review.AcknowledgementAIDiscussion, review.AcknowledgementAICodeChange:
		return true
	default:
		return false
	}
}

func validCoverage(value review.CoverageOutcome) bool {
	switch value {
	case review.CoverageComplete, review.CoveragePartial, review.CoverageFailed, review.CoverageExcludedByPolicy:
		return true
	default:
		return false
	}
}

func renderFindingGroups(out *strings.Builder, findings []review.Finding, state review.FindingState) {
	for _, category := range categoryOrder {
		if !containsFindingGroup(findings, state, category, "") {
			continue
		}
		fmt.Fprintf(out, "### %s\n\n", categoryLabel(category))
		for _, subcategory := range subcategoryOrder {
			if !containsFindingGroup(findings, state, category, subcategory) {
				continue
			}
			fmt.Fprintf(out, "#### %s\n\n", subcategoryLabel(subcategory))
			for i := range findings {
				if findings[i].State == state && findings[i].Category == category && findings[i].Subcategory == subcategory {
					renderFinding(out, findings[i], state == review.FindingAcknowledged)
					out.WriteByte('\n')
				}
			}
		}
	}
}

func containsFindingGroup(findings []review.Finding, state review.FindingState, category review.Category, subcategory review.Subcategory) bool {
	for i := range findings {
		if findings[i].State == state && findings[i].Category == category && (subcategory == "" || findings[i].Subcategory == subcategory) {
			return true
		}
	}
	return false
}

func renderFinding(out *strings.Builder, finding review.Finding, acknowledged bool) {
	marker := findingMarker(finding.ID)
	prefix := "- [ ] "
	if acknowledged {
		if finding.Acknowledgement != nil && finding.Acknowledgement.Method != review.AcknowledgementCheckbox {
			prefix = "- 🤖 "
		} else {
			prefix = "- [x] "
		}
	}
	fmt.Fprintf(out, "%s**%s** %s\n", prefix, escape(finding.Title), marker)
	if finding.Explanation != "" {
		fmt.Fprintf(out, "  %s\n", indentMultiline(escape(finding.Explanation), "  "))
	}
	assessment := escape(humanize(string(finding.Assessment)))
	if finding.Assessment == review.AssessmentAddressed {
		assessment = "✅ " + assessment
	}
	fmt.Fprintf(out, "  - **Assessment:** %s\n", assessment)
	for _, evidence := range finding.Evidence {
		fmt.Fprintf(out, "  - **Evidence:** %s", escape(evidence.Explanation))
		if locations := renderLocations(evidence.Locations); locations != "" {
			fmt.Fprintf(out, " — %s", locations)
		}
		out.WriteByte('\n')
	}
}

func findingMarker(id review.FindingID) string {
	return findingPrefix + base64.StdEncoding.EncodeToString([]byte(id)) + " -->"
}

func renderLocations(locations []review.Location) string {
	values := make([]string, 0, len(locations))
	for _, location := range locations {
		value := escape(location.Path)
		if location.StartLine > 0 {
			value += ":" + fmt.Sprint(location.StartLine)
			if location.EndLine > location.StartLine {
				value += "-" + fmt.Sprint(location.EndLine)
			}
		}
		values = append(values, value)
	}
	return strings.Join(values, ", ")
}

func escape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	replacer := strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_",
		"{", "\\{", "}", "\\}", "[", "\\[", "]", "\\]",
		"(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+",
		"-", "\\-", ".", "\\.", "!", "\\!", "|", "\\|",
	)
	value = replacer.Replace(value)
	lines := strings.Split(value, "\n")
	for i := range lines {
		trimmed := strings.TrimLeft(lines[i], " \t")
		if strings.HasPrefix(trimmed, "/") {
			at := len(lines[i]) - len(trimmed)
			lines[i] = lines[i][:at] + "\\" + lines[i][at:]
		}
	}
	return strings.Join(lines, "\n")
}

func indentMultiline(value, indent string) string {
	return strings.ReplaceAll(value, "\n", "\n"+indent)
}

func categoryLabel(value review.Category) string {
	return map[review.Category]string{
		review.CategoryBlocker: "🛑 Blocker", review.CategoryRisk: "⚠️ Risk",
		review.CategoryQuestion: "❓ Question", review.CategoryInfo: "ℹ️ Info",
	}[value]
}

func subcategoryLabel(value review.Subcategory) string {
	return map[review.Subcategory]string{
		review.SubcategorySecurity: "🔒 Security", review.SubcategoryCorrections: "🐛 Corrections",
		review.SubcategoryReliability: "🛡️ Reliability", review.SubcategoryMaintenance: "🧰 Maintenance",
		review.SubcategoryOperational: "⚙️ Operational", review.SubcategoryInfo: "📘 Info",
	}[value]
}

func verdictLabel(value review.Verdict) string {
	if value == review.VerdictApproved {
		return "✅ Approved"
	}
	return "⚠️ Needs Review"
}

func deriveVerdict(state review.State) review.Verdict {
	if len(state.Runs) == 0 {
		return review.VerdictNeedsReview
	}
	latest := state.Runs[len(state.Runs)-1]
	if latest.Verdict == review.VerdictApproved || latest.Verdict == review.VerdictNeedsReview {
		return latest.Verdict
	}
	return review.DeriveVerdict(review.VerdictInput{
		ProposedVerdict: review.VerdictApproved,
		Completion:      latest.Completion,
		Coverage:        latest.Coverage,
		Findings:        state.Findings,
	}, review.DefaultVerdictPolicy()).Verdict
}

func coverageLabel(runs []review.Run) string {
	if len(runs) == 0 {
		return "unknown"
	}
	counts := map[review.CoverageOutcome]int{}
	for _, unit := range runs[len(runs)-1].Coverage {
		counts[unit.Outcome]++
	}
	return fmt.Sprintf("complete %d · partial %d · failed %d · excluded %d",
		counts[review.CoverageComplete], counts[review.CoveragePartial], counts[review.CoverageFailed], counts[review.CoverageExcludedByPolicy])
}

func findingCounts(findings []review.Finding) (open, acknowledged int) {
	for _, finding := range findings {
		if finding.State == review.FindingAcknowledged {
			acknowledged++
		} else {
			open++
		}
	}
	return open, acknowledged
}

func humanize(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	if value == "" {
		return "Unknown"
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func interactionSources(interactions []Interaction, findingID review.FindingID, at time.Time) []review.SourceReferenceID {
	var newest *Interaction
	for i := range interactions {
		candidate := &interactions[i]
		if !candidate.Human || candidate.SourceRefID == "" || (candidate.FindingID != "" && candidate.FindingID != findingID) || candidate.At.After(at) {
			continue
		}
		if newest == nil || candidate.At.After(newest.At) {
			newest = candidate
		}
	}
	if newest == nil {
		return nil
	}
	return []review.SourceReferenceID{newest.SourceRefID}
}

func containsFindings(current, predecessor []review.Finding) bool {
	ids := make(map[review.FindingID]struct{}, len(current))
	for _, finding := range current {
		ids[finding.ID] = struct{}{}
	}
	for _, finding := range predecessor {
		if _, ok := ids[finding.ID]; !ok {
			return false
		}
	}
	return true
}

func insideBlockquote(node ast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, ok := parent.(*ast.Blockquote); ok {
			return true
		}
	}
	return false
}

func findFinding(findings []review.Finding, id review.FindingID) *review.Finding {
	for i := range findings {
		if findings[i].ID == id {
			return &findings[i]
		}
	}
	return nil
}

func cloneState(state review.State) review.State {
	encoded, _ := json.Marshal(state)
	var out review.State
	_ = json.Unmarshal(encoded, &out)
	return out
}

func normalizeTimes(state *review.State) {
	state.Snapshot.CapturedAt = state.Snapshot.CapturedAt.UTC()
	if state.Publication != nil {
		state.Publication.PublishedAt = state.Publication.PublishedAt.UTC()
	}
	for i := range state.Sources {
		state.Sources[i].At = state.Sources[i].At.UTC()
	}
	for i := range state.Runs {
		state.Runs[i].StartedAt = state.Runs[i].StartedAt.UTC()
		state.Runs[i].CompletedAt = state.Runs[i].CompletedAt.UTC()
	}
	for i := range state.Findings {
		finding := &state.Findings[i]
		finding.FirstSeenAt = finding.FirstSeenAt.UTC()
		finding.LastSeenAt = finding.LastSeenAt.UTC()
		if finding.Acknowledgement != nil {
			finding.Acknowledgement.At = finding.Acknowledgement.At.UTC()
		}
		for j := range finding.History {
			finding.History[j].At = finding.History[j].At.UTC()
		}
	}
}
