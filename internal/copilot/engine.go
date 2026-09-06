package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"ci-signal/internal/config"
	"ci-signal/internal/review"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

const (
	SDKVersion = "1.0.13"
	CLIVersion = "1.0.83"

	submitReviewTool  = "submit_review"
	reviewerAgentName = "ci-signal-reviewer"
)

var (
	ErrNoAcceptedSubmission      = errors.New("Copilot session ended without an accepted submit_review call")
	ErrCorrectionBudgetExhausted = errors.New("submit_review correction budget exhausted")
	ErrRequiredSkillsNotInvoked  = errors.New("required review skills were not invoked")
)

// Options are host-controlled dependencies shared by review assignments.
type Options struct {
	Config      config.Config
	Secrets     config.Secrets
	Acceptor    *review.Acceptor
	NativeTools []sdk.Tool
	Environment []string
	LookupEnv   config.LookupEnv
}

// RunRequest binds a Copilot session to one materialized, pinned repository snapshot.
// Project guidance directories must be inside RuntimeDirectory. Trusted core guidance
// comes from Config and is never discovered from the CI checkout.
type RunRequest struct {
	SnapshotID             review.SnapshotID
	RuntimeDirectory       string
	InstructionDirectories []string
	SkillDirectories       []string
	Prompt                 string
	Registry               *AssignmentRegistry
	Acceptance             review.AcceptanceMetadata
}

// Result contains only host-accepted structured review state and factual runtime data.
// Assistant prose is intentionally absent.
type Result struct {
	Accepted      *review.AcceptedSubmission
	Telemetry     review.Telemetry
	InvokedSkills []string
}

// Engine owns one isolated Copilot lifecycle per assignment.
type Engine struct {
	config      config.Config
	secrets     config.Secrets
	acceptor    *review.Acceptor
	nativeTools []sdk.Tool
	environment []string
	lookupEnv   config.LookupEnv
	factory     runtimeFactory
}

func New(options Options) (*Engine, error) {
	return newEngine(options, sdkRuntimeFactory{})
}

func newEngine(options Options, factory runtimeFactory) (*Engine, error) {
	if err := options.Config.Validate(); err != nil {
		return nil, fmt.Errorf("validate engine config: %w", err)
	}
	if options.Acceptor == nil {
		return nil, errors.New("review acceptor is required")
	}
	if options.Secrets.ProviderToken.Value() == "" {
		return nil, errors.New("Ollama Cloud BYOK credential is required")
	}
	if factory == nil {
		return nil, errors.New("Copilot runtime factory is required")
	}
	if options.LookupEnv == nil {
		options.LookupEnv = os.LookupEnv
	}
	if options.Environment == nil {
		options.Environment = os.Environ()
	}
	tools, err := validateNativeTools(options.NativeTools, options.Config.Permissions.NativeTools)
	if err != nil {
		return nil, err
	}
	return &Engine{
		config:      options.Config,
		secrets:     options.Secrets,
		acceptor:    options.Acceptor,
		nativeTools: tools,
		environment: append([]string(nil), options.Environment...),
		lookupEnv:   options.LookupEnv,
		factory:     factory,
	}, nil
}

