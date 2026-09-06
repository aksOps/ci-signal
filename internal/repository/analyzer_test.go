package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"ci-signal/internal/review"
)

func TestGitCommandsAllowValidatedRepositoryWithDifferentOwner(t *testing.T) {
	fixture := newGitFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git")
	content := fmt.Sprintf("#!/bin/sh\nexport GIT_TEST_ASSUME_DIFFERENT_OWNER=1\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(wrapper, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	rule := filepath.Join(t.TempDir(), "rule.yml")
	if err := os.WriteFile(rule, []byte("id: unused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	if err := os.Symlink(fixture.dir, repositoryPath); err != nil {
		t.Fatal(err)
	}
	analyzer, err := NewAnalyzer(Options{
		RepositoryDir: repositoryPath, GitPath: wrapper, ASTGrepPath: "unused", GoRulePath: rule,
		MaxMetadataBytes: 4096, MaxASTSourceBytes: 4096, MaxASTOutputBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(fixture.dir) + "\n"

	t.Run("buffered command", func(t *testing.T) {
		got, err := analyzer.runGit(context.Background(), 4096, "rev-parse", "--show-toplevel")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("repository root = %q, want %q", got, want)
		}
	})
	t.Run("chunked command", func(t *testing.T) {
		got, err := analyzer.runGitChunk(context.Background(), 0, 4096, "rev-parse", "--show-toplevel")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Data) != want || !got.Complete {
			t.Fatalf("repository root chunk = %#v, want %q", got, want)
		}
	})
}

func TestSnapshotInventoryAndPinnedRetrieval(t *testing.T) {
	fixture := newGitFixture(t)
	analyzer := newTestAnalyzer(t, fixture.dir, 2*1024*1024)
	ctx := context.Background()
	if err := analyzer.VerifyTools(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := analyzer.CaptureSnapshot(ctx, fixture.base, fixture.head, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CheckedOutIsSourceHead || snapshot.CheckedOutCommit != fixture.checkout {
		t.Fatalf("checkout/source-head distinction lost: %#v", snapshot)
	}

	source := readAllChunks(t, func(offset int64) (Chunk, error) {
		result, err := analyzer.ReadSource(ctx, snapshot, SideHead, "pkg/service.go", offset, 23)
		if err == nil && result.Reference.ID == "" {
			t.Fatal("source result omitted host reference")
		}
		return result.Chunk, err
	})
	if !bytes.Contains(source, []byte("return 2")) || bytes.Contains(source, []byte("UNTRUSTED_WORKTREE")) {
		t.Fatalf("head source did not come from pinned object: %q", source)
	}
	diffFirst, err := analyzer.ReadDiff(ctx, snapshot, "pkg/service.go", 0, 40)
	if err != nil {
		t.Fatal(err)
	}
	if diffFirst.Chunk.Complete || diffFirst.Chunk.NextOffset == 0 || diffFirst.Reference.Kind != review.SourceRepositoryDiff {
		t.Fatalf("bounded diff omitted continuation: %#v", diffFirst)
	}

	search := readAllChunks(t, func(offset int64) (Chunk, error) {
		result, err := analyzer.Search(ctx, snapshot, SideHead, "SharedValue", nil, offset, 17)
		if err == nil && result.Reference.Kind != review.SourceRepositorySearch {
			t.Fatalf("search source kind = %q", result.Reference.Kind)
		}
		return result.Chunk, err
	})
	if !bytes.Contains(search, []byte("pkg/caller.go")) || !bytes.Contains(search, []byte("pkg/new.go")) {
		t.Fatalf("cross-file search = %q", search)
	}

	inventory, err := analyzer.Inventory(ctx, snapshot, review.ScopeMRImpact)
	if err != nil {
		t.Fatal(err)
	}
	assertChange(t, inventory.Changes, ChangeAdded, "", "pkg/new.go")
	assertChange(t, inventory.Changes, ChangeDeleted, "pkg/delete.go", "")
	assertChange(t, inventory.Changes, ChangeRenamed, "docs/readme.md", "docs/guide.md")
	assertUnit(t, inventory.Units, "Removed", true, false)
	assertUnit(t, inventory.Units, "Added", false, true)
	assertUnit(t, inventory.Units, "Run", true, true)
	assertExclusion(t, inventory.Exclusions, "docs/guide.md", ExclusionUnsupportedStructure, false)
	assertExclusion(t, inventory.Exclusions, "vendor/dependency.go", ExclusionVendor, true)
	assertExclusion(t, inventory.Exclusions, "api.generated.go", ExclusionGenerated, true)
	assertExclusion(t, inventory.Exclusions, "asset.bin", ExclusionBinary, true)

	full, err := analyzer.Inventory(ctx, snapshot, review.ScopeFullProject)
	if err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	for _, unit := range full.Units {
		if unit.Kind == UnitFile && unit.Head != nil && unit.Head.Path == "pkg/caller.go" && unit.Change == ChangeUnchanged {
			unchanged = true
		}
	}
	if !unchanged {
		t.Fatal("full_project inventory omitted unchanged eligible file")
	}
}

func TestGuidanceMaterializationUsesHeadObjectsAndRejectsSymlinks(t *testing.T) {
	fixture := newGitFixture(t)
	analyzer := newTestAnalyzer(t, fixture.dir, 2*1024*1024)
	ctx := context.Background()
	snapshot, err := analyzer.CaptureSnapshot(ctx, fixture.base, fixture.head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "runtime")
	guidance, err := analyzer.MaterializeGuidance(ctx, snapshot, destination, 50, 128*1024)
	if err != nil {
		t.Fatal(err)
	}
	if guidance.RuntimeDirectory != destination || len(guidance.InstructionFiles) < 2 || len(guidance.SkillDirectories) != 1 {
		t.Fatalf("guidance snapshot = %#v", guidance)
	}
	content, err := os.ReadFile(filepath.Join(destination, "AGENTS.md"))
	if err != nil || !bytes.Contains(content, []byte("root guidance")) {
		t.Fatalf("materialized AGENTS.md = %q, %v", content, err)
	}
	for _, file := range guidance.Files {
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("guidance file is not read-only regular content: %s %#v %v", file, info, err)
		}
	}

	unsafeSnapshot, err := analyzer.CaptureSnapshot(ctx, fixture.head, fixture.checkout, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = analyzer.MaterializeGuidance(ctx, unsafeSnapshot, filepath.Join(t.TempDir(), "unsafe"), 50, 128*1024)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink guidance error = %v", err)
	}
}

func TestMissingShallowHistoryIsExplicit(t *testing.T) {
	fixture := newGitFixture(t)
	clone := filepath.Join(t.TempDir(), "shallow")
	runTestGit(t, "", "clone", "--depth=1", "file://"+fixture.dir, clone)
	analyzer := newTestAnalyzer(t, clone, 2*1024*1024)
	_, err := analyzer.CaptureSnapshot(context.Background(), fixture.base, fixture.checkout, time.Now())
	if err == nil || !errors.Is(err, ErrMissingHistory) || !strings.Contains(err.Error(), "shallow=true") {
		t.Fatalf("CaptureSnapshot() error = %v", err)
	}
}

func TestExtractionLimitFallsBackToEnclosingUnit(t *testing.T) {
	fixture := newGitFixture(t)
	analyzer := newTestAnalyzer(t, fixture.dir, 32)
	snapshot, err := analyzer.CaptureSnapshot(context.Background(), fixture.base, fixture.head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := analyzer.Inventory(context.Background(), snapshot, review.ScopeMRImpact)
	if err != nil {
		t.Fatal(err)
	}
	var oversized Unit
	for _, unit := range inventory.Units {
		if unit.Kind == UnitFile && unit.Head != nil && unit.Head.Path == "pkg/service.go" {
			if !unit.Fallback || !unit.Oversized || !unit.NeedsEnclosing {
				t.Fatalf("oversized file fallback = %#v", unit)
			}
			oversized = unit
			break
		}
	}
	if oversized.ID == "" {
		t.Fatal("service fallback unit missing")
	}
	requirements, err := inventory.UnitRequirements()
	if err != nil {
		t.Fatal(err)
	}
	requirement, ok := requirements[oversized.ID]
	if !ok || requirement.RetrievalComplete || requirement.EnclosingUnitID == "" {
		t.Fatalf("oversized coverage requirement = %#v, present=%v", requirement, ok)
	}
}

func TestLiteralPathsRejectTraversal(t *testing.T) {
	for _, value := range []string{"../secret", "/absolute", "a/../secret", "a\\secret", ""} {
		if _, err := literalPath(value); err == nil {
			t.Fatalf("literalPath(%q) succeeded", value)
		}
	}
}

func TestRepositoryEvidenceCanBeValidatedWithoutExpandingCoverage(t *testing.T) {
	fixture := newGitFixture(t)
	analyzer := newTestAnalyzer(t, fixture.dir, 2*1024*1024)
	snapshot, err := analyzer.CaptureSnapshot(context.Background(), fixture.base, fixture.head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := analyzer.ReadSource(context.Background(), snapshot, SideHead, "pkg/caller.go", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	assignment := review.Assignment{
		UnitIDs:    map[review.ReviewUnitID]struct{}{"assigned-unit": {}},
		Findings:   map[review.FindingID]review.KnownFinding{},
		SourceRefs: map[review.SourceReferenceID]review.SourceReference{evidence.Reference.ID: evidence.Reference},
	}
	raw := []byte(fmt.Sprintf(`{
		"verdict":"needs_review","completion":"complete",
		"findings":[{"category":"risk","subcategory":"corrections","relationship":"introduced","title":"Cross-file regression","explanation":"The assigned change breaks its caller.","evidence":[{"explanation":"Pinned caller source uses the changed contract.","source_refs":[%q]}],"assigned_units":["assigned-unit"]}],
		"reassessments":[],"acknowledgement_changes":[],
		"coverage":[{"unit_id":"assigned-unit","outcome":"complete"}],"limitations":[]
	}`, evidence.Reference.ID))
	validator, err := review.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(raw, assignment); err != nil {
		t.Fatalf("repository-wide source reference was rejected: %v", err)
	}
}

func TestCodeChangeAcknowledgementUsesFreshSnapshotEvidence(t *testing.T) {
	fixture := newGitFixture(t)
	analyzer := newTestAnalyzer(t, fixture.dir, 2*1024*1024)
	ctx := context.Background()
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	read := func(head string, capturedAt time.Time) review.SourceReference {
		t.Helper()
		snapshot, err := analyzer.CaptureSnapshot(ctx, fixture.base, head, capturedAt)
		if err != nil {
			t.Fatal(err)
		}
		evidence, err := analyzer.ReadSource(ctx, snapshot, SideHead, "pkg/service.go", 0, 4096)
		if err != nil {
			t.Fatal(err)
		}
		return evidence.Reference
	}
	old := read(fixture.base, at)
	repeated := read(fixture.base, at.Add(time.Hour))
	fixed := read(fixture.head, at.Add(2*time.Hour))
	if repeated.ID != old.ID {
		t.Fatal("recapturing unchanged code minted a new source reference")
	}
	if fixed.ID == old.ID || fixed.Commit == old.Commit {
		t.Fatal("changed commit did not produce fresh source evidence")
	}
	assignment := review.Assignment{
		UnitIDs:    map[review.ReviewUnitID]struct{}{"unit-1": {}},
		Findings:   map[review.FindingID]review.KnownFinding{"finding-1": {ID: "finding-1", State: review.FindingOpen, ConsumedSourceRefs: map[review.SourceReferenceID]struct{}{old.ID: {}}}},
		SourceRefs: map[review.SourceReferenceID]review.SourceReference{old.ID: old, fixed.ID: fixed},
	}
	validator, err := review.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []review.SourceReference{repeated, fixed} {
		raw := []byte(fmt.Sprintf(`{
			"verdict":"approved","completion":"complete","findings":[],
			"reassessments":[{"finding_id":"finding-1","assessment":"addressed","explanation":"The implementation is corrected.","evidence":[{"explanation":"Pinned implementation.","source_refs":[%q]}],"assigned_units":["unit-1"]}],
			"acknowledgement_changes":[{"finding_id":"finding-1","action":"acknowledge","method":"ai_code_change","explanation":"The code addresses the finding.","source_refs":[%q]}],
			"coverage":[{"unit_id":"unit-1","outcome":"complete"}],"limitations":[]
		}`, source.ID, source.ID))
		submission, err := validator.Validate(raw, assignment)
		if source.ID == old.ID {
			if err == nil || !strings.Contains(err.Error(), "already consumed") {
				t.Fatalf("old evidence validation error = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("fresh fixed-snapshot evidence rejected: %v", err)
		}
		prior := review.Finding{ID: "finding-1", State: review.FindingOpen, History: []review.FindingEvent{
			{Kind: review.FindingEventAcknowledged, Method: review.AcknowledgementCheckbox, At: at},
			{Kind: review.FindingEventReopened, Method: review.AcknowledgementCheckbox, At: at.Add(time.Hour)},
		}}
		findings, err := review.NewReconciler().Reconcile([]review.Finding{prior}, submission, review.ReconcileMetadata{RunID: "fixed", At: at.Add(2 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		finding := findings[0]
		if finding.State != review.FindingAcknowledged || finding.Assessment != review.AssessmentAddressed || finding.Acknowledgement == nil || finding.Acknowledgement.Method != review.AcknowledgementAICodeChange || finding.Acknowledgement.SourceRefs[0] != fixed.ID || len(finding.History) != 4 || finding.History[1].Kind != review.FindingEventReopened {
			t.Fatalf("fresh acknowledgement lost state or human history: %#v", finding)
		}
	}
}

func TestConfiguredStructuralScanCoversFullProjectWithoutExpandingMRImpactCoverage(t *testing.T) {
	fixture := newGitFixture(t)
	analyzer := newTestAnalyzer(t, fixture.dir, 2*1024*1024)
	snapshot, err := analyzer.CaptureSnapshot(context.Background(), fixture.base, fixture.head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := analyzer.Inventory(context.Background(), snapshot, review.ScopeMRImpact)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range inventory.Units {
		if unit.Head != nil && unit.Head.Path == "pkg/caller.go" {
			t.Fatal("unchanged caller unexpectedly entered mr_impact coverage")
		}
	}
	scan, err := analyzer.RunStructuralScan(context.Background(), snapshot, "all-go-declarations", "go", analyzer.goRulePath, "")
	if err != nil {
		t.Fatal(err)
	}
	foundUnchanged := false
	for _, match := range scan.Matches {
		if match.Path == "pkg/caller.go" {
			foundUnchanged = true
		}
	}
	if !scan.Complete || scan.Reference.Kind != review.SourceRepositoryAST || !foundUnchanged {
		t.Fatalf("structural scan = %#v", scan)
	}
}

func readAllChunks(t *testing.T, read func(int64) (Chunk, error)) []byte {
	t.Helper()
	var result []byte
	var offset int64
	for attempts := 0; attempts < 100; attempts++ {
		chunk, err := read(offset)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, chunk.Data...)
		if chunk.Complete {
			return result
		}
		if chunk.NextOffset <= offset {
			t.Fatalf("continuation did not advance: %#v", chunk)
		}
		offset = chunk.NextOffset
	}
	t.Fatal("chunk retrieval did not complete")
	return nil
}

func assertChange(t *testing.T, changes []FileChange, kind ChangeKind, basePath, headPath string) {
	t.Helper()
	for _, change := range changes {
		if change.Kind == kind && change.BasePath == basePath && change.HeadPath == headPath {
			return
		}
	}
	t.Fatalf("change %s %q -> %q missing from %#v", kind, basePath, headPath, changes)
}

func assertUnit(t *testing.T, units []Unit, symbol string, base, head bool) {
	t.Helper()
	for _, unit := range units {
		if unit.Symbol == symbol && (unit.Base != nil) == base && (unit.Head != nil) == head {
			return
		}
	}
	t.Fatalf("unit %q base=%t head=%t missing from %#v", symbol, base, head, units)
}

func assertExclusion(t *testing.T, exclusions []Exclusion, filePath string, reason ExclusionReason, excluded bool) {
	t.Helper()
	for _, exclusion := range exclusions {
		if exclusion.Path == filePath && exclusion.Reason == reason && exclusion.Excluded == excluded {
			return
		}
	}
	t.Fatalf("exclusion %q %s excluded=%t missing from %#v", filePath, reason, excluded, exclusions)
}

type gitFixture struct {
	dir      string
	base     string
	head     string
	checkout string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	directory := t.TempDir()
	runTestGit(t, "", "init", "--initial-branch=main", directory)
	writeFixture(t, directory, "AGENTS.md", "root guidance\n")
	writeFixture(t, directory, ".github/copilot-instructions.md", "copilot guidance\n")
	writeFixture(t, directory, ".github/instructions/pkg.instructions.md", "---\napplyTo: pkg/**\n---\npackage guidance\n")
	writeFixture(t, directory, ".agents/skills/project-check/SKILL.md", "# Project check\n")
	writeFixture(t, directory, "pkg/service.go", "package pkg\n\ntype Service struct{}\n\nfunc (Service) Run() int { return 1 }\n")
	writeFixture(t, directory, "pkg/delete.go", "package pkg\n\nfunc Removed() {}\n")
	writeFixture(t, directory, "pkg/caller.go", "package pkg\n\nconst SharedValue = 1\nfunc Caller() int { return SharedValue }\n")
	writeFixture(t, directory, "docs/readme.md", "documentation\n")
	writeFixture(t, directory, "vendor/dependency.go", "package vendor\n")
	writeFixture(t, directory, "api.generated.go", "package generated\n")
	runTestGit(t, directory, "add", "--", ".")
	runTestGit(t, directory, "commit", "-m", "base")
	base := gitOutput(t, directory, "rev-parse", "HEAD")

	writeFixture(t, directory, "pkg/service.go", "package pkg\n\ntype Service struct{}\n\nfunc (Service) Run() int { return 2 }\n")
	if err := os.Remove(filepath.Join(directory, "pkg/delete.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, "docs/readme.md"), filepath.Join(directory, "docs/guide.md")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, directory, "pkg/new.go", "package pkg\n\nfunc Added() int { return SharedValue }\n")
	writeFixture(t, directory, "vendor/dependency.go", "package vendor\n\nconst Changed = true\n")
	writeFixture(t, directory, "api.generated.go", "package generated\n\nconst Changed = true\n")
	writeFixtureBytes(t, directory, "asset.bin", []byte{'a', 0, 'b'})
	runTestGit(t, directory, "add", "--", ".")
	runTestGit(t, directory, "commit", "-m", "head")
	head := gitOutput(t, directory, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Join(directory, "bad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(directory, "bad", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, directory, "add", "--", "bad/AGENTS.md")
	runTestGit(t, directory, "commit", "-m", "merged checkout")
	checkout := gitOutput(t, directory, "rev-parse", "HEAD")
	writeFixture(t, directory, "pkg/service.go", "UNTRUSTED_WORKTREE\n")
	return gitFixture{dir: directory, base: base, head: head, checkout: checkout}
}

func newTestAnalyzer(t *testing.T, directory string, maxASTSource int64) *Analyzer {
	t.Helper()
	binary := os.Getenv("AST_GREP_BIN")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("ast-grep")
		if err != nil {
			t.Skip("ast-grep binary unavailable")
		}
	}
	version := strings.TrimSpace(commandOutput(t, "", binary, "--version"))
	if version != "ast-grep "+SupportedASTGrepVersion {
		t.Skipf("requires ast-grep %s, found %s", SupportedASTGrepVersion, version)
	}
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	rule := filepath.Join(filepath.Dir(testFile), "..", "..", "core", "skills", "ast-analysis", "rules", "go-declarations.yml")
	analyzer, err := NewAnalyzer(Options{
		RepositoryDir:     directory,
		GitPath:           "git",
		ASTGrepPath:       binary,
		GoRulePath:        rule,
		MaxMetadataBytes:  2 * 1024 * 1024,
		MaxASTSourceBytes: maxASTSource,
		MaxASTOutputBytes: 2 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return analyzer
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	writeFixtureBytes(t, root, name, []byte(content))
}

func writeFixtureBytes(t *testing.T, root, name string, content []byte) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTestGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	commandOutput(t, directory, "git", arguments...)
}

func gitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	return strings.TrimSpace(commandOutput(t, directory, "git", arguments...))
}

func commandOutput(t *testing.T, directory, executable string, arguments ...string) string {
	t.Helper()
	if executable == "git" {
		arguments = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, arguments...)
	}
	command := exec.Command(executable, arguments...)
	if directory != "" {
		command.Dir = directory
	}
	command.Env = safeChildEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", executable, strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func TestParseNameStatusRejectsMalformedData(t *testing.T) {
	_, err := parseNameStatus([]byte("R100\x00old.go\x00"))
	if err == nil {
		t.Fatal("malformed rename succeeded")
	}
	changes, err := parseNameStatus([]byte("A\x00new.go\x00D\x00old.go\x00R090\x00before.go\x00after.go\x00"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(changes, func(i, j int) bool { return fmt.Sprint(changes[i]) < fmt.Sprint(changes[j]) })
	if len(changes) != 3 {
		t.Fatalf("changes = %#v", changes)
	}
}
