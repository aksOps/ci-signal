package coordinator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/copilot"
	"ci-signal/internal/repository"
	"ci-signal/internal/review"
	sdk "github.com/github/copilot-sdk/go"
)

func TestRepositoryToolsReturnLosslessReadableChunks(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test"}, args...)...)
		command.Dir = dir
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet")
	sources := map[string][]byte{"source.go": []byte("package source\n// café\nfunc Amount() int { return 0 }\n"), "binary.dat": {0xff, 0x00, 0xfe}}
	for path, data := range sources {
		if err := os.WriteFile(filepath.Join(dir, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "fixture")
	base := git("rev-parse", "HEAD")
	sources["source.go"] = bytes.ReplaceAll(sources["source.go"], []byte("return 0"), []byte("return 1"))
	if err := os.WriteFile(filepath.Join(dir, "source.go"), sources["source.go"], 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "--quiet", "-m", "change")
	commit := git("rev-parse", "HEAD")
	astPath := os.Getenv("AST_GREP_BIN")
	if astPath == "" {
		astPath = "ast-grep"
	}
	rulePath, err := filepath.Abs("../../core/skills/ast-analysis/rules/go-declarations.yml")
	if err != nil {
		t.Fatal(err)
	}
	analyzer, err := repository.NewAnalyzer(repository.Options{RepositoryDir: dir, GitPath: "git", ASTGrepPath: astPath, GoRulePath: rulePath, MaxMetadataBytes: 4096, MaxASTSourceBytes: 4096, MaxASTOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := analyzer.CaptureSnapshot(context.Background(), base, commit, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := copilot.NewAssignmentRegistry(review.Assignment{UnitIDs: map[review.ReviewUnitID]struct{}{"unit-1": {}}, UnitRequirements: map[review.ReviewUnitID]review.UnitRequirement{"unit-1": {RetrievalComplete: false}}, Findings: map[review.FindingID]review.KnownFinding{}, SourceRefs: map[review.SourceReferenceID]review.SourceReference{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{4096, 1} {
		tools := repositoryTools(analyzer, snapshot, registry, config.Limits{MaxSourceBytes: limit, MaxDiffBytes: limit, MaxToolOutputBytes: limit}, []string{"repository_read", "git_read", "repository_search"}, nil)
		for _, tool := range tools {
			paths := []string{"source.go"}
			if tool.Name == "repository_read" {
				paths = append(paths, "binary.dat")
			}
			for _, path := range paths {
				var assembled []byte
				var offset int64
				for {
					args := map[string]any{"path": path, "offset": offset}
					if tool.Name != "git_read" {
						args["side"] = "head"
					}
					if tool.Name == "repository_search" {
						delete(args, "path")
						args["paths"] = []string{path}
						args["query"] = "café"
					}
					result, err := tool.Handler(sdk.ToolInvocation{Arguments: args, TraceContext: context.Background()})
					if err != nil {
						t.Fatalf("%s: %v", tool.Name, err)
					}
					var response struct {
						Chunk struct {
							Data, Encoding string
							Offset         int64
							NextOffset     int64 `json:"next_offset"`
							Complete       bool
						}
						Reference review.SourceReference `json:"source_reference"`
					}
					if err := json.Unmarshal([]byte(result.TextResultForLLM), &response); err != nil {
						t.Fatal(err)
					}
					data := []byte(response.Chunk.Data)
					switch response.Chunk.Encoding {
					case "utf-8":
					case "base64":
						data, err = base64.StdEncoding.DecodeString(response.Chunk.Data)
						if err != nil {
							t.Fatal(err)
						}
					default:
						t.Fatalf("%s has no explicit text encoding: %s", tool.Name, result.TextResultForLLM)
					}
					if len(data) > limit || response.Chunk.Offset != offset {
						t.Fatal("byte bound or offset changed")
					}
					if limit == 4096 && path == "source.go" && response.Chunk.Encoding != "utf-8" {
						t.Fatal("ordinary source was not readable UTF-8")
					}
					if _, ok := registry.Snapshot().SourceRefs[response.Reference.ID]; !ok {
						t.Fatal("source reference lost")
					}
					assembled = append(assembled, data...)
					if response.Chunk.Complete {
						break
					}
					if response.Chunk.NextOffset <= offset {
						t.Fatal("continuation did not advance")
					}
					offset = response.Chunk.NextOffset
				}
				if tool.Name == "repository_read" && !bytes.Equal(assembled, sources[path]) {
					t.Fatalf("source bytes changed: %q", assembled)
				}
				if tool.Name == "repository_search" && !bytes.Contains(assembled, []byte("café")) {
					t.Fatalf("search bytes changed: %q", assembled)
				}
				if tool.Name == "git_read" && (!bytes.Contains(assembled, []byte("-func Amount() int { return 0 }")) || !bytes.Contains(assembled, []byte("+func Amount() int { return 1 }"))) {
					t.Fatalf("diff bytes changed: %q", assembled)
				}
			}
		}
	}
	t.Run("structural scan returns locations without bodies", func(t *testing.T) {
		if _, err := exec.LookPath(astPath); err != nil {
			t.Skip("set AST_GREP_BIN to pinned ast-grep for native scan response check")
		}
		raw, err := analyzer.RunStructuralScan(context.Background(), snapshot, "declarations", "go", rulePath, "")
		if err != nil {
			t.Fatal(err)
		}
		tools := repositoryTools(analyzer, snapshot, registry, config.Limits{}, []string{"structural_scan"}, []config.StructuralScan{{Name: "declarations", Language: "go", RulePath: rulePath}})
		result, err := tools[0].Handler(sdk.ToolInvocation{Arguments: map[string]any{"name": "declarations"}, TraceContext: context.Background()})
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Matches []struct {
				Path         string
				StartLine    int `json:"start_line"`
				EndLine      int `json:"end_line"`
				MatchedBytes int `json:"matched_bytes"`
			}
			ContentOmitted bool `json:"content_omitted"`
			Complete       bool
			Reference      review.SourceReference `json:"source_reference"`
		}
		if err := json.Unmarshal([]byte(result.TextResultForLLM), &response); err != nil {
			t.Fatal(err)
		}
		if !response.ContentOmitted || strings.Contains(result.TextResultForLLM, "return 1") || strings.Contains(result.TextResultForLLM, `"text"`) {
			t.Fatalf("scan leaked source bodies: %s", result.TextResultForLLM)
		}
		if len(raw.Matches) == 0 || len(response.Matches) != len(raw.Matches) || response.Complete != raw.Complete || response.Reference.Kind != raw.Reference.Kind || response.Reference.Path != raw.Reference.Path || response.Reference.Commit != raw.Reference.Commit {
			t.Fatal("scan locations, completeness or evidence changed")
		}
		if registry.Snapshot().SourceRefs[response.Reference.ID] != response.Reference {
			t.Fatal("scan source reference was not preserved in the assignment")
		}
		for index, match := range response.Matches {
			want := raw.Matches[index]
			if match.Path != want.Path || match.StartLine != want.StartLine || match.EndLine != want.EndLine || match.MatchedBytes != len(want.Text) {
				t.Fatal("scan match metadata changed")
			}
		}
		if registry.Snapshot().UnitRequirements["unit-1"].RetrievalComplete {
			t.Fatal("location-only scan satisfied required body retrieval")
		}
	})
}