func validateNativeTools(tools []sdk.Tool, allowed []string) ([]sdk.Tool, error) {
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tools)+1)
	seen[submitReviewTool] = struct{}{}
	result := make([]sdk.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "" || tool.Handler == nil {
			return nil, errors.New("native tools require a name and handler")
		}
		if _, ok := allow[tool.Name]; !ok {
			return nil, fmt.Errorf("native tool %q is not allowed by host configuration", tool.Name)
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			return nil, fmt.Errorf("duplicate or reserved native tool %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		result = append(result, tool)
	}
	return result, nil
}

func (e *Engine) Run(ctx context.Context, request RunRequest) (result Result, runErr error) {
	if err := e.validateRequest(request); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(e.config.Copilot.HomeDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create isolated Copilot home: %w", err)
	}
	if err := os.Chmod(e.config.Copilot.HomeDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("protect isolated Copilot home: %w", err)
	}
	diagnostics, err := newDiagnostics(e.config.Diagnostics, e.secrets, e.sensitiveEnvironmentValues()...)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if err := diagnostics.Close(); runErr == nil && err != nil {
			runErr = err
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, e.config.Limits.SessionTimeout.Value())
	defer cancel()
	collector := newEventCollector(
		string(request.Acceptance.SubmissionID),
		e.config.Copilot.Provider.Model,
		e.config.Guidance.RequiredSkills,
		e.config.Limits,
		e.config.Diagnostics,
		diagnostics,
		cancel,
	)

	clientOptions := e.clientOptions(request.RuntimeDirectory)
	runtime, err := e.factory.Start(runCtx, clientOptions)
	if err != nil {
		return Result{}, fmt.Errorf("start pinned Copilot CLI %s: %w", CLIVersion, err)
	}
	defer func() {
		if err := runtime.Stop(); runErr == nil && err != nil {
			runErr = fmt.Errorf("stop Copilot runtime: %w", err)
		}
	}()
	version, err := runtime.Version(runCtx)
	if err != nil {
		return Result{}, fmt.Errorf("read Copilot CLI version: %w", err)
	}
	if version != CLIVersion {
		return Result{}, fmt.Errorf("unsupported Copilot CLI version %q; require %s", version, CLIVersion)
	}
	authenticated, err := runtime.SubscriptionAuthenticated(runCtx)
	if err != nil {
		return Result{}, fmt.Errorf("verify Copilot subscription authentication is disabled: %w", err)
	}
	if authenticated {
		return Result{}, errors.New("Copilot subscription authentication is active in a BYOK-only runtime")
	}

	mcpServers, err := e.mcpServers(request.RuntimeDirectory)
	if err != nil {
		return Result{}, err
	}
	state := &submissionState{
		acceptor:       e.acceptor,
		registry:       request.Registry,
		metadata:       request.Acceptance,
		maxCorrections: e.config.Limits.MaxCorrectionAttempts,
		collector:      collector,
		cancel:         cancel,
	}
	tools, err := e.sessionTools(state, diagnostics)
	if err != nil {
		return Result{}, err
	}
	sessionConfig := e.sessionConfig(request, tools, mcpServers, collector)
	session, err := runtime.CreateSession(runCtx, sessionConfig)
	if err != nil {
		return Result{}, fmt.Errorf("create Ollama Cloud BYOK session: %w", err)
	}
	defer session.Disconnect()

	skills, err := session.LoadedSkills(runCtx)
	if err != nil {
		return Result{}, fmt.Errorf("load configured skills: %w", err)
	}
	if err := validateSkillOwnership(skills, e.config.Guidance.ReservedSkillNames, request.RuntimeDirectory, request.SkillDirectories); err != nil {
		return Result{}, err
	}
	invoked, err := session.InvokedSkills(runCtx)
	if err != nil {
		return Result{}, fmt.Errorf("read invoked skills before review: %w", err)
	}
	collector.recordInvokedSkills(invoked)

	prompt, err := e.prompt(request)
	if err != nil {
		return Result{}, err
	}
	_, sendErr := session.SendAndWait(runCtx, sdk.MessageOptions{Prompt: prompt})
	if sendErr != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		abortCtx, abortCancel := context.WithTimeout(context.WithoutCancel(ctx), e.config.Limits.ToolTimeout.Value())
		_ = session.Abort(abortCtx)
		abortCancel()
		return e.failedResult(collector), fmt.Errorf("Copilot session timeout: %w", runCtx.Err())
	}
	if sendErr != nil && !state.accepted() {
		if state.correctionsExhausted() {
			return e.failedResult(collector), fmt.Errorf("%w: %v", ErrCorrectionBudgetExhausted, sendErr)
		}
		return e.failedResult(collector), fmt.Errorf("run Copilot review session: %w", sendErr)
	}

	invoked, err = session.InvokedSkills(ctx)
	if err != nil {
		return e.failedResult(collector), fmt.Errorf("read invoked skills after review: %w", err)
	}
	collector.recordInvokedSkills(invoked)
	if missing := collector.missingSkills(e.config.Guidance.RequiredSkills); len(missing) != 0 {
		return e.failedResult(collector), fmt.Errorf("%w: %s", ErrRequiredSkillsNotInvoked, strings.Join(missing, ", "))
	}
	if err := collector.integrityError(); err != nil {
		return e.failedResult(collector), err
	}
	accepted := state.result()
	if accepted == nil {
		if state.correctionsExhausted() {
			return e.failedResult(collector), ErrCorrectionBudgetExhausted
		}
		return e.failedResult(collector), ErrNoAcceptedSubmission
	}
	return Result{
		Accepted:      accepted,
		Telemetry:     collector.telemetry(),
		InvokedSkills: collector.invokedSkills(),
	}, nil
}

