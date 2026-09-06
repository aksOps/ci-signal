package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/review"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestEngineRunUsesPinnedBYOKAndAcceptsStructuredSubmission(t *testing.T) {
	fixture := newFixture(t)
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion}}
	engine := fixture.engine(t, factory, []sdk.Tool{{
		Name:        "repository_read",
		Description: "read pinned repository content",
		Handler: func(sdk.ToolInvocation) (sdk.ToolResult, error) {
			return sdk.ToolResult{TextResultForLLM: "content", ResultType: "success"}, nil
		},
	}})
	cliDirectory := t.TempDir()
	cliPath := filepath.Join(cliDirectory, "copilot-runtime")
	if err := os.WriteFile(cliPath, []byte("fixture runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDirectory, "runtime.node"), []byte("fixture node"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine.environment = append(engine.environment, "COPILOT_CLI_PATH="+cliPath)
	factory.runtime.session = &fakeSession{
		loadedSkills:  []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md"), Source: "custom"}},
		invokedSkills: []string{"core-review"},
		submissions:   []any{completeSubmission()},
		emitTelemetry: true,
	}

	result, err := engine.Run(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Accepted == nil || result.Accepted.Value.Completion != review.SubmissionComplete {
		t.Fatalf("accepted submission = %#v", result.Accepted)
	}
	if len(result.Telemetry.RequestedModels) != 1 || result.Telemetry.RequestedModels[0] != config.OllamaCloudModel {
		t.Fatalf("requested models = %v", result.Telemetry.RequestedModels)
	}
	if len(result.Telemetry.ObservedModels) != 1 || result.Telemetry.ObservedModels[0].Model != config.OllamaCloudModel {
		t.Fatalf("observed models = %v", result.Telemetry.ObservedModels)
	}
	if len(result.Telemetry.Usage) != 1 || result.Telemetry.Usage[0].TotalTokens != 15 || !result.Telemetry.UsageComplete {
		t.Fatalf("usage telemetry = %#v", result.Telemetry)
	}
	if len(result.Telemetry.Tools) != 1 || result.Telemetry.Tools[0].Tool != submitReviewTool || !result.Telemetry.Tools[0].Succeeded {
		t.Fatalf("tool telemetry = %#v", result.Telemetry.Tools)
	}
	if len(result.InvokedSkills) != 1 || result.InvokedSkills[0] != "core-review" {
		t.Fatalf("invoked skills = %v", result.InvokedSkills)
	}

	options := factory.options
	if options.Mode != sdk.ModeEmpty || options.WorkingDirectory != fixture.runtimeDir || options.BaseDirectory != fixture.config.Copilot.HomeDir {
		t.Fatalf("client options do not isolate cwd/home: %#v", options)
	}
	if options.UseLoggedInUser == nil || *options.UseLoggedInUser {
		t.Fatalf("UseLoggedInUser = %v", options.UseLoggedInUser)
	}
	stdio, ok := options.Connection.(sdk.StdioConnection)
	if !ok {
		t.Fatalf("connection = %T", options.Connection)
	}
	if stdio.Path != cliPath {
		t.Fatalf("runtime path = %q, want %q", stdio.Path, cliPath)
	}
	for _, entry := range stdio.Env {
		if strings.Contains(entry, "provider-secret") || strings.Contains(entry, "gitlab-secret") || strings.Contains(entry, "job-secret") || strings.HasPrefix(entry, "GITHUB_") || strings.HasPrefix(entry, "COPILOT_") {
			t.Fatalf("credential-bearing child environment entry survived: %q", entry)
		}
	}

	sessionConfig := factory.runtime.created
	if sessionConfig == nil {
		t.Fatal("session config was not captured")
	}
	if sessionConfig.WorkingDirectory != fixture.runtimeDir || sessionConfig.EnableConfigDiscovery == nil || *sessionConfig.EnableConfigDiscovery {
		t.Fatalf("session discovery/cwd = %#v", sessionConfig)
	}
	if sessionConfig.EnableOnDemandInstructionDiscovery == nil || *sessionConfig.EnableOnDemandInstructionDiscovery {
		t.Fatal("on-demand checkout instruction discovery was enabled")
	}
	provider := sessionConfig.Provider
	if provider == nil || provider.Type != "openai" || provider.WireAPI != "responses" || provider.BaseURL != config.OllamaCloudEndpoint || provider.ModelID != config.OllamaCloudModel || provider.WireModel != config.OllamaCloudModel || provider.BearerToken != "provider-secret" {
		t.Fatalf("provider config = %#v", provider)
	}
	if provider.MaxPromptTokens != 1000 || provider.MaxOutputTokens != 1000 {
		t.Fatal("positive provider token settings were lost")
	}
	if sessionConfig.Model != config.OllamaCloudModel || len(sessionConfig.Providers) != 0 || len(sessionConfig.Models) != 0 || sessionConfig.GitHubToken != "" {
		t.Fatalf("session could use an alternate provider or model: %#v", sessionConfig)
	}
	if len(sessionConfig.CustomAgents) != 1 || len(sessionConfig.CustomAgents[0].Skills) != 1 || sessionConfig.CustomAgents[0].Skills[0] != "core-review" {
		t.Fatalf("required skills were not explicitly preloaded: %#v", sessionConfig.CustomAgents)
	}
	if !contains(sessionConfig.AvailableTools, "custom:submit_review") || !contains(sessionConfig.AvailableTools, "custom:repository_read") {
		t.Fatalf("available tools = %v", sessionConfig.AvailableTools)
	}
	permission, err := sessionConfig.OnPermissionRequest(sdk.PermissionRequestCustomTool{ToolName: "submit_review"}, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := permission.(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Fatalf("submit_review permission = %T", permission)
	}
	permission, err = sessionConfig.OnPermissionRequest(&sdk.PermissionRequestCustomTool{ToolName: "submit_review"}, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := permission.(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Fatalf("SDK submit_review permission = %T", permission)
	}
	for _, request := range []sdk.PermissionRequest{
		&sdk.PermissionRequestCustomTool{ToolName: "custom:unknown"},
		&sdk.PermissionRequestShell{},
	} {
		permission, err = sessionConfig.OnPermissionRequest(request, sdk.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := permission.(*rpc.PermissionDecisionReject); !ok {
			t.Fatalf("permission for %T = %T, want rejection", request, permission)
		}
	}
}

func TestEngineRunRejectsMissingInvalidAndUninvokedSubmission(t *testing.T) {
	tests := []struct {
		name       string
		session    *fakeSession
		wantError  error
		wantAccept bool
	}{
		{
			name: "missing",
			session: &fakeSession{
				loadedSkills:  []skillInfo{{Name: "core-review", Path: "/trusted/core-review/SKILL.md"}},
				invokedSkills: []string{"core-review"},
			},
			wantError: ErrNoAcceptedSubmission,
		},
		{
			name: "invalid arguments exhaust corrections",
			session: &fakeSession{
				loadedSkills:  []skillInfo{{Name: "core-review", Path: "/trusted/core-review/SKILL.md"}},
				invokedSkills: []string{"core-review"},
				submissions:   []any{map[string]any{"verdict": "approved"}, map[string]any{"verdict": "approved"}},
			},
			wantError: ErrCorrectionBudgetExhausted,
		},
		{
			name: "submission before required skill invocation",
			session: &fakeSession{
				loadedSkills: []skillInfo{{Name: "core-review", Path: "/trusted/core-review/SKILL.md"}},
				submissions:  []any{completeSubmission()},
			},
			wantError: ErrRequiredSkillsNotInvoked,
		},
		{
			name: "partial assessment is accepted but remains partial",
			session: &fakeSession{
				loadedSkills:  []skillInfo{{Name: "core-review", Path: "/trusted/core-review/SKILL.md"}},
				invokedSkills: []string{"core-review"},
				submissions:   []any{partialSubmission()},
			},
			wantAccept: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			for i := range test.session.loadedSkills {
				if strings.HasPrefix(test.session.loadedSkills[i].Path, "/trusted/") {
					test.session.loadedSkills[i].Path = filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")
				}
			}
			factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: test.session}}
			engine := fixture.engine(t, factory, nil)
			result, err := engine.Run(context.Background(), fixture.request())
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("Run() error = %v, want %v", err, test.wantError)
			}
			if test.wantAccept {
				if err != nil || result.Accepted == nil || result.Accepted.Value.Completion != review.SubmissionPartial || result.Accepted.Value.Verdict != review.VerdictNeedsReview {
					t.Fatalf("partial result = %#v, error = %v", result, err)
				}
			}
		})
	}
}

func TestEngineRejectsMissingBYOKAndSubscriptionAuthentication(t *testing.T) {
	fixture := newFixture(t)
	validator, err := review.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := review.NewAcceptor(validator, fixture.config.Copilot.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newEngine(Options{Config: fixture.config, Acceptor: acceptor}, &fakeRuntimeFactory{})
	if err == nil || !strings.Contains(err.Error(), "BYOK credential") {
		t.Fatalf("New() error = %v", err)
	}

	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, subscriptionAuthenticated: true}}
	engine := fixture.engine(t, factory, nil)
	_, err = engine.Run(context.Background(), fixture.request())
	if err == nil || !strings.Contains(err.Error(), "subscription authentication is active") {
		t.Fatalf("Run() error = %v", err)
	}
	if factory.runtime.created != nil {
		t.Fatal("session was created after subscription authentication was detected")
	}
}

