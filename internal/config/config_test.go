package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ci-signal/internal/review"
)

func TestLoadFromEnvironmentIsStrictAndSeparatesSecrets(t *testing.T) {
	configPath := writeConfig(t, validConfig(t))
	environment := map[string]string{
		"REVIEWER_CONFIG_FILE": configPath,
		"CI_PROJECT_DIR":       "/build/project",
		"GITLAB_API_TOKEN":     "api-secret",
		"CI_JOB_TOKEN":         "job-secret",
		"OLLAMA_API_KEY":       "provider-secret",
	}
	loaded, err := LoadFromEnvironment(mapLookup(environment))
	if err != nil {
		t.Fatalf("LoadFromEnvironment() error = %v", err)
	}
	if loaded.Config.Repository.ProjectDir != "/build/project" {
		t.Fatalf("project dir = %q", loaded.Config.Repository.ProjectDir)
	}
	if loaded.Secrets.ProviderToken.Value() != "provider-secret" || loaded.Secrets.ProviderToken.String() != "[redacted]" {
		t.Fatal("provider secret was not retained and redacted")
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"api-secret", "job-secret", "provider-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("serialized config contains secret %q", secret)
		}
	}
}

func TestLoadFromEnvironmentRejectsUnknownFieldsAndMissingCredential(t *testing.T) {
	configPath := writeConfig(t, validConfig(t))
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"REVIEWER_CONFIG_FILE": configPath, "GITLAB_API_TOKEN": "api", "OLLAMA_API_KEY": "provider"}
	if _, err := LoadFromEnvironment(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown-field error = %v", err)
	}

	configPath = writeConfig(t, validConfig(t))
	environment = map[string]string{"REVIEWER_CONFIG_FILE": configPath, "GITLAB_API_TOKEN": "api"}
	if _, err := LoadFromEnvironment(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "provider credential") {
		t.Fatalf("missing-provider error = %v", err)
	}
}

func TestLoadResolvesConfiguredPathsFromConfigDirectory(t *testing.T) {
	value := validConfig(t)
	value.Repository.ProjectDir = "project"
	value.Copilot.HomeDir = "state/home"
	value.Copilot.StateDir = "state"
	value.Guidance.CoreSkillDirs = []string{"core/skills"}
	value.Guidance.CoreInstructionDirs = []string{"core/instructions"}
	value.Guidance.ReviewPromptFile = "core/review.md"
	configPath := writeConfig(t, value)
	loaded, err := LoadFromEnvironment(mapLookup(map[string]string{
		"REVIEWER_CONFIG_FILE": configPath,
		"GITLAB_API_TOKEN":     "api",
		"OLLAMA_API_KEY":       "provider",
	}))
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(configPath)
	if loaded.Config.Repository.ProjectDir != filepath.Join(base, "project") || loaded.Config.Guidance.ReviewPromptFile != filepath.Join(base, "core/review.md") {
		t.Fatalf("relative paths were not config-relative: %#v", loaded.Config)
	}
}

func TestConfigRejectsProviderDriftAndLabelScopeConflicts(t *testing.T) {
	value := validConfig(t)
	value.applyDefaults()
	value.Copilot.Provider.Model = "another-model"
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "Ollama Cloud") {
		t.Fatalf("provider drift error = %v", err)
	}

	value = validConfig(t)
	value.applyDefaults()
	value.Labels.Static = []string{"ai-review::approved", "ai-review::needs-review"}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("label conflict error = %v", err)
	}
	if got := labelScope("ai-review::coverage::complete"); got != "ai-review::coverage" {
		t.Fatalf("labelScope() = %q", got)
	}
}

func TestConfigRejectsGitLabCredentialPassedToMCP(t *testing.T) {
	value := validConfig(t)
	value.Tools.ExternalMCP = []MCPServer{{Name: "unsafe", Transport: MCPStdio, Command: "/bin/tool", EnvRefs: map[string]string{"TOKEN": "GITLAB_API_TOKEN"}}}
	value.applyDefaults()
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("MCP credential error = %v", err)
	}
}