func (e *Engine) failedResult(collector *eventCollector) Result {
	return Result{Telemetry: collector.telemetry(), InvokedSkills: collector.invokedSkills()}
}

func (e *Engine) validateRequest(request RunRequest) error {
	if request.SnapshotID == "" {
		return errors.New("snapshot identity is required")
	}
	if request.Registry == nil {
		return errors.New("assignment registry is required")
	}
	runtimeDir, err := existingDirectory(request.RuntimeDirectory, "runtime directory")
	if err != nil {
		return err
	}
	projectDir := filepath.Clean(e.config.Repository.ProjectDir)
	if within(projectDir, runtimeDir) {
		return errors.New("Copilot runtime directory must not be the CI repository checkout")
	}
	for _, item := range append(append([]string(nil), request.InstructionDirectories...), request.SkillDirectories...) {
		dir, err := existingDirectory(item, "project guidance directory")
		if err != nil {
			return err
		}
		if !within(runtimeDir, dir) {
			return fmt.Errorf("project guidance directory %q is outside materialized runtime directory", item)
		}
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return errors.New("review assignment prompt is required")
	}
	for _, secret := range []string{e.secrets.ProviderToken.Value(), e.secrets.JobToken.Value(), e.secrets.APIToken.Value()} {
		if secret != "" && strings.Contains(request.Prompt, secret) {
			return errors.New("review prompt contains a configured credential")
		}
	}
	return nil
}

func existingDirectory(path, label string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be absolute", label)
	}
	clean := filepath.Clean(path)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s must be a directory", label)
	}
	return clean, nil
}

