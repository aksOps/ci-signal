package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestHelpExitsZeroWithoutConfiguration(t *testing.T) {
	var output bytes.Buffer
	code := run(context.Background(), []string{"--help"}, func(string) (string, bool) { return "", false }, &output, &output, nil)
	if code != 0 || !bytes.Contains(output.Bytes(), []byte("-log-tool-calls")) {
		t.Fatalf("help code=%d output=%q", code, output.String())
	}
}

func TestLoadIsIndependentOfLaunchDirectory(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "reviewer.json")
	document := `{
		"version":1,
		"gitlab":{"base_url":"https://gitlab.example.com","project":"group/project","mr_iid":7,"job_token_env":"CI_JOB_TOKEN","api_token_env":"GITLAB_API_TOKEN"},
		"repository":{"project_dir":"configured-project"},
		"copilot":{"provider":{"name":"ollama_cloud","type":"openai","endpoint":"https://ollama.com/v1","model":"deepseek-v4-flash:cloud","credential_env":"OLLAMA_API_KEY","wire_api":"responses"},"home_dir":"state/home","state_dir":"state/work"},
		"guidance":{"core_skill_dirs":["core/skills"],"core_instruction_dirs":["core/instructions"],"review_prompt_file":"core/review.md","required_skills":["review"],"reserved_skill_names":["review"]},
		"limits":{},"review":{},"tools":{},"permissions":{},"metadata":{},"labels":{},"diagnostics":{}
	}`
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, "source-head")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"REVIEWER_CONFIG_FILE": configPath, "REVIEW_PROJECT_DIR": projectDir, "CI_PROJECT_DIR": filepath.Join(root, "wrong"), "GITLAB_API_TOKEN": "api", "OLLAMA_API_KEY": "provider"}
		value, ok := values[name]
		return value, ok
	}
	launch := filepath.Join(root, "unrelated")
	if err := os.Mkdir(launch, 0o700); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(launch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	loaded, err := load([]string{"--log-level=debug", "--dry-run"}, lookup, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Repository.ProjectDir != projectDir {
		t.Fatalf("project_dir = %q", loaded.Config.Repository.ProjectDir)
	}
	if loaded.Config.Guidance.ReviewPromptFile != filepath.Join(configDir, "core/review.md") || loaded.Config.Copilot.StateDir != filepath.Join(configDir, "state/work") {
		t.Fatalf("relative paths not based on config: %#v", loaded.Config)
	}
	if !loaded.Config.Diagnostics.DryRun || loaded.Config.Diagnostics.LogLevel != "debug" {
		t.Fatal(fmt.Sprintf("CLI overrides absent: %#v", loaded.Config.Diagnostics))
	}
}

func TestPackagedExampleConfigLoadsFromUnrelatedDirectory(t *testing.T) {
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(packageDir, "..", ".."))
	configPath := filepath.Join(repositoryRoot, "examples", "reviewer.json")
	projectDir := filepath.Join(t.TempDir(), "source-head")
	lookup := func(name string) (string, bool) {
		values := map[string]string{
			"REVIEWER_CONFIG_FILE": configPath,
			"REVIEW_PROJECT_DIR":   projectDir,
			"OLLAMA_API_KEY":       "fixture-provider-token",
			"GITLAB_API_TOKEN":     "fixture-api-token",
			"CI_JOB_TOKEN":         "fixture-job-token",
		}
		value, ok := values[name]
		return value, ok
	}
	unrelated := t.TempDir()
	if err := os.Chdir(unrelated); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(packageDir) })
	loaded, err := load(nil, lookup, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	provider := loaded.Config.Copilot.Provider
	if provider.Name != "ollama_cloud" || provider.Type != "openai" || provider.WireAPI != "responses" || provider.Endpoint != "https://ollama.com/v1" || provider.Model != "deepseek-v4-flash:cloud" {
		t.Fatalf("provider = %#v", provider)
	}
	examplesDir := filepath.Dir(configPath)
	if loaded.Config.Guidance.CoreSkillDirs[0] != filepath.Clean(filepath.Join(examplesDir, "../core/skills")) || loaded.Config.Guidance.ReviewPromptFile != filepath.Clean(filepath.Join(examplesDir, "../core/prompts/review.md")) {
		t.Fatalf("packaged guidance paths = %#v", loaded.Config.Guidance)
	}
	if loaded.Config.Repository.ProjectDir != projectDir {
		t.Fatalf("project dir = %q", loaded.Config.Repository.ProjectDir)
	}
}
