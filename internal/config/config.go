package config

import (
	"encoding"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ci-signal/internal/review"
)

const (
	Version = 1

	ProviderOllamaCloud = "ollama_cloud"
	ProviderTypeOpenAI  = "openai"
	WireAPIResponses    = "responses"
	OllamaCloudEndpoint = "https://ollama.com/v1"
	OllamaCloudModel    = "deepseek-v4-flash:cloud"
)

type Duration time.Duration

var _ encoding.TextUnmarshaler = (*Duration)(nil)

func (d *Duration) UnmarshalText(text []byte) error {
	value, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

type ExitPolicy string

const (
	ExitAlwaysZero    ExitPolicy = "always_zero"
	ExitOnNeedsReview ExitPolicy = "needs_review_nonzero"
	ExitOnIncomplete  ExitPolicy = "incomplete_nonzero"
)

type MCPTransport string

const (
	MCPStdio MCPTransport = "stdio"
	MCPHTTP  MCPTransport = "http"
)

type LabelSource string

const (
	LabelFromVerdict         LabelSource = "verdict"
	LabelFromCoverage        LabelSource = "coverage"
	LabelFromRequestedModels LabelSource = "requested_models"
	LabelFromObservedModels  LabelSource = "observed_models"
	LabelFromExecutedTools   LabelSource = "executed_tools"
	LabelFromTokenUsage      LabelSource = "token_usage"
	LabelFromTeam            LabelSource = "team"
	LabelFromMetadata        LabelSource = "metadata"
)

type LabelMode string

const (
	LabelSingle LabelMode = "single"
	LabelSet    LabelMode = "set"
)

type MissingValue string

const (
	MissingOmit    MissingValue = "omit"
	MissingUnknown MissingValue = "unknown"
)

type TokenLabelFormat string

const (
	TokenLabelExact   TokenLabelFormat = "exact"
	TokenLabelBuckets TokenLabelFormat = "buckets"
)

type Config struct {
	Version     int         `json:"version"`
	GitLab      GitLab      `json:"gitlab"`
	Repository  Repository  `json:"repository"`
	Copilot     Copilot     `json:"copilot"`
	Guidance    Guidance    `json:"guidance"`
	Limits      Limits      `json:"limits"`
	Review      Review      `json:"review"`
	Tools       Tools       `json:"tools"`
	Permissions Permissions `json:"permissions"`
	Metadata    Metadata    `json:"metadata"`
	Labels      Labels      `json:"labels"`
	Diagnostics Diagnostics `json:"diagnostics"`
}

type GitLab struct {
	BaseURL     string `json:"base_url"`
	Project     string `json:"project"`
	MRIID       int    `json:"mr_iid"`
	JobTokenEnv string `json:"job_token_env"`
	APITokenEnv string `json:"api_token_env"`
}

type Repository struct {
	ProjectDir    string   `json:"project_dir"`
	ExcludedPaths []string `json:"excluded_paths,omitempty"`
}

type Copilot struct {
	Provider Provider `json:"provider"`
	HomeDir  string   `json:"home_dir"`
	StateDir string   `json:"state_dir"`
}

type Provider struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	CredentialEnv string `json:"credential_env"`
	WireAPI       string `json:"wire_api"`
}

type Guidance struct {
	CoreSkillDirs       []string `json:"core_skill_dirs"`
	CoreInstructionDirs []string `json:"core_instruction_dirs"`
	ProjectSkillDirs    []string `json:"project_skill_dirs"`
	ReviewPromptFile    string   `json:"review_prompt_file"`
	RequiredSkills      []string `json:"required_skills"`
	ReservedSkillNames  []string `json:"reserved_skill_names"`
}