func within(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (e *Engine) clientOptions(runtimeDirectory string) *sdk.ClientOptions {
	return &sdk.ClientOptions{
		Connection:           sdk.StdioConnection{Env: sanitizedEnvironment(e.environment, e.config, e.secrets)},
		WorkingDirectory:     runtimeDirectory,
		BaseDirectory:        e.config.Copilot.HomeDir,
		UseLoggedInUser:      sdk.Bool(false),
		Mode:                 sdk.ModeEmpty,
		LogLevel:             sdkLogLevel(e.config.Diagnostics.LogLevel),
		EnableRemoteSessions: false,
	}
}

func (e *Engine) sessionConfig(request RunRequest, tools []sdk.Tool, mcp map[string]sdk.MCPServerConfig, collector *eventCollector) *sdk.SessionConfig {
	toolSet := sdk.NewToolSet()
	for _, tool := range tools {
		toolSet.AddCustom(tool.Name)
	}
	if len(mcp) != 0 {
		toolSet.AddMCP("*")
	}
	instructionDirs := append(append([]string(nil), e.config.Guidance.CoreInstructionDirs...), request.InstructionDirectories...)
	skillDirs := append(append([]string(nil), e.config.Guidance.CoreSkillDirs...), request.SkillDirectories...)
	return &sdk.SessionConfig{
		SessionID:                          string(request.Acceptance.SubmissionID),
		ClientName:                         "ci-signal/" + SDKVersion,
		Model:                              e.config.Copilot.Provider.Model,
		WorkingDirectory:                   request.RuntimeDirectory,
		ConfigDirectory:                    e.config.Copilot.HomeDir,
		EnableConfigDiscovery:              sdk.Bool(false),
		EnableOnDemandInstructionDiscovery: sdk.Bool(false),
		EnableHostGitOperations:            sdk.Bool(false),
		EnableSessionStore:                 sdk.Bool(false),
		SkipEmbeddingRetrieval:             sdk.Bool(true),
		EmbeddingCacheStorage:              sdk.String("in-memory"),
		EnableSkills:                       sdk.Bool(true),
		IncludedBuiltinSkills:              []string{},
		SkipCustomInstructions:             sdk.Bool(false),
		CustomAgentsLocalOnly:              sdk.Bool(true),
		EnableFileHooks:                    sdk.Bool(false),
		EnableExperimentalMode:             sdk.Bool(false),
		EnableFileChangeTracking:           sdk.Bool(false),
		Streaming:                          sdk.Bool(e.config.Diagnostics.LogReasoning),
		IncludeSubAgentStreamingEvents:     sdk.Bool(false),
		Provider: &sdk.ProviderConfig{
			Type:            config.ProviderTypeOpenAI,
			WireAPI:         config.WireAPIResponses,
			BaseURL:         config.OllamaCloudEndpoint,
			BearerToken:     e.secrets.ProviderToken.Value(),
			ModelID:         config.OllamaCloudModel,
			WireModel:       config.OllamaCloudModel,
			MaxPromptTokens: safeInt(e.config.Limits.MaxInputTokens),
			MaxOutputTokens: safeInt(e.config.Limits.MaxOutputTokens),
		},
		Providers:           nil,
		Models:              nil,
		Tools:               tools,
		AvailableTools:      toolSet.ToSlice(),
		OnPermissionRequest: e.permissionHandler(),
		OnAutoModeSwitchRequest: func(sdk.AutoModeSwitchRequest, sdk.AutoModeSwitchInvocation) (sdk.AutoModeSwitchResponse, error) {
			return sdk.AutoModeSwitchResponseNo, nil
		},
		MCPServers:             mcp,
		DisabledMCPServers:     nil,
		MCPOAuthTokenStorage:   "in-memory",
		InstructionDirectories: instructionDirs,
		SkillDirectories:       skillDirs,
		CustomAgents: []sdk.CustomAgentConfig{{
			Name:   reviewerAgentName,
			Prompt: "Apply every preloaded review skill and finish only by calling submit_review.",
			Skills: append([]string(nil), e.config.Guidance.RequiredSkills...),
		}},
		Agent:            reviewerAgentName,
		OnEvent:          collector.handle,
		InfiniteSessions: &sdk.InfiniteSessionConfig{Enabled: sdk.Bool(false)},
		LargeOutput: &sdk.LargeToolOutputConfig{
			Enabled:      sdk.Bool(false),
			MaxSizeBytes: int64Pointer(int64(e.config.Limits.MaxToolOutputBytes)),
		},
		ToolSearch: &sdk.ToolSearchConfig{Enabled: sdk.Bool(false)},
		Memory:     &sdk.MemoryConfiguration{Enabled: false},
	}
}

func safeInt(value uint64) int {
	if value > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}

func int64Pointer(value int64) *int64 { return &value }

func (e *Engine) prompt(request RunRequest) (string, error) {
	base, err := os.ReadFile(e.config.Guidance.ReviewPromptFile)
	if err != nil {
		return "", fmt.Errorf("read configured review prompt: %w", err)
	}
	if len(base) > e.config.Limits.MaxToolOutputBytes {
		return "", errors.New("configured review prompt exceeds the host byte limit")
	}
	var builder strings.Builder
	builder.Write(base)
	builder.WriteString("\n\nPinned snapshot: ")
	builder.WriteString(string(request.SnapshotID))
	builder.WriteString("\n\n")
	builder.WriteString(request.Prompt)
	builder.WriteString("\n\nUse every required preloaded skill. Submit the assessment exclusively through submit_review. Assistant prose is ignored.")
	return builder.String(), nil
}

func sanitizedEnvironment(environment []string, cfg config.Config, secrets config.Secrets) []string {
	deniedExact := map[string]struct{}{
		cfg.GitLab.JobTokenEnv:             {},
		cfg.GitLab.APITokenEnv:             {},
		cfg.Copilot.Provider.CredentialEnv: {},
		"CI_JOB_TOKEN":                     {},
		"GITLAB_API_TOKEN":                 {},
		"OLLAMA_API_KEY":                   {},
	}
	result := make([]string, 0, len(environment))
	secretValues := map[string]struct{}{}
	for _, value := range []string{secrets.ProviderToken.Value(), secrets.JobToken.Value(), secrets.APIToken.Value()} {
		if value != "" {
			secretValues[value] = struct{}{}
		}
	}
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if !found {
			continue
		}
		if _, denied := deniedExact[name]; denied {
			continue
		}
		if strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "GH_") || strings.HasPrefix(upper, "COPILOT_") {
			continue
		}
		if _, aliasesCredential := secretValues[value]; aliasesCredential {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func sdkLogLevel(level config.LogLevel) string {
	if level == config.LogWarn {
		return "warning"
	}
	return string(level)
}

func (e *Engine) mcpServers(runtimeDirectory string) (map[string]sdk.MCPServerConfig, error) {
	allowed := make(map[string]struct{}, len(e.config.Permissions.ExternalMCP))
	for _, name := range e.config.Permissions.ExternalMCP {
		allowed[name] = struct{}{}
	}
	servers := make(map[string]sdk.MCPServerConfig, len(allowed))
	for _, server := range e.config.Tools.ExternalMCP {
		if _, ok := allowed[server.Name]; !ok {
			continue
		}
		environment := make(map[string]string, len(server.EnvRefs))
		for key, reference := range server.EnvRefs {
			value, ok := e.lookupEnv(reference)
			if !ok || value == "" {
				return nil, fmt.Errorf("MCP server %q environment reference %s is missing", server.Name, reference)
			}
			if e.isProtectedCredential(value) {
				return nil, fmt.Errorf("MCP server %q environment reference %s aliases a protected credential", server.Name, reference)
			}
			environment[key] = value
		}
		switch server.Transport {
		case config.MCPStdio:
			servers[server.Name] = sdk.MCPStdioServerConfig{
				Tools:            []string{"*"},
				Timeout:          int(e.config.Limits.ToolTimeout.Value().Seconds()),
				Command:          server.Command,
				Args:             append([]string(nil), server.Args...),
				Env:              environment,
				WorkingDirectory: runtimeDirectory,
			}
		case config.MCPHTTP:
			servers[server.Name] = sdk.MCPHTTPServerConfig{
				Tools:   []string{"*"},
				Timeout: int(e.config.Limits.ToolTimeout.Value().Seconds()),
				URL:     server.URL,
				Headers: environment,
			}
		default:
			return nil, fmt.Errorf("unsupported MCP transport %q", server.Transport)
		}
	}
	return servers, nil
}

func (e *Engine) isProtectedCredential(value string) bool {
	return value != "" && (value == e.secrets.ProviderToken.Value() || value == e.secrets.JobToken.Value() || value == e.secrets.APIToken.Value())
}

func (e *Engine) sensitiveEnvironmentValues() []string {
	values := make([]string, 0)
	for _, server := range e.config.Tools.ExternalMCP {
		for _, reference := range server.EnvRefs {
			if value, ok := e.lookupEnv(reference); ok && value != "" {
				values = append(values, value)
			}
		}
	}
	return values
}

func (e *Engine) permissionHandler() sdk.PermissionHandlerFunc {
	native := make(map[string]struct{}, len(e.config.Permissions.NativeTools))
	for _, name := range e.config.Permissions.NativeTools {
		native[name] = struct{}{}
	}
	mcp := make(map[string]struct{}, len(e.config.Permissions.ExternalMCP))
	for _, name := range e.config.Permissions.ExternalMCP {
		mcp[name] = struct{}{}
	}
	return func(request sdk.PermissionRequest, _ sdk.PermissionInvocation) (rpc.PermissionDecision, error) {
		switch item := request.(type) {
		case sdk.PermissionRequestCustomTool:
			if _, ok := native[item.ToolName]; ok && !requiresManagedApproval(item.ManagedApprovalRequired) {
				return &rpc.PermissionDecisionApproveOnce{}, nil
			}
		case sdk.PermissionRequestMCP:
			if _, ok := mcp[item.ServerName]; ok && item.ReadOnly && !requiresManagedApproval(item.ManagedApprovalRequired) {
				return &rpc.PermissionDecisionApproveOnce{}, nil
			}
		}
		feedback := "ci-signal permits only host-configured read-only tools"
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
	}
}

func requiresManagedApproval(value *bool) bool { return value != nil && *value }

func validateSkillOwnership(skills []skillInfo, reserved []string, runtimeDirectory string, projectDirs []string) error {
	reservedNames := make(map[string]struct{}, len(reserved))
	for _, name := range reserved {
		reservedNames[strings.ToLower(name)] = struct{}{}
	}
	projectRoots := append([]string{runtimeDirectory}, projectDirs...)
	for _, skill := range skills {
		if _, isReserved := reservedNames[strings.ToLower(skill.Name)]; !isReserved {
			continue
		}
		if skill.Path == "" {
			return fmt.Errorf("reserved core skill %q has no verifiable filesystem identity", skill.Name)
		}
		for _, root := range projectRoots {
			if within(filepath.Clean(root), filepath.Clean(skill.Path)) {
				return fmt.Errorf("project skill %q shadows a reserved core skill", skill.Name)
			}
		}
	}
	return nil
}

type submissionState struct {
	mu             sync.Mutex
	acceptor       *review.Acceptor
	registry       *AssignmentRegistry
	metadata       review.AcceptanceMetadata
	maxCorrections int
	corrections    int
	acceptedValue  *review.AcceptedSubmission
	collector      *eventCollector
	cancel         context.CancelFunc
}

func (s *submissionState) submit(invocation sdk.ToolInvocation) (sdk.ToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptedValue != nil {
		return sdk.ToolResult{}, errors.New("submit_review already accepted for this assignment")
	}
	if missing := s.collector.missingSkillsLocked(); len(missing) != 0 {
		return sdk.ToolResult{}, fmt.Errorf("%w: %s", ErrRequiredSkillsNotInvoked, strings.Join(missing, ", "))
	}
	raw, err := json.Marshal(invocation.Arguments)
	if err == nil {
		accepted, acceptErr := s.acceptor.Accept(invocation.TraceContext, raw, s.registry.Snapshot(), s.metadata)
		if acceptErr == nil {
			s.acceptedValue = &accepted
			receipt, marshalErr := json.Marshal(accepted.Receipt)
			if marshalErr != nil {
				return sdk.ToolResult{}, fmt.Errorf("encode submit_review receipt: %w", marshalErr)
			}
			return sdk.ToolResult{TextResultForLLM: string(receipt), ResultType: "success"}, nil
		}
		err = acceptErr
	}
	s.corrections++
	if s.corrections >= s.maxCorrections {
		s.cancel()
		return sdk.ToolResult{}, fmt.Errorf("%w: %v", ErrCorrectionBudgetExhausted, err)
	}
	return sdk.ToolResult{}, fmt.Errorf("submit_review rejected; correct the arguments and retry: %w", err)
}

func (s *submissionState) result() *review.AcceptedSubmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptedValue
}

func (s *submissionState) accepted() bool {
	return s.result() != nil
}

func (s *submissionState) correctionsExhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.corrections >= s.maxCorrections
}