func TestConfigRejectsCredentialAliasesAndMissingPermissions(t *testing.T) {
	value := validConfig(t)
	value.GitLab.JobTokenEnv = value.GitLab.APITokenEnv
	value.applyDefaults()
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("credential alias error = %v", err)
	}

	value = validConfig(t)
	value.Permissions.NativeTools = []string{"repository_read"}
	value.Permissions.Mode = PermissionReadOnly
	value.applyDefaults()
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "submit_review") {
		t.Fatalf("permission error = %v", err)
	}
}

func TestConfigRequiresTrustedInputsForTeamAndMetadataMappings(t *testing.T) {
	value := validConfig(t)
	value.Labels.Dynamic = []LabelMapping{{Name: "team", Source: LabelFromTeam, Mode: LabelSingle, Values: map[string]string{"payments": "ai-review::team::payments"}}}
	value.applyDefaults()
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "metadata.team") {
		t.Fatalf("team input error = %v", err)
	}
}

func TestConfigRequiresRenderableTokenLabels(t *testing.T) {
	value := validConfig(t)
	value.Labels.Dynamic = []LabelMapping{{
		Name:        "tokens",
		Source:      LabelFromTokenUsage,
		Mode:        LabelSingle,
		TokenFormat: TokenLabelExact,
	}}
	value.applyDefaults()
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "requires a prefix") {
		t.Fatalf("exact token mapping error = %v", err)
	}

	value = validConfig(t)
	value.Labels.Dynamic = []LabelMapping{{
		Name:        "tokens",
		Source:      LabelFromTokenUsage,
		Mode:        LabelSingle,
		Missing:     MissingUnknown,
		TokenFormat: TokenLabelBuckets,
		TokenBuckets: []TokenBucket{
			{LessThan: 50_000, Label: "ai-review::tokens::under-50k"},
			{Label: "ai-review::tokens::50k-plus"},
		},
	}}
	value.applyDefaults()
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "unknown label") {
		t.Fatalf("bucket unknown mapping error = %v", err)
	}

	value.Labels.Dynamic[0].Values = map[string]string{"unknown": "ai-review::tokens::unknown"}
	if err := value.Validate(); err != nil {
		t.Fatalf("valid bucket mapping error = %v", err)
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		Version:    Version,
		GitLab:     GitLab{BaseURL: "https://gitlab.example.com", Project: "group/project", MRIID: 7, JobTokenEnv: "CI_JOB_TOKEN", APITokenEnv: "GITLAB_API_TOKEN"},
		Repository: Repository{ProjectDir: "/configured/project"},
		Copilot: Copilot{
			Provider: Provider{Name: ProviderOllamaCloud, Type: ProviderTypeOpenAI, Endpoint: OllamaCloudEndpoint, Model: OllamaCloudModel, CredentialEnv: "OLLAMA_API_KEY", WireAPI: WireAPIResponses},
			HomeDir:  filepath.Join(root, "copilot"),
			StateDir: filepath.Join(root, "state"),
		},
		Guidance: Guidance{
			CoreSkillDirs:       []string{filepath.Join(root, "skills")},
			CoreInstructionDirs: []string{filepath.Join(root, "instructions")},
			ReviewPromptFile:    filepath.Join(root, "review.md"),
			RequiredSkills:      []string{"review"},
			ReservedSkillNames:  []string{"review"},
		},
		Review:      Review{Scope: review.ScopeMRImpact},
		Diagnostics: Diagnostics{DryRun: true},
	}
}

func writeConfig(t *testing.T, value Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reviewer.json")
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestRepositoryExcludedPathsAreLiteralAndBounded(t *testing.T) {
	settings := validConfig(t)
	settings.applyDefaults()
	settings.Repository.ExcludedPaths = []string{"csharp/Program.cs", "custom-checks"}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", ".", "/", "/tmp/tests", "../tests", "src/../tests", "tests/", "C:/tests", "src\\tests", "tests\x00hidden"} {
		settings.Repository.ExcludedPaths = []string{invalid}
		if err := settings.Validate(); err == nil || !strings.Contains(err.Error(), "excluded_paths") {
			t.Errorf("invalid exclusion %q: %v", invalid, err)
		}
	}
}