type Limits struct {
	OverallTimeout        Duration `json:"overall_timeout"`
	SessionTimeout        Duration `json:"session_timeout"`
	ToolTimeout           Duration `json:"tool_timeout"`
	APITimeout            Duration `json:"api_timeout"`
	MaxConcurrency        int      `json:"max_concurrency"`
	MaxSessions           int      `json:"max_sessions"`
	MaxCorrectionAttempts int      `json:"max_correction_attempts"`
	MaxUnitsPerSession    int      `json:"max_units_per_session"`
	MaxSourceBytes        int      `json:"max_source_bytes"`
	MaxDiffBytes          int      `json:"max_diff_bytes"`
	MaxToolOutputBytes    int      `json:"max_tool_output_bytes"`
	MaxReportBytes        int      `json:"max_report_bytes"`
	MaxInputTokens        uint64   `json:"max_input_tokens"`
	MaxOutputTokens       uint64   `json:"max_output_tokens"`
}

type Review struct {
	Scope            review.Scope      `json:"scope"`
	GatingCategories []review.Category `json:"gating_categories"`
	ExitPolicy       ExitPolicy        `json:"exit_policy"`
}

type Tools struct {
	GitPath         string           `json:"git_path"`
	ASTGrepPath     string           `json:"ast_grep_path"`
	StructuralScans []StructuralScan `json:"structural_scans,omitempty"`
	ExternalMCP     []MCPServer      `json:"external_mcp"`
}

type StructuralScan struct {
	Name     string `json:"name"`
	Language string `json:"language"`
	RulePath string `json:"rule_path"`
}

type PermissionMode string

const PermissionReadOnly PermissionMode = "read_only"

type Permissions struct {
	Mode        PermissionMode `json:"mode"`
	NativeTools []string       `json:"native_tools"`
	ExternalMCP []string       `json:"external_mcp"`
}

type Metadata struct {
	Team   string            `json:"team,omitempty"`
	Values map[string]string `json:"values,omitempty"`
}