func (e *Engine) sessionTools(state *submissionState, diagnostics *diagnostics) ([]sdk.Tool, error) {
	schema, err := review.SubmissionSchema()
	if err != nil {
		return nil, fmt.Errorf("load submit_review tool schema: %w", err)
	}
	tools := make([]sdk.Tool, 0, len(e.nativeTools)+1)
	for _, tool := range e.nativeTools {
		tools = append(tools, e.wrapTool(tool, diagnostics))
	}
	tools = append(tools, sdk.Tool{
		Name:        submitReviewTool,
		Description: "Submit the complete structured assessment for the assigned review units. This is the only review result channel.",
		Parameters:  schema,
		IsTerminal:  true,
		Defer:       sdk.ToolDeferNever,
		Handler:     state.submit,
	})
	return tools, nil
}

func (e *Engine) wrapTool(tool sdk.Tool, diagnostics *diagnostics) sdk.Tool {
	handler := tool.Handler
	tool.Handler = func(invocation sdk.ToolInvocation) (sdk.ToolResult, error) {
		parent := invocation.TraceContext
		if parent == nil {
			parent = context.Background()
		}
		toolCtx, cancel := context.WithTimeout(parent, e.config.Limits.ToolTimeout.Value())
		defer cancel()
		invocation.TraceContext = toolCtx
		type response struct {
			result sdk.ToolResult
			err    error
		}
		completed := make(chan response, 1)
		go func() {
			result, err := handler(invocation)
			completed <- response{result: result, err: err}
		}()
		select {
		case <-toolCtx.Done():
			return sdk.ToolResult{}, fmt.Errorf("native tool %q timeout: %w", tool.Name, toolCtx.Err())
		case outcome := <-completed:
			if outcome.err != nil {
				return sdk.ToolResult{}, outcome.err
			}
			if len(outcome.result.TextResultForLLM) > e.config.Limits.MaxToolOutputBytes || len(outcome.result.SessionLog) > e.config.Limits.MaxToolOutputBytes {
				return sdk.ToolResult{}, fmt.Errorf("native tool %q output exceeds host byte limit", tool.Name)
			}
			diagnostics.ToolResult(tool.Name, invocation.ToolCallID, outcome.result)
			return outcome.result, nil
		}
	}
	return tool
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