func TestEngineLogsSanitizedRuntimeFailure(t *testing.T) {
	fixture := newFixture(t)
	factory := &fakeRuntimeFactory{startErr: errors.New("authentication rejected provider-secret gitlab-secret job-secret")}
	engine := fixture.engine(t, factory, nil)

	stderr, capture, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	originalStderr := os.Stderr
	t.Cleanup(func() { os.Stderr = originalStderr })
	os.Stderr = capture
	_, err := engine.Run(context.Background(), fixture.request())
	os.Stderr = originalStderr
	if closeErr := capture.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "start pinned Copilot CLI") {
		t.Fatalf("Run() error = %v", err)
	}
	raw, readErr := io.ReadAll(stderr)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr := stderr.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	text := string(raw)
	if !strings.Contains(text, "start pinned Copilot CLI") || !strings.Contains(text, "authentication rejected [redacted]") {
		t.Fatalf("diagnostics omitted actionable runtime stage: %s", text)
	}
	for _, secret := range []string{"provider-secret", "gitlab-secret", "job-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, text)
		}
	}
}

func TestEngineProtectsExistingCopilotHome(t *testing.T) {
	fixture := newFixture(t)
	if err := os.MkdirAll(fixture.config.Copilot.HomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.config.Copilot.HomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, subscriptionAuthenticated: true}}
	engine := fixture.engine(t, factory, nil)
	_, _ = engine.Run(context.Background(), fixture.request())
	info, err := os.Stat(fixture.config.Copilot.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("Copilot home mode = %o", info.Mode().Perm())
	}
}

