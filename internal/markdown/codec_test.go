package markdown

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"ci-signal/internal/review"
)

func TestCodecRoundTripCanonicalState(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	state := sampleState()

	source, err := codec.Encode(state)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := codec.Decode(source)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if want := Canonicalize(state); !reflect.DeepEqual(decoded, want) {
		t.Fatalf("Decode(Encode(state)) mismatch\ngot:  %#v\nwant: %#v", decoded, want)
	}

	assertOrder(t, source,
		"## Verdict: ⚠️ Needs Review",
		"## Stats",
		"**Findings:** open 1 · acknowledged 2",
		"**Assigned review units:** complete 2 · partial 0 · failed 0 · excluded 1",
		"Coverage counts accepted outcomes for host-assigned review work, not tests or full-project coverage.",
		"## Open",
		"### 🛑 Blocker",
		"#### 🐛 Corrections",
		"<summary>Acknowledged (2)</summary>",
		disclaimer,
		statePrefix,
	)
	for _, expected := range []string{"- [ ] **Cross\\-file amount regression**", "### ⚠️ Risk", "#### ⚙️ Operational", "- [x] **Retry limit is low**", "### ℹ️ Info", "#### 📘 Info", "- 🤖 **Migration context**", "**Assessment:** ✅ Addressed"} {
		if !strings.Contains(source, expected) {
			t.Fatalf("rendered report missing %q:\n%s", expected, source)
		}
	}
	if strings.Contains(source, string(state.Findings[0].ID)) {
		t.Fatal("finding ID was rendered visibly")
	}
	if strings.Count(source, "- [ ] ") != 1 || strings.Count(source, "- [x] ") != 1 || strings.Count(source, "- 🤖 ") != 1 {
		t.Fatalf("unexpected task/robot controls:\n%s", source)
	}
	if strings.Contains(source, "AI-assisted acknowledgement") {
		t.Fatal("forbidden AI acknowledgement label rendered")
	}
}

func TestReconcileControlsCheckUncheckAndIdempotence(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	state := sampleState()
	state.Findings = state.Findings[:1]
	source, err := codec.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	checked := strings.Replace(source, "- [ ] ", "- [x] ", 1)
	at := time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC)
	interactions := []Interaction{{SourceRefID: "src_checkbox", Human: true, FindingID: "f_open", At: at.Add(-time.Second)}}

	acknowledged, changed, err := codec.ReconcileControls(checked, Canonicalize(state), interactions, at)
	if err != nil || !changed {
		t.Fatalf("check reconciliation = changed %v, err %v", changed, err)
	}
	finding := acknowledged.Findings[0]
	if finding.State != review.FindingAcknowledged || finding.Acknowledgement == nil || finding.Acknowledgement.Method != review.AcknowledgementCheckbox {
		t.Fatalf("check did not acknowledge finding: %#v", finding)
	}
	if !reflect.DeepEqual(finding.Acknowledgement.SourceRefs, []review.SourceReferenceID{"src_checkbox"}) {
		t.Fatalf("acknowledgement provenance = %v", finding.Acknowledgement.SourceRefs)
	}
	historyLength := len(finding.History)
	again, changed, err := codec.ReconcileControls(checked, acknowledged, interactions, at.Add(time.Minute))
	if err != nil || changed || len(again.Findings[0].History) != historyLength {
		t.Fatalf("unchanged checkbox was not idempotent: changed %v, history %d, err %v", changed, len(again.Findings[0].History), err)
	}

	ackSource, err := codec.Encode(acknowledged)
	if err != nil {
		t.Fatal(err)
	}
	unchecked := strings.Replace(ackSource, "- [x] ", "- [ ] ", 1)
	reopened, changed, err := codec.ReconcileControls(unchecked, Canonicalize(acknowledged), interactions, at.Add(2*time.Minute))
	if err != nil || !changed {
		t.Fatalf("uncheck reconciliation = changed %v, err %v", changed, err)
	}
	if reopened.Findings[0].State != review.FindingOpen || reopened.Findings[0].Acknowledgement != nil {
		t.Fatalf("uncheck did not reopen finding: %#v", reopened.Findings[0])
	}
	if got := reopened.Findings[0].History[len(reopened.Findings[0].History)-1]; got.Kind != review.FindingEventReopened || got.Method != review.AcknowledgementCheckbox {
		t.Fatalf("reopen event = %#v", got)
	}
}