type MCPServer struct {
	Name      string            `json:"name"`
	Transport MCPTransport      `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	EnvRefs   map[string]string `json:"env_refs,omitempty"`
}

type Labels struct {
	Static         []string       `json:"static"`
	CreateMissing  bool           `json:"create_missing"`
	ReservedNames  []string       `json:"reserved_names"`
	ReservedScopes []string       `json:"reserved_scopes"`
	Dynamic        []LabelMapping `json:"dynamic"`
}

type LabelMapping struct {
	Name         string            `json:"name"`
	Source       LabelSource       `json:"source"`
	Mode         LabelMode         `json:"mode"`
	Missing      MissingValue      `json:"missing"`
	Values       map[string]string `json:"values,omitempty"`
	Prefix       string            `json:"prefix,omitempty"`
	MetadataKey  string            `json:"metadata_key,omitempty"`
	TokenFormat  TokenLabelFormat  `json:"token_format,omitempty"`
	TokenBuckets []TokenBucket     `json:"token_buckets,omitempty"`
}

type TokenBucket struct {
	LessThan uint64 `json:"less_than,omitempty"`
	Label    string `json:"label"`
}

type Diagnostics struct {
	LogLevel       LogLevel `json:"log_level"`
	LogToolCalls   bool     `json:"log_tool_calls"`
	LogToolResults bool     `json:"log_tool_results"`
	LogReasoning   bool     `json:"log_reasoning"`
	LogFile        string   `json:"log_file"`
	DryRun         bool     `json:"dry_run"`
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = Version
	}
	if c.GitLab.JobTokenEnv == "" {
		c.GitLab.JobTokenEnv = "CI_JOB_TOKEN"
	}
	if c.GitLab.APITokenEnv == "" {
		c.GitLab.APITokenEnv = "GITLAB_API_TOKEN"
	}
	if c.Limits.OverallTimeout == 0 {
		c.Limits.OverallTimeout = Duration(30 * time.Minute)
	}
	if c.Limits.SessionTimeout == 0 {
		c.Limits.SessionTimeout = Duration(10 * time.Minute)
	}
	if c.Limits.ToolTimeout == 0 {
		c.Limits.ToolTimeout = Duration(30 * time.Second)
	}
	if c.Limits.APITimeout == 0 {
		c.Limits.APITimeout = Duration(30 * time.Second)
	}
	if c.Limits.MaxConcurrency == 0 {
		c.Limits.MaxConcurrency = 2
	}
	if c.Limits.MaxSessions == 0 {
		c.Limits.MaxSessions = 8
	}
	if c.Limits.MaxCorrectionAttempts == 0 {
		c.Limits.MaxCorrectionAttempts = 2
	}
	if c.Limits.MaxUnitsPerSession == 0 {
		c.Limits.MaxUnitsPerSession = 20
	}
	if c.Limits.MaxSourceBytes == 0 {
		c.Limits.MaxSourceBytes = 256 * 1024
	}
	if c.Limits.MaxDiffBytes == 0 {
		c.Limits.MaxDiffBytes = 512 * 1024
	}
	if c.Limits.MaxToolOutputBytes == 0 {
		c.Limits.MaxToolOutputBytes = 256 * 1024
	}
	if c.Limits.MaxReportBytes == 0 {
		c.Limits.MaxReportBytes = 900 * 1024
	}
	if c.Limits.MaxInputTokens == 0 {
		c.Limits.MaxInputTokens = 200_000
	}
	if c.Limits.MaxOutputTokens == 0 {
		c.Limits.MaxOutputTokens = 32_000
	}
	if c.Review.Scope == "" {
		c.Review.Scope = review.ScopeMRImpact
	}
	if len(c.Review.GatingCategories) == 0 {
		c.Review.GatingCategories = []review.Category{review.CategoryBlocker, review.CategoryRisk, review.CategoryQuestion}
	}
	if c.Review.ExitPolicy == "" {
		c.Review.ExitPolicy = ExitOnIncomplete
	}
	if c.Tools.GitPath == "" {
		c.Tools.GitPath = "git"
	}
	if c.Tools.ASTGrepPath == "" {
		c.Tools.ASTGrepPath = "ast-grep"
	}
	if c.Permissions.Mode == "" {
		c.Permissions.Mode = PermissionReadOnly
	}
	if len(c.Permissions.NativeTools) == 0 {
		c.Permissions.NativeTools = []string{"repository_read", "repository_search", "git_read", "ast_grep", "structural_scan", "submit_review"}
	}
	if c.Diagnostics.LogLevel == "" {
		c.Diagnostics.LogLevel = LogInfo
	}
	for i := range c.Labels.Dynamic {
		if c.Labels.Dynamic[i].Missing == "" {
			c.Labels.Dynamic[i].Missing = MissingOmit
		}
	}
}

func (c Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if err := validateGitLab(c.GitLab); err != nil {
		return err
	}
	if !filepath.IsAbs(c.Repository.ProjectDir) {
		return errors.New("repository.project_dir must be absolute")
	}
	for _, excluded := range c.Repository.ExcludedPaths {
		if excluded == "" || excluded == "." || path.IsAbs(excluded) || path.Clean(excluded) != excluded || strings.HasPrefix(excluded, "../") || strings.ContainsAny(excluded, "\\\x00") || len(excluded) > 1 && excluded[1] == ':' {
			return fmt.Errorf("repository.excluded_paths entry %q must be a literal repository-relative file or directory", excluded)
		}
	}
	if err := validateCopilot(c.Copilot); err != nil {
		return err
	}
	if err := validateGuidance(c.Guidance); err != nil {
		return err
	}
	if err := validateLimits(c.Limits); err != nil {
		return err
	}
	if err := validateReview(c.Review); err != nil {
		return err
	}
	if err := validateTools(c.Tools); err != nil {
		return err
	}
	if err := validatePermissions(c.Permissions, c.Tools); err != nil {
		return err
	}
	if err := validateMetadata(c.Metadata); err != nil {
		return err
	}
	if c.GitLab.JobTokenEnv == c.GitLab.APITokenEnv || c.GitLab.JobTokenEnv == c.Copilot.Provider.CredentialEnv || c.GitLab.APITokenEnv == c.Copilot.Provider.CredentialEnv {
		return errors.New("job, API, and provider credential references must be distinct")
	}
	for _, server := range c.Tools.ExternalMCP {
		for _, reference := range server.EnvRefs {
			if reference == c.GitLab.JobTokenEnv || reference == c.GitLab.APITokenEnv || reference == c.Copilot.Provider.CredentialEnv {
				return fmt.Errorf("external MCP server %q cannot receive a GitLab or provider credential", server.Name)
			}
		}
	}
	if err := validateLabels(c.Labels, c.Metadata); err != nil {
		return err
	}
	if !oneOf(string(c.Diagnostics.LogLevel), string(LogDebug), string(LogInfo), string(LogWarn), string(LogError)) {
		return fmt.Errorf("unsupported diagnostics.log_level %q", c.Diagnostics.LogLevel)
	}
	if c.Diagnostics.LogFile != "" && !filepath.IsAbs(c.Diagnostics.LogFile) {
		return errors.New("diagnostics.log_file must be absolute when set")
	}
	return nil
}

func validateGitLab(value GitLab) error {
	parsed, err := url.Parse(value.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || !oneOf(parsed.Scheme, "http", "https") {
		return errors.New("gitlab.base_url must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("gitlab.base_url cannot contain credentials, query, or fragment")
	}
	if strings.TrimSpace(value.Project) == "" || value.MRIID < 1 {
		return errors.New("gitlab.project and positive gitlab.mr_iid are required")
	}
	if !validEnvName(value.JobTokenEnv) || !validEnvName(value.APITokenEnv) {
		return errors.New("GitLab token references must be environment variable names")
	}
	return nil
}

func validateCopilot(value Copilot) error {
	if value.Provider.Name != ProviderOllamaCloud || value.Provider.Type != ProviderTypeOpenAI || value.Provider.Endpoint != OllamaCloudEndpoint || value.Provider.Model != OllamaCloudModel || value.Provider.WireAPI != WireAPIResponses {
		return errors.New("copilot.provider must be the supported Ollama Cloud openai/responses endpoint and model")
	}
	if !validEnvName(value.Provider.CredentialEnv) {
		return errors.New("copilot.provider.credential_env must be an environment variable name")
	}
	if !filepath.IsAbs(value.HomeDir) || !filepath.IsAbs(value.StateDir) {
		return errors.New("copilot.home_dir and copilot.state_dir must be absolute")
	}
	if filepath.Clean(value.HomeDir) == filepath.Clean(value.StateDir) {
		return errors.New("copilot.home_dir and copilot.state_dir must be distinct")
	}
	return nil
}

func validateGuidance(value Guidance) error {
	if len(value.CoreSkillDirs) == 0 || len(value.CoreInstructionDirs) == 0 || len(value.RequiredSkills) == 0 {
		return errors.New("core skill directories, core instruction directories, and required skills are required")
	}
	paths := append(append(append([]string{}, value.CoreSkillDirs...), value.CoreInstructionDirs...), value.ProjectSkillDirs...)
	paths = append(paths, value.ReviewPromptFile)
	for _, item := range paths {
		if !filepath.IsAbs(item) {
			return fmt.Errorf("guidance path %q must be absolute", item)
		}
	}
	reserved := make(map[string]struct{}, len(value.ReservedSkillNames))
	for _, name := range value.ReservedSkillNames {
		if strings.TrimSpace(name) == "" {
			return errors.New("reserved skill names cannot be empty")
		}
		if _, duplicate := reserved[name]; duplicate {
			return fmt.Errorf("duplicate reserved skill name %q", name)
		}
		reserved[name] = struct{}{}
	}
	for _, name := range value.RequiredSkills {
		if _, ok := reserved[name]; !ok {
			return fmt.Errorf("required core skill %q must have a reserved name", name)
		}
	}
	return nil
}

func validateLimits(value Limits) error {
	durations := []Duration{value.OverallTimeout, value.SessionTimeout, value.ToolTimeout, value.APITimeout}
	for _, duration := range durations {
		if duration <= 0 {
			return errors.New("all configured timeouts must be positive")
		}
	}
	if value.SessionTimeout > value.OverallTimeout {
		return errors.New("limits.session_timeout cannot exceed limits.overall_timeout")
	}
	integers := []int{value.MaxConcurrency, value.MaxSessions, value.MaxCorrectionAttempts, value.MaxUnitsPerSession, value.MaxSourceBytes, value.MaxDiffBytes, value.MaxToolOutputBytes, value.MaxReportBytes}
	for _, number := range integers {
		if number < 1 {
			return errors.New("all configured count and byte limits must be positive")
		}
	}
	if value.MaxConcurrency > 32 {
		return errors.New("limits.max_concurrency cannot exceed 32")
	}
	if value.MaxInputTokens == 0 || value.MaxOutputTokens == 0 {
		return errors.New("review token budgets must be positive")
	}
	return nil
}

func validateReview(value Review) error {
	if value.Scope != review.ScopeMRImpact && value.Scope != review.ScopeFullProject {
		return fmt.Errorf("unsupported review.scope %q", value.Scope)
	}
	if !oneOf(string(value.ExitPolicy), string(ExitAlwaysZero), string(ExitOnNeedsReview), string(ExitOnIncomplete)) {
		return fmt.Errorf("unsupported review.exit_policy %q", value.ExitPolicy)
	}
	seen := make(map[review.Category]struct{}, len(value.GatingCategories))
	for _, category := range value.GatingCategories {
		if category == review.CategoryInfo || !oneOf(string(category), string(review.CategoryBlocker), string(review.CategoryRisk), string(review.CategoryQuestion)) {
			return fmt.Errorf("review.gating_categories contains unsupported category %q", category)
		}
		if _, duplicate := seen[category]; duplicate {
			return fmt.Errorf("duplicate gating category %q", category)
		}
		seen[category] = struct{}{}
	}
	return nil
}

func validateTools(value Tools) error {
	if strings.TrimSpace(value.GitPath) == "" || strings.TrimSpace(value.ASTGrepPath) == "" {
		return errors.New("tools.git_path and tools.ast_grep_path are required")
	}
	names := make(map[string]struct{}, len(value.ExternalMCP))
	for _, scan := range value.StructuralScans {
		if !toolNamePattern.MatchString(scan.Name) || scan.Language != "go" || !filepath.IsAbs(scan.RulePath) {
			return fmt.Errorf("structural scan %q requires a valid name, language go, and absolute rule_path", scan.Name)
		}
		if _, duplicate := names[scan.Name]; duplicate {
			return fmt.Errorf("duplicate structural scan %q", scan.Name)
		}
		names[scan.Name] = struct{}{}
	}
	for _, server := range value.ExternalMCP {
		if strings.TrimSpace(server.Name) == "" {
			return errors.New("external MCP server name is required")
		}
		if _, duplicate := names[server.Name]; duplicate {
			return fmt.Errorf("duplicate external MCP server %q", server.Name)
		}
		names[server.Name] = struct{}{}
		switch server.Transport {
		case MCPStdio:
			if server.Command == "" || server.URL != "" {
				return fmt.Errorf("stdio MCP server %q requires command and forbids URL", server.Name)
			}
		case MCPHTTP:
			parsed, err := url.Parse(server.URL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || server.Command != "" || len(server.Args) != 0 {
				return fmt.Errorf("HTTP MCP server %q requires an HTTPS URL and forbids command arguments", server.Name)
			}
		default:
			return fmt.Errorf("unsupported MCP transport %q", server.Transport)
		}
		for key, reference := range server.EnvRefs {
			if !validEnvName(key) || !validEnvName(reference) {
				return fmt.Errorf("MCP server %q has invalid environment reference", server.Name)
			}
		}
	}
	return nil
}

func validatePermissions(value Permissions, tools Tools) error {
	if value.Mode != PermissionReadOnly {
		return fmt.Errorf("unsupported permissions.mode %q", value.Mode)
	}
	if len(value.NativeTools) == 0 {
		return errors.New("permissions.native_tools cannot be empty")
	}
	native := make(map[string]struct{}, len(value.NativeTools))
	for _, name := range value.NativeTools {
		if !toolNamePattern.MatchString(name) {
			return fmt.Errorf("invalid native tool name %q", name)
		}
		if _, duplicate := native[name]; duplicate {
			return fmt.Errorf("duplicate native tool permission %q", name)
		}
		native[name] = struct{}{}
	}
	if _, ok := native["submit_review"]; !ok {
		return errors.New("permissions.native_tools must include submit_review")
	}
	configuredMCP := make(map[string]struct{}, len(tools.ExternalMCP))
	for _, server := range tools.ExternalMCP {
		configuredMCP[server.Name] = struct{}{}
	}
	allowedMCP := make(map[string]struct{}, len(value.ExternalMCP))
	for _, name := range value.ExternalMCP {
		if _, ok := configuredMCP[name]; !ok {
			return fmt.Errorf("permissions.external_mcp references unconfigured server %q", name)
		}
		if _, duplicate := allowedMCP[name]; duplicate {
			return fmt.Errorf("duplicate external MCP permission %q", name)
		}
		allowedMCP[name] = struct{}{}
	}
	return nil
}

func validateMetadata(value Metadata) error {
	if len(value.Team) > 200 || strings.ContainsAny(value.Team, "\r\n") {
		return errors.New("metadata.team is invalid")
	}
	for key, item := range value.Values {
		if !metadataKeyPattern.MatchString(key) || strings.TrimSpace(item) == "" || len(item) > 1000 || strings.ContainsAny(item, "\r\n") {
			return fmt.Errorf("metadata value %q is invalid", key)
		}
	}
	return nil
}

func validateLabels(value Labels, metadata Metadata) error {
	managed := make(map[string]struct{})
	scopes := make(map[string]string)
	for _, label := range append(append([]string{}, value.Static...), value.ReservedNames...) {
		if err := validateLabel(label); err != nil {
			return err
		}
		managed[label] = struct{}{}
	}
	for _, label := range value.Static {
		if scope := labelScope(label); scope != "" {
			if prior, exists := scopes[scope]; exists && prior != label {
				return fmt.Errorf("static labels %q and %q conflict in scope %q", prior, label, scope)
			}
			scopes[scope] = label
		}
	}
	for _, scope := range value.ReservedScopes {
		if strings.TrimSpace(scope) == "" {
			return errors.New("reserved label scopes cannot be empty")
		}
	}
	if len(value.Dynamic) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(value.Dynamic))
	for _, mapping := range value.Dynamic {
		if strings.TrimSpace(mapping.Name) == "" {
			return errors.New("dynamic label mapping name is required")
		}
		if _, duplicate := names[mapping.Name]; duplicate {
			return fmt.Errorf("duplicate dynamic label mapping %q", mapping.Name)
		}
		names[mapping.Name] = struct{}{}
		if !validLabelSource(mapping.Source) || !oneOf(string(mapping.Mode), string(LabelSingle), string(LabelSet)) || !oneOf(string(mapping.Missing), string(MissingOmit), string(MissingUnknown)) {
			return fmt.Errorf("dynamic label mapping %q has an unsupported source, mode, or missing-value choice", mapping.Name)
		}
		outputs := make([]string, 0, len(mapping.Values)+len(mapping.TokenBuckets))
		for _, label := range mapping.Values {
			outputs = append(outputs, label)
		}
		for _, bucket := range mapping.TokenBuckets {
			outputs = append(outputs, bucket.Label)
		}
		if mapping.Prefix != "" {
			outputs = append(outputs, mapping.Prefix+"value")
		}
		for _, label := range outputs {
			if err := validateLabel(label); err != nil {
				return fmt.Errorf("dynamic label mapping %q: %w", mapping.Name, err)
			}
			if mapping.Mode == LabelSet && labelScope(label) != "" {
				return fmt.Errorf("set-valued mapping %q cannot emit scoped label %q", mapping.Name, label)
			}
		}
		if mapping.Mode == LabelSingle {
			var mappingScope string
			for _, label := range outputs {
				scope := labelScope(label)
				if scope == "" {
					return fmt.Errorf("single-valued mapping %q must emit scoped labels", mapping.Name)
				}
				if mappingScope != "" && mappingScope != scope {
					return fmt.Errorf("single-valued mapping %q emits multiple scopes", mapping.Name)
				}
				mappingScope = scope
			}
			if prior, exists := scopes[mappingScope]; exists {
				return fmt.Errorf("dynamic mapping %q conflicts with %q in scope %q", mapping.Name, prior, mappingScope)
			}
			scopes[mappingScope] = mapping.Name
		}
		if err := validateMappingShape(mapping); err != nil {
			return fmt.Errorf("dynamic label mapping %q: %w", mapping.Name, err)
		}
		if mapping.Source == LabelFromTeam && strings.TrimSpace(metadata.Team) == "" {
			return fmt.Errorf("dynamic label mapping %q requires trusted metadata.team", mapping.Name)
		}
		if mapping.Source == LabelFromMetadata {
			if _, ok := metadata.Values[mapping.MetadataKey]; !ok {
				return fmt.Errorf("dynamic label mapping %q references missing trusted metadata %q", mapping.Name, mapping.MetadataKey)
			}
		}
	}
	return nil
}

func validateMappingShape(mapping LabelMapping) error {
	switch mapping.Source {
	case LabelFromRequestedModels, LabelFromObservedModels, LabelFromExecutedTools:
		if mapping.Mode != LabelSet || mapping.Prefix == "" || len(mapping.Values) != 0 || len(mapping.TokenBuckets) != 0 {
			return errors.New("model and tool sets require set mode and a prefix")
		}
	case LabelFromTokenUsage:
		if mapping.Mode != LabelSingle || !oneOf(string(mapping.TokenFormat), string(TokenLabelExact), string(TokenLabelBuckets)) {
			return errors.New("token usage requires single mode and exact or buckets format")
		}
		if mapping.TokenFormat == TokenLabelExact {
			if mapping.Prefix == "" || len(mapping.TokenBuckets) != 0 {
				return errors.New("exact token format requires a prefix and no buckets")
			}
		} else {
			if len(mapping.TokenBuckets) == 0 {
				return errors.New("bucket token format requires buckets")
			}
			last := uint64(0)
			for i, bucket := range mapping.TokenBuckets {
				if bucket.LessThan != 0 && bucket.LessThan <= last {
					return errors.New("token bucket bounds must increase")
				}
				if bucket.LessThan == 0 && i != len(mapping.TokenBuckets)-1 {
					return errors.New("unbounded token bucket must be last")
				}
				last = bucket.LessThan
			}
		}
	case LabelFromMetadata:
		if mapping.MetadataKey == "" || len(mapping.Values) == 0 {
			return errors.New("metadata mapping requires metadata_key and values")
		}
	default:
		if len(mapping.Values) == 0 {
			return errors.New("mapping requires configured values")
		}
	}
	if mapping.Missing == MissingUnknown && mapping.Prefix == "" && strings.TrimSpace(mapping.Values["unknown"]) == "" {
		return errors.New("unknown missing-value handling requires a prefix or an unknown label")
	}
	return nil
}

func validLabelSource(source LabelSource) bool {
	return oneOf(string(source), string(LabelFromVerdict), string(LabelFromCoverage), string(LabelFromRequestedModels), string(LabelFromObservedModels), string(LabelFromExecutedTools), string(LabelFromTokenUsage), string(LabelFromTeam), string(LabelFromMetadata))
}

func validateLabel(label string) error {
	if strings.TrimSpace(label) == "" || len(label) > 255 || strings.ContainsAny(label, "\r\n") {
		return fmt.Errorf("invalid label %q", label)
	}
	return nil
}

func labelScope(label string) string {
	index := strings.LastIndex(label, "::")
	if index < 1 || index+2 == len(label) {
		return ""
	}
	return label[:index]
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
var metadataKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

func validEnvName(value string) bool {
	return envNamePattern.MatchString(value)
}

func parsePositiveInt(value, name string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}