func TestEngineRejectsReservedProjectSkillCollision(t *testing.T) {
	fixture := newFixture(t)
	projectSkill := filepath.Join(fixture.projectSkills, "core-review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(projectSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSkill, []byte("---\nname: core-review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{loadedSkills: []skillInfo{{Name: "core-review", Path: projectSkill, Source: "custom"}}}
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
	engine := fixture.engine(t, factory, nil)
	_, err := engine.Run(context.Background(), fixture.request())
	if err == nil || !strings.Contains(err.Error(), "shadows a reserved core skill") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestEngineRejectsCIRepositoryAsRuntimeDirectory(t *testing.T) {
	fixture := newFixture(t)
	request := fixture.request()
	request.RuntimeDirectory = fixture.config.Repository.ProjectDir
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion}}
	engine := fixture.engine(t, factory, nil)
	_, err := engine.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "must not be the CI repository checkout") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestEngineTimeoutAbortsSession(t *testing.T) {
	fixture := newFixture(t)
	fixture.config.Limits.SessionTimeout = config.Duration(20 * time.Millisecond)
	session := &fakeSession{
		loadedSkills:  []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
		invokedSkills: []string{"core-review"},
		block:         true,
	}
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
	engine := fixture.engine(t, factory, nil)
	_, err := engine.Run(context.Background(), fixture.request())
	if err == nil || !strings.Contains(err.Error(), "session timeout") {
		t.Fatalf("Run() error = %v", err)
	}
	if !session.aborted {
		t.Fatal("timed out session was not aborted")
	}
}