func TestReconcileLatePredecessorControlIntoSuccessor(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	predecessor := sampleState()
	predecessor.Findings = predecessor.Findings[:1]
	predecessorSource, err := codec.Encode(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	predecessorSource = strings.Replace(predecessorSource, "- [ ] ", "- [x] ", 1)

	successor := Canonicalize(predecessor)
	successor.Snapshot.ID = "snap_2"
	successor.Fingerprint = "fp_2"
	successor.Publication = &review.Publication{Generation: "pub_2", NoteID: 100, PredecessorNoteID: 99, PublishedAt: time.Now()}
	successor.Findings = append(successor.Findings, review.Finding{
		ID: "f_new", IdentityKey: "identity-new", Category: review.CategoryInfo, Subcategory: review.SubcategoryInfo,
		Relationship: review.RelationshipIntroduced, Title: "Successor-only context", Assessment: review.AssessmentPresent,
		State: review.FindingOpen, Evidence: []review.Evidence{}, AssignedUnits: []review.ReviewUnitID{}, History: []review.FindingEvent{},
	})

	result, changed, err := codec.ReconcileControls(predecessorSource, successor, nil, time.Now())
	if err != nil || !changed {
		t.Fatalf("late predecessor reconciliation = changed %v, err %v", changed, err)
	}
	if result.Findings[0].State != review.FindingAcknowledged || result.Findings[1].State != review.FindingOpen {
		t.Fatalf("late predecessor control affected wrong state: %#v", result.Findings)
	}
	if result.Snapshot.ID != "snap_2" || result.Publication.Generation != "pub_2" {
		t.Fatal("late predecessor reconciliation replaced successor binding")
	}
}

func TestLateHumanUncheckOverridesOlderSuccessorAIAcknowledgement(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	predecessor := sampleState()
	predecessor.Findings = predecessor.Findings[1:2]
	predecessorSource, err := codec.Encode(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	predecessorSource = strings.Replace(predecessorSource, "- [x] ", "- [ ] ", 1)

	successor := Canonicalize(predecessor)
	successor.Snapshot.ID = "snap_2"
	successor.Fingerprint = "fp_2"
	successor.Publication = &review.Publication{Generation: "pub_2", NoteID: 100, PredecessorNoteID: 99, PublishedAt: time.Now()}
	successor.Findings[0].Acknowledgement = &review.Acknowledgement{
		Method: review.AcknowledgementAIDiscussion, SourceRefs: []review.SourceReferenceID{"src_old_discussion"}, At: time.Now().Add(-time.Minute),
	}
	successor.Findings[0].LastRenderedCheckbox = nil

	result, changed, err := codec.ReconcileControls(predecessorSource, successor, []Interaction{{
		SourceRefID: "src_late_control", Human: true, FindingID: "f_human", At: time.Now(),
	}}, time.Now().Add(time.Second))
	if err != nil || !changed {
		t.Fatalf("late human reversal = changed %v, err %v", changed, err)
	}
	if result.Findings[0].State != review.FindingOpen || result.Findings[0].Acknowledgement != nil {
		t.Fatalf("late human reversal did not reopen successor: %#v", result.Findings[0])
	}
	last := result.Findings[0].History[len(result.Findings[0].History)-1]
	if last.Kind != review.FindingEventReopened || !reflect.DeepEqual(last.SourceRefs, []review.SourceReferenceID{"src_late_control"}) {
		t.Fatalf("late human reversal history = %#v", last)
	}
}

func TestAIControlTamperingIsInert(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	state := sampleState()
	state.Findings = state.Findings[2:]
	source, err := codec.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(source, "- 🤖 ", "- [x] ", 1)
	result, changed, err := codec.ReconcileControls(tampered, Canonicalize(state), []Interaction{{SourceRefID: "src_human", Human: true, At: time.Now()}}, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("ReconcileControls() error = %v", err)
	}
	if changed || !reflect.DeepEqual(result, Canonicalize(state)) {
		t.Fatal("inserted checkbox changed an AI acknowledgement")
	}
}

func TestStructuralParsingIgnoresUnrelatedQuotedAndCodeTasks(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	state := sampleState()
	state.Findings = state.Findings[:1]
	source, err := codec.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	marker := findingMarker(state.Findings[0].ID)
	noise := strings.Join([]string{
		"- [x] unrelated task",
		"> - [x] copied marker " + marker,
		"```markdown",
		"- [x] example " + marker,
		"```",
		"",
	}, "\n")
	noisy := strings.Replace(source, "\n"+statePrefix, "\n"+noise+statePrefix, 1)
	decoded, err := codec.Decode(noisy)
	if err != nil {
		t.Fatalf("Decode(noisy report) error = %v", err)
	}
	result, changed, err := codec.ReconcileControls(noisy, decoded, nil, time.Now())
	if err != nil || changed || !reflect.DeepEqual(result, decoded) {
		t.Fatalf("noise affected controls: changed %v, err %v", changed, err)
	}
}

func TestRendererEscapesMarkersControlsHTMLAndQuickActions(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	state := sampleState()
	state.Findings = state.Findings[:1]
	state.Findings[0].Title = "</details>\n- [x] injected\n/approve\n<!-- ci-signal-state v=999 -->"
	state.Findings[0].Explanation = "<script>alert(1)</script>\n/merge"
	state.Findings[0].Evidence[0].Locations[0].Path = "x.md\n- [x] second"

	source, err := codec.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(source, statePrefix) != 1 {
		t.Fatalf("untrusted marker escaped incorrectly:\n%s", source)
	}
	for _, unsafe := range []string{"<script>", "\n/approve", "\n/merge", "\n- [x] injected", "\n- [x] second"} {
		if strings.Contains(source, unsafe) {
			t.Fatalf("rendered unsafe text %q:\n%s", unsafe, source)
		}
	}
	if _, err := codec.Decode(source); err != nil {
		t.Fatalf("escaped report no longer decodes: %v", err)
	}
}

func TestDecodeOwnershipAndConflicts(t *testing.T) {
	t.Parallel()
	codec := NewCodec()
	source, err := codec.Encode(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	markerStart := strings.LastIndex(source, statePrefix)
	marker := strings.TrimSpace(source[markerStart:])

	tests := []struct {
		name   string
		source string
		target error
	}{
		{name: "unrelated", source: "ordinary developer note", target: ErrNotOwned},
		{name: "malformed", source: statePrefix + " v=1", target: ErrConflict},
		{name: "duplicate", source: source + "\n" + marker, target: ErrConflict},
		{name: "unknown version", source: strings.Replace(source, " v=1 sha256=", " v=2 sha256=", 1), target: ErrConflict},
		{name: "checksum", source: strings.Replace(source, "sha256=", "sha256=0", 1), target: ErrConflict},
		{name: "missing finding", source: strings.Replace(source, findingMarker("f_open"), "", 1), target: ErrConflict},
		{name: "foreign finding", source: strings.Replace(source, findingMarker("f_open"), findingMarker("f_foreign"), 1), target: ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := codec.Decode(test.source)
			if !errors.Is(err, test.target) {
				t.Fatalf("Decode() error = %v, want errors.Is(%v)", err, test.target)
			}
		})
	}
}

func TestSampleMarkdown(t *testing.T) {
	t.Parallel()
	got, err := NewCodec().Encode(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	path := "testdata/sample.md"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatal("sample Markdown does not match codec output; run with UPDATE_GOLDEN=1")
	}
}

func sampleState() review.State {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.FixedZone("fixture", 2*60*60))
	return review.State{
		SchemaVersion: 1,
		MR:            review.MRIdentity{BaseURL: "https://gitlab.example", Project: "payments/api", MRIID: 42},
		Snapshot:      review.Snapshot{ID: "snap_1", BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40), SourceTree: strings.Repeat("c", 40), CapturedAt: at},
		Fingerprint:   "fp_1",
		Findings: []review.Finding{
			{
				ID: "f_open", IdentityKey: "identity-open", Category: review.CategoryBlocker, Subcategory: review.SubcategoryCorrections,
				Relationship: review.RelationshipIntroduced, Title: "Cross-file amount regression", Explanation: "The caller now drops minor units.",
				Evidence:      []review.Evidence{{Explanation: "The conversion passes a decimal into an integer API.", SourceRefs: []review.SourceReferenceID{"src_1"}, Locations: []review.Location{{Path: "internal/payments/convert.go", StartLine: 24, EndLine: 27}}}},
				AssignedUnits: []review.ReviewUnitID{"unit_1"}, Assessment: review.AssessmentPresent, State: review.FindingOpen,
				History: []review.FindingEvent{{Kind: review.FindingEventCreated, At: at, RunID: "run_1", Assessment: review.AssessmentPresent}}, FirstSeenAt: at, LastSeenAt: at,
			},
			{
				ID: "f_human", IdentityKey: "identity-human", Category: review.CategoryRisk, Subcategory: review.SubcategoryOperational,
				Relationship: review.RelationshipPreExisting, Title: "Retry limit is low", Explanation: "The team accepted this operating limit.",
				Evidence: []review.Evidence{}, AssignedUnits: []review.ReviewUnitID{"unit_2"}, Assessment: review.AssessmentPresent, State: review.FindingAcknowledged,
				Acknowledgement: &review.Acknowledgement{Method: review.AcknowledgementCheckbox, SourceRefs: []review.SourceReferenceID{"src_note"}, At: at},
				History:         []review.FindingEvent{{Kind: review.FindingEventAcknowledged, At: at, Method: review.AcknowledgementCheckbox, SourceRefs: []review.SourceReferenceID{"src_note"}}}, FirstSeenAt: at, LastSeenAt: at,
			},
			{
				ID: "f_ai", IdentityKey: "identity-ai", Category: review.CategoryInfo, Subcategory: review.SubcategoryInfo,
				Relationship: review.RelationshipUnknown, Title: "Migration context", Explanation: "A later code change addressed the original observation.",
				Evidence: []review.Evidence{}, AssignedUnits: []review.ReviewUnitID{"unit_3"}, Assessment: review.AssessmentAddressed, State: review.FindingAcknowledged,
				Acknowledgement: &review.Acknowledgement{Method: review.AcknowledgementAICodeChange, SourceRefs: []review.SourceReferenceID{"src_diff"}, At: at},
				History:         []review.FindingEvent{{Kind: review.FindingEventAcknowledged, At: at, Method: review.AcknowledgementAICodeChange, SourceRefs: []review.SourceReferenceID{"src_diff"}}}, FirstSeenAt: at, LastSeenAt: at,
			},
		},
		Runs:        []review.Run{{ID: "run_1", Fingerprint: "fp_1", StartedAt: at, CompletedAt: at.Add(time.Minute), Completion: review.SubmissionComplete, Coverage: []review.UnitCoverage{{UnitID: "unit_1", Outcome: review.CoverageComplete}, {UnitID: "unit_2", Outcome: review.CoverageComplete}, {UnitID: "unit_3", Outcome: review.CoverageExcludedByPolicy}}}},
		Publication: &review.Publication{Generation: "pub_1", NoteID: 99, PredecessorNoteID: 88, StateDigest: "state-digest", PublishedAt: at},
	}
}

func assertOrder(t *testing.T, source string, values ...string) {
	t.Helper()
	position := -1
	for _, value := range values {
		next := strings.Index(source, value)
		if next < 0 || next <= position {
			t.Fatalf("%q missing or out of order in:\n%s", value, source)
		}
		position = next
	}
}

func TestLegacyPublicationRendererMatchesOriginalGolden(t *testing.T) {
	body, err := NewCodec().EncodeLegacyPublication(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	// Original sample.md at d8221dbe6e7c5bacaedeb27eebd83317146ad5c0.
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(body))); got != "cd2cc519b0d3f8e0c5bdd3f1d7a5092e8d6aa07cfaea8ce97009704d85bb183e" {
		t.Fatalf("historical renderer changed: %s", got)
	}
}
