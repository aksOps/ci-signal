package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ci-signal/internal/config"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestPinnedCLIRuntimeContract(t *testing.T) {
	cliPath := os.Getenv("COPILOT_CLI_PATH")
	if cliPath == "" {
		t.Skip("set COPILOT_CLI_PATH to the verified Copilot CLI 1.0.83 runtime wrapper")
	}
	if info, err := os.Stat(cliPath); err != nil || info.IsDir() {
		t.Fatalf("COPILOT_CLI_PATH does not name a runtime executable: %v", err)
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	work := filepath.Join(root, "work")
	skillRoot := filepath.Join(root, "skills")
	skillDir := filepath.Join(skillRoot, "required-review")
	for _, directory := range []string{home, work, skillDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	skill := "---\nname: required-review\ndescription: Required fixture review skill\n---\n\n# Required Review\n\nCall submit_review exactly once with an empty object.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &responsesFixture{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: provider}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	client := sdk.NewClient(&sdk.ClientOptions{
		Connection: sdk.StdioConnection{
			Path: cliPath,
			Env: []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + home,
				"XDG_CONFIG_HOME=" + home,
				"XDG_STATE_HOME=" + home,
				"LANG=C.UTF-8",
			},
		},
		WorkingDirectory: work,
		BaseDirectory:    home,
		UseLoggedInUser:  sdk.Bool(false),
		Mode:             sdk.ModeEmpty,
		LogLevel:         "error",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start pinned CLI: %v", err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	status, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != CLIVersion {
		t.Fatalf("runtime version = %q, require %s", status.Version, CLIVersion)
	}
	auth, err := client.GetAuthStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if auth.IsAuthenticated {
		t.Fatal("runtime unexpectedly used stored Copilot authentication")
	}

	var eventMu sync.Mutex
	var order []string
	toolCalls := 0
	tool := sdk.Tool{
		Name:        submitReviewTool,
		Description: "Submit fixture review",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		IsTerminal:  true,
		Defer:       sdk.ToolDeferNever,
		Handler: func(sdk.ToolInvocation) (sdk.ToolResult, error) {
			eventMu.Lock()
			defer eventMu.Unlock()
			toolCalls++
			order = append(order, "handler")
			return sdk.ToolResult{TextResultForLLM: "accepted", ResultType: "success"}, nil
		},
	}
	session, err := client.CreateSession(ctx, &sdk.SessionConfig{
		SessionID:                          "ci-signal-runtime-fixture",
		ClientName:                         "ci-signal/runtime-fixture",
		Model:                              "fixture-model",
		WorkingDirectory:                   work,
		ConfigDirectory:                    home,
		EnableConfigDiscovery:              sdk.Bool(false),
		EnableOnDemandInstructionDiscovery: sdk.Bool(false),
		EnableHostGitOperations:            sdk.Bool(false),
		EnableSessionStore:                 sdk.Bool(false),
		EnableSkills:                       sdk.Bool(true),
		IncludedBuiltinSkills:              []string{},
		SkipCustomInstructions:             sdk.Bool(true),
		CustomAgentsLocalOnly:              sdk.Bool(true),
		EnableFileHooks:                    sdk.Bool(false),
		EnableFileChangeTracking:           sdk.Bool(false),
		Provider: &sdk.ProviderConfig{
			Type:            "openai",
			WireAPI:         "responses",
			BaseURL:         "http://" + listener.Addr().String() + "/v1",
			BearerToken:     "fixture-token",
			ModelID:         "fixture-model",
			WireModel:       "fixture-model",
			MaxPromptTokens: 4096,
			MaxOutputTokens: 128,
		},
		Tools:               []sdk.Tool{tool},
		AvailableTools:      sdk.NewToolSet().AddCustom(submitReviewTool).ToSlice(),
		OnPermissionRequest: sdk.PermissionHandler.ApproveAll,
		SkillDirectories:    []string{skillRoot},
		CustomAgents: []sdk.CustomAgentConfig{{
			Name:   "fixture-reviewer",
			Prompt: "Apply the required skill and call submit_review.",
			Skills: []string{"required-review"},
		}},
		Agent: "fixture-reviewer",
		OnEvent: func(event sdk.SessionEvent) {
			eventMu.Lock()
			defer eventMu.Unlock()
			switch event.Data.(type) {
			case *sdk.SkillInvokedData:
				order = append(order, "skill")
			case *sdk.ToolExecutionStartData:
				order = append(order, "tool_start")
			case *sdk.ToolExecutionCompleteData:
				order = append(order, "tool_complete")
			}
		},
	})
	if err != nil {
		t.Fatalf("create local BYOK session: %v", err)
	}
	t.Cleanup(func() { _ = session.Disconnect() })
	if _, err := session.RPC.Skills.EnsureLoaded(ctx); err != nil {
		t.Fatalf("load required skill: %v", err)
	}
	loaded, err := session.RPC.Skills.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsRuntimeSkill(loaded.Skills, "required-review") {
		t.Fatal("required skill was not loaded")
	}
	if _, err := session.SendAndWait(ctx, sdk.MessageOptions{Prompt: "Perform the fixture review now."}); err != nil {
		t.Fatalf("run local BYOK tool turn: %v", err)
	}

	eventMu.Lock()
	gotOrder := append([]string(nil), order...)
	gotToolCalls := toolCalls
	eventMu.Unlock()
	if gotToolCalls != 1 {
		t.Fatalf("submit_review calls = %d, want 1", gotToolCalls)
	}
	if indexOf(gotOrder, "skill") < 0 || indexOf(gotOrder, "skill") > indexOf(gotOrder, "handler") {
		t.Fatalf("required skill was not observed before submit_review: %v", gotOrder)
	}
	if indexOf(gotOrder, "tool_complete") < 0 {
		t.Fatalf("tool completion event missing: %v", gotOrder)
	}
	if calls, authorized, path := provider.result(); calls != 1 || !authorized || path != "/v1/responses" {
		t.Fatalf("provider calls=%d authorized=%v path=%q", calls, authorized, path)
	}
}