func TestConfiguredReadOnlyMCPIsExposedAndPermissioned(t *testing.T) {
	fixture := newFixture(t)
	fixture.config.Tools.ExternalMCP = []config.MCPServer{{
		Name:      "safe",
		Transport: config.MCPHTTP,
		URL:       "https://mcp.example.test/rpc",
		EnvRefs:   map[string]string{"AUTHORIZATION": "MCP_AUTH"},
	}}
	fixture.config.Permissions.ExternalMCP = []string{"safe"}
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: &fakeSession{
		loadedSkills:  []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
		invokedSkills: []string{"core-review"},
		submissions:   []any{completeSubmission()},
	}}}
	engine := fixture.engineWithLookup(t, factory, nil, func(name string) (string, bool) {
		if name == "MCP_AUTH" {
			return "Bearer mcp-secret", true
		}
		return "", false
	})
	if _, err := engine.Run(context.Background(), fixture.request()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	server, ok := factory.runtime.created.MCPServers["safe"].(sdk.MCPHTTPServerConfig)
	if !ok || server.Headers["AUTHORIZATION"] != "Bearer mcp-secret" || !contains(factory.runtime.created.AvailableTools, "mcp:*") {
		t.Fatalf("configured MCP = %#v, tools = %v", server, factory.runtime.created.AvailableTools)
	}
	handler := factory.runtime.created.OnPermissionRequest
	approved, err := handler(sdk.PermissionRequestMCP{ServerName: "safe", ToolName: "lookup", ReadOnly: true}, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := approved.(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Fatalf("read-only MCP decision = %T", approved)
	}
	denied, err := handler(sdk.PermissionRequestMCP{ServerName: "safe", ToolName: "mutate", ReadOnly: false}, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := denied.(*rpc.PermissionDecisionReject); !ok {
		t.Fatalf("mutating MCP decision = %T", denied)
	}
	shell, err := handler(sdk.PermissionRequestShell{}, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shell.(*rpc.PermissionDecisionReject); !ok {
		t.Fatalf("shell decision = %T", shell)
	}
}

func TestMCPAliasOfProtectedCredentialIsRejected(t *testing.T) {
	fixture := newFixture(t)
	fixture.config.Tools.ExternalMCP = []config.MCPServer{{
		Name: "safe", Transport: config.MCPHTTP, URL: "https://mcp.example.test/rpc", EnvRefs: map[string]string{"AUTHORIZATION": "ALIAS"},
	}}
	fixture.config.Permissions.ExternalMCP = []string{"safe"}
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion}}
	engine := fixture.engineWithLookup(t, factory, nil, func(name string) (string, bool) {
		return "provider-secret", name == "ALIAS"
	})
	_, err := engine.Run(context.Background(), fixture.request())
	if err == nil || !strings.Contains(err.Error(), "aliases a protected credential") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestDiagnosticsRedactCredentialValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilot.jsonl")
	d, err := newDiagnostics(config.Diagnostics{LogFile: path, LogLevel: config.LogDebug}, config.Secrets{
		ProviderToken: config.Secret("provider-secret"),
		JobToken:      config.Secret("job-secret"),
		APIToken:      config.Secret("api-secret"),
	}, "mcp-secret")
	if err != nil {
		t.Fatal(err)
	}
	d.EventToolCall("repository_read", "call-1", map[string]any{"value": "provider-secret job-secret api-secret mcp-secret"})
	d.Reasoning("reason-1", "alias provider-secret")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"provider-secret", "job-secret", "api-secret", "mcp-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, text)
		}
	}
}

func TestDiagnosticsProtectExistingLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilot.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := newDiagnostics(config.Diagnostics{LogFile: path, LogLevel: config.LogInfo}, config.Secrets{})
	if err != nil {
		t.Fatal(err)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostics mode = %o", info.Mode().Perm())
	}
}

func TestTelemetryRejectsModelOrAuthenticationFallbackAndDeduplicatesUsage(t *testing.T) {
	fixture := newFixture(t)
	fixture.config.Limits.MaxInputTokens = 0
	fixture.config.Limits.MaxOutputTokens = 0
	t.Run("model mismatch", func(t *testing.T) {
		session := &fakeSession{
			loadedSkills:  []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
			invokedSkills: []string{"core-review"},
			submissions:   []any{completeSubmission()},
			observedModel: "other-model",
		}
		factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
		engine := fixture.engine(t, factory, nil)
		_, err := engine.Run(context.Background(), fixture.request())
		if err == nil || !strings.Contains(err.Error(), "instead of requested") {
			t.Fatalf("Run() error = %v", err)
		}
	})
	t.Run("model mismatch cancellation preserves integrity error", func(t *testing.T) {
		session := &fakeSession{
			loadedSkills:              []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
			invokedSkills:             []string{"core-review"},
			observedModel:             "other-model",
			telemetryBeforeSubmission: true,
		}
		factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
		engine := fixture.engine(t, factory, nil)
		result, err := engine.Run(context.Background(), fixture.request())
		if err == nil || !strings.Contains(err.Error(), "instead of requested") {
			t.Fatalf("Run() error = %v", err)
		}
		if len(result.Telemetry.ObservedModels) != 1 || result.Telemetry.ObservedModels[0].Model != "other-model" {
			t.Fatalf("telemetry = %#v", result.Telemetry)
		}
	})
	t.Run("non BYOK", func(t *testing.T) {
		falseValue := false
		session := &fakeSession{
			loadedSkills:  []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
			invokedSkills: []string{"core-review"},
			submissions:   []any{completeSubmission()},
			isBYOK:        &falseValue,
		}
		factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
		engine := fixture.engine(t, factory, nil)
		_, err := engine.Run(context.Background(), fixture.request())
		if err == nil || !strings.Contains(err.Error(), "non-BYOK") {
			t.Fatalf("Run() error = %v", err)
		}
	})
	t.Run("dedupe usage", func(t *testing.T) {
		session := &fakeSession{
			loadedSkills:   []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
			invokedSkills:  []string{"core-review"},
			submissions:    []any{completeSubmission()},
			emitTelemetry:  true,
			duplicateUsage: true,
		}
		factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
		engine := fixture.engine(t, factory, nil)
		result, err := engine.Run(context.Background(), fixture.request())
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Telemetry.Usage) != 1 {
			t.Fatalf("usage = %#v", result.Telemetry.Usage)
		}
	})
}

func TestDisabledTokenBudgetsOmitProviderCapsAndRetainTelemetry(t *testing.T) {
	fixture := newFixture(t)
	fixture.config.Limits.MaxInputTokens = 0
	fixture.config.Limits.MaxOutputTokens = 0
	session := &fakeSession{
		loadedSkills:  []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}},
		invokedSkills: []string{"core-review"}, submissions: []any{completeSubmission()},
		emitTelemetry: true, duplicateUsage: true, telemetryBeforeSubmission: true,
	}
	factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
	engine := fixture.engine(t, factory, nil)
	result, err := engine.Run(context.Background(), fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted == nil || !result.Telemetry.UsageComplete || len(result.Telemetry.Usage) != 1 || result.Telemetry.Usage[0].InputTokens != 10 || result.Telemetry.Usage[0].OutputTokens != 5 {
		t.Fatalf("accepted review or telemetry lost: %#v", result)
	}
	raw, err := json.Marshal(factory.runtime.created.Provider)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "maxPromptTokens") || strings.Contains(string(raw), "maxOutputTokens") {
		t.Fatal("disabled token caps were sent to provider")
	}
}

func TestTokenBudgetsEnforceOnlyPositiveCumulativeLimits(t *testing.T) {
	for _, test := range []struct {
		name          string
		input, output uint64
		wantFailure   bool
	}{
		{"disabled", 0, 0, false}, {"input", 15, 0, true}, {"output", 0, 7, true}, {"equal", 20, 10, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			canceled := false
			collector := newEventCollector("session", config.OllamaCloudModel, nil, config.Limits{MaxInputTokens: test.input, MaxOutputTokens: test.output}, config.Diagnostics{}, nil, func() { canceled = true })
			input, output, byok := int64(10), int64(5), true
			for _, id := range []string{"call1", "call1", "call2"} {
				collector.handle(sdk.SessionEvent{ID: id, Data: &sdk.AssistantUsageData{APICallID: &id, Model: config.OllamaCloudModel, IsByok: &byok, InputTokens: &input, OutputTokens: &output}})
			}
			if canceled != test.wantFailure || (collector.integrityError() != nil) != test.wantFailure {
				t.Fatalf("canceled=%v error=%v", canceled, collector.integrityError())
			}
			telemetry := collector.telemetry()
			if !telemetry.UsageComplete || len(telemetry.Usage) != 2 || collector.inputTokens != 20 || collector.outputTokens != 10 {
				t.Fatalf("usage was lost or duplicated: %#v", telemetry)
			}
		})
	}
}