func TestEnginePinnedCLIRuntimeContract(t *testing.T) {
	cliPath := os.Getenv("COPILOT_CLI_PATH")
	if cliPath == "" {
		t.Skip("set COPILOT_CLI_PATH to the verified Copilot CLI 1.0.83 runtime wrapper")
	}
	if info, err := os.Stat(cliPath); err != nil || info.IsDir() {
		t.Fatalf("COPILOT_CLI_PATH does not name a runtime executable: %v", err)
	}

	arguments, err := json.Marshal(completeSubmission())
	if err != nil {
		t.Fatal(err)
	}
	provider := &responsesFixture{token: "provider-secret", arguments: string(arguments)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: provider}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	fixture := newFixture(t)
	fixture.config.Limits.SessionTimeout = config.Duration(30 * time.Second)
	fixture.config.Limits.MaxInputTokens = 4096
	engine := fixture.engine(t, sdkRuntimeFactory{}, nil)
	engine.config.Copilot.Provider.Endpoint = "http://" + listener.Addr().String() + "/v1"
	engine.config.Copilot.Provider.Model = "fixture-model"
	engine.environment = append(engine.environment, "COPILOT_CLI_PATH="+cliPath)

	result, err := engine.Run(context.Background(), fixture.request())
	if err != nil {
		calls, authorized, path := provider.result()
		t.Fatalf("run review through pinned CLI: %v (provider calls=%d authorized=%v path=%q)", err, calls, authorized, path)
	}
	if result.Accepted == nil || result.Accepted.Value.Completion != "complete" {
		t.Fatalf("accepted submission = %#v", result.Accepted)
	}
	if calls, authorized, path := provider.result(); calls != 1 || !authorized || path != "/v1/responses" {
		t.Fatalf("provider calls=%d authorized=%v path=%q", calls, authorized, path)
	}
}

type responsesFixture struct {
	mu         sync.Mutex
	calls      int
	authorized bool
	path       string
	token      string
	arguments  string
}

func (s *responsesFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 2<<20))
	s.mu.Lock()
	s.calls++
	token := s.token
	if token == "" {
		token = "fixture-token"
	}
	s.authorized = request.Header.Get("Authorization") == "Bearer "+token
	s.path = request.URL.Path
	s.mu.Unlock()
	arguments := s.arguments
	if arguments == "" {
		arguments = "{}"
	}
	encodedArguments, _ := json.Marshal(arguments)
	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, `{"id":"resp_1","object":"response","created_at":%d,"status":"completed","error":null,"incomplete_details":null,"instructions":null,"max_output_tokens":128,"model":"fixture-model","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"submit_review","arguments":%s,"status":"completed"}],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"store":false,"temperature":1,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":1,"truncation":"disabled","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":15},"user":null,"metadata":{}}`, time.Now().Unix(), encodedArguments)
}

func (s *responsesFixture) result() (int, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.authorized, s.path
}

func containsRuntimeSkill(skills []rpc.Skill, name string) bool {
	for _, skill := range skills {
		if skill.Name == name {
			return true
		}
	}
	return false
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