type fixture struct {
	config        config.Config
	runtimeDir    string
	projectSkills string
	projectInstr  string
	coreSkills    string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	checkoutDir := filepath.Join(root, "checkout")
	runtimeDir := filepath.Join(root, "snapshot")
	projectSkills := filepath.Join(runtimeDir, ".agents", "skills")
	projectInstr := filepath.Join(runtimeDir, ".instructions")
	coreSkills := filepath.Join(root, "core-skills")
	coreInstructions := filepath.Join(root, "core-instructions")
	for _, path := range []string{checkoutDir, runtimeDir, projectSkills, projectInstr, filepath.Join(coreSkills, "core-review"), coreInstructions} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(coreSkills, "core-review", "SKILL.md"), []byte("---\nname: core-review\ndescription: review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(root, "review-prompt.md")
	if err := os.WriteFile(promptFile, []byte("Review the assigned units."), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture{
		config: config.Config{
			Version:    1,
			GitLab:     config.GitLab{BaseURL: "https://gitlab.example.test", Project: "group/project", MRIID: 1, JobTokenEnv: "CI_JOB_TOKEN", APITokenEnv: "GITLAB_API_TOKEN"},
			Repository: config.Repository{ProjectDir: checkoutDir},
			Copilot: config.Copilot{
				Provider: config.Provider{Name: config.ProviderOllamaCloud, Type: config.ProviderTypeOpenAI, Endpoint: config.OllamaCloudEndpoint, Model: config.OllamaCloudModel, CredentialEnv: "OLLAMA_API_KEY", WireAPI: config.WireAPIResponses},
				HomeDir:  filepath.Join(root, "copilot-home"), StateDir: filepath.Join(root, "state"),
			},
			Guidance: config.Guidance{
				CoreSkillDirs: []string{coreSkills}, CoreInstructionDirs: []string{coreInstructions}, ReviewPromptFile: promptFile,
				RequiredSkills: []string{"core-review"}, ReservedSkillNames: []string{"core-review"},
			},
			Limits: config.Limits{
				OverallTimeout: config.Duration(time.Minute), SessionTimeout: config.Duration(time.Second), ToolTimeout: config.Duration(time.Second), APITimeout: config.Duration(time.Second),
				MaxConcurrency: 1, MaxSessions: 1, MaxCorrectionAttempts: 2, MaxUnitsPerSession: 1,
				MaxSourceBytes: 1024, MaxDiffBytes: 1024, MaxToolOutputBytes: 4096, MaxReportBytes: 8192, MaxInputTokens: 1000, MaxOutputTokens: 1000,
			},
			Review:      reviewConfig(),
			Tools:       config.Tools{GitPath: "git", ASTGrepPath: "ast-grep"},
			Permissions: config.Permissions{Mode: config.PermissionReadOnly, NativeTools: []string{"repository_read", "submit_review"}},
			Diagnostics: config.Diagnostics{LogLevel: config.LogInfo},
		},
		runtimeDir: runtimeDir, projectSkills: projectSkills, projectInstr: projectInstr, coreSkills: coreSkills,
	}
}

func reviewConfig() config.Review {
	return config.Review{Scope: review.ScopeMRImpact, GatingCategories: []review.Category{review.CategoryBlocker, review.CategoryRisk, review.CategoryQuestion}, ExitPolicy: config.ExitOnIncomplete}
}

func (f fixture) engine(t *testing.T, factory runtimeFactory, tools []sdk.Tool) *Engine {
	t.Helper()
	return f.engineWithLookup(t, factory, tools, func(string) (string, bool) { return "", false })
}

func (f fixture) engineWithLookup(t *testing.T, factory runtimeFactory, tools []sdk.Tool, lookup config.LookupEnv) *Engine {
	t.Helper()
	validator, err := review.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := review.NewAcceptor(validator, f.config.Copilot.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newEngine(Options{
		Config:   f.config,
		Secrets:  config.Secrets{ProviderToken: config.Secret("provider-secret"), APIToken: config.Secret("gitlab-secret"), JobToken: config.Secret("job-secret")},
		Acceptor: acceptor, NativeTools: tools, LookupEnv: lookup,
		Environment: []string{"PATH=/usr/bin", "GITHUB_TOKEN=github-secret", "COPILOT_TOKEN=copilot-secret", "CI_JOB_TOKEN=job-secret", "GITLAB_API_TOKEN=gitlab-secret", "OLLAMA_API_KEY=provider-secret", "SECRET_ALIAS=provider-secret"},
	}, factory)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func (f fixture) request() RunRequest {
	registry, err := NewAssignmentRegistry(review.Assignment{UnitIDs: map[review.ReviewUnitID]struct{}{"unit-1": {}}, Findings: map[review.FindingID]review.KnownFinding{}, SourceRefs: map[review.SourceReferenceID]review.SourceReference{}})
	if err != nil {
		panic(err)
	}
	return RunRequest{
		SnapshotID: "snapshot-1", RuntimeDirectory: f.runtimeDir,
		InstructionDirectories: []string{f.projectInstr}, SkillDirectories: []string{f.projectSkills},
		Prompt:     "Inspect unit-1 using bounded repository tools.",
		Registry:   registry,
		Acceptance: review.AcceptanceMetadata{RunID: "run-1", SubmissionID: "submission-1", AcceptedAt: time.Unix(1, 0)},
	}
}

func completeSubmission() map[string]any {
	return map[string]any{
		"verdict": "approved", "completion": "complete", "findings": []any{}, "reassessments": []any{}, "acknowledgement_changes": []any{},
		"coverage": []any{map[string]any{"unit_id": "unit-1", "outcome": "complete"}}, "limitations": []any{},
	}
}

func partialSubmission() map[string]any {
	return map[string]any{
		"verdict": "needs_review", "completion": "partial", "findings": []any{}, "reassessments": []any{}, "acknowledgement_changes": []any{},
		"coverage": []any{map[string]any{"unit_id": "unit-1", "outcome": "partial", "explanation": "budget exhausted"}}, "limitations": []any{"unit incomplete"},
	}
}

type fakeRuntimeFactory struct {
	mu       sync.Mutex
	options  *sdk.ClientOptions
	runtime  *fakeRuntime
	startErr error
}

func (f *fakeRuntimeFactory) Start(_ context.Context, options *sdk.ClientOptions) (runtimeClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.options = options
	if f.startErr != nil {
		return nil, f.startErr
	}
	if f.runtime == nil {
		return nil, errors.New("fake runtime is not configured")
	}
	return f.runtime, nil
}

type fakeRuntime struct {
	version                   string
	subscriptionAuthenticated bool
	session                   *fakeSession
	created                   *sdk.SessionConfig
}

func (r *fakeRuntime) Version(context.Context) (string, error) { return r.version, nil }
func (r *fakeRuntime) SubscriptionAuthenticated(context.Context) (bool, error) {
	return r.subscriptionAuthenticated, nil
}
func (r *fakeRuntime) CreateSession(_ context.Context, config *sdk.SessionConfig) (runtimeSession, error) {
	r.created = config
	if r.session == nil {
		return nil, errors.New("fake session is not configured")
	}
	r.session.config = config
	return r.session, nil
}
func (r *fakeRuntime) Stop() error { return nil }

type fakeSession struct {
	config                     *sdk.SessionConfig
	loadedSkills               []skillInfo
	invokedSkills              []string
	submissions                []any
	emitTelemetry              bool
	duplicateUsage             bool
	observedModel              string
	isBYOK                     *bool
	telemetryBeforeSubmission  bool
	cancelMasksSubmissionError bool
	block                      bool
	aborted                    bool
}

func (s *fakeSession) LoadedSkills(context.Context) ([]skillInfo, error) {
	return append([]skillInfo(nil), s.loadedSkills...), nil
}
func (s *fakeSession) InvokedSkills(context.Context) ([]string, error) {
	return append([]string(nil), s.invokedSkills...), nil
}
func (s *fakeSession) SendAndWait(ctx context.Context, _ sdk.MessageOptions) (*sdk.SessionEvent, error) {
	for _, name := range s.invokedSkills {
		trigger := sdk.SkillInvokedTriggerContextLoad
		s.emit("skill-"+name, &sdk.SkillInvokedData{Name: name, Trigger: &trigger})
	}
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.telemetryBeforeSubmission {
		s.emitProviderTelemetry()
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for session.idle: %w", err)
		}
	}
	tool := s.tool(submitReviewTool)
	var lastErr error
	for i, arguments := range s.submissions {
		callID := "submit-" + string(rune('1'+i))
		s.emit("start-"+callID, &sdk.ToolExecutionStartData{ToolName: submitReviewTool, ToolCallID: callID, Arguments: arguments})
		_, lastErr = tool.Handler(sdk.ToolInvocation{SessionID: s.config.SessionID, ToolCallID: callID, ToolName: submitReviewTool, Arguments: arguments, TraceContext: ctx})
		s.emit("complete-"+callID, &sdk.ToolExecutionCompleteData{ToolCallID: callID, Success: lastErr == nil})
		if lastErr == nil {
			break
		}
	}
	if !s.telemetryBeforeSubmission {
		s.emitProviderTelemetry()
	}
	if s.cancelMasksSubmissionError && ctx.Err() != nil {
		return nil, fmt.Errorf("waiting for session.idle: %w", ctx.Err())
	}
	return &sdk.SessionEvent{}, lastErr
}

func (s *fakeSession) emitProviderTelemetry() {
	if !s.emitTelemetry && s.observedModel == "" && s.isBYOK == nil {
		return
	}
	model := s.observedModel
	if model == "" {
		model = config.OllamaCloudModel
	}
	input, output := int64(10), int64(5)
	byok := true
	if s.isBYOK != nil {
		byok = *s.isBYOK
	}
	callID := "provider-call-1"
	usage := &sdk.AssistantUsageData{APICallID: &callID, Model: model, IsByok: &byok, InputTokens: &input, OutputTokens: &output}
	s.emit("usage-1", usage)
	if s.duplicateUsage {
		s.emit("usage-2", usage)
	}
}
func (s *fakeSession) Abort(context.Context) error { s.aborted = true; return nil }
func (s *fakeSession) Disconnect() error           { return nil }
func (s *fakeSession) tool(name string) sdk.Tool {
	for _, tool := range s.config.Tools {
		if tool.Name == name {
			return tool
		}
	}
	panic("tool not configured: " + name)
}
func (s *fakeSession) emit(id string, data sdk.SessionEventData) {
	if s.config.OnEvent != nil {
		s.config.OnEvent(sdk.SessionEvent{ID: id, Data: data})
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestCorrectionExhaustionRetainsSafeRejectionWhenSDKReturnsCancellation(t *testing.T) {
	for _, test := range []struct {
		name, want string
		mutate     func(map[string]any)
	}{
		{"prose", "finding explanation: review text must be a prose summary without code blocks", func(value map[string]any) {
			value["findings"] = []any{map[string]any{"category": "risk", "subcategory": "corrections", "relationship": "introduced", "title": "Regression", "explanation": "```go\nprovider-secret\n```", "evidence": []any{map[string]any{"explanation": "Pinned implementation.", "source_refs": []any{"source-1"}}}, "assigned_units": []any{"unit-1"}}}
		}},
		{"schema", "schema validation", func(value map[string]any) { value["verdict"] = "provider-secret" }},
		{"foreign unit", "references foreign unit", func(value map[string]any) {
			value["coverage"] = []any{map[string]any{"unit_id": "provider-secret", "outcome": "complete"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			value := completeSubmission()
			test.mutate(value)
			first := completeSubmission()
			first["coverage"] = []any{}
			session := &fakeSession{loadedSkills: []skillInfo{{Name: "core-review", Path: filepath.Join(fixture.coreSkills, "core-review", "SKILL.md")}}, invokedSkills: []string{"core-review"}, submissions: []any{first, value}, cancelMasksSubmissionError: true}
			factory := &fakeRuntimeFactory{runtime: &fakeRuntime{version: CLIVersion, session: session}}
			_, err := fixture.engine(t, factory, nil).Run(context.Background(), fixture.request())
			if !errors.Is(err, ErrCorrectionBudgetExhausted) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("exhaustion omitted rejection reason: %v", err)
			}
			if strings.Contains(err.Error(), "provider-secret") || strings.Contains(err.Error(), "```go") {
				t.Fatal("exhaustion exposed rejected argument contents")
			}
		})
	}
}
