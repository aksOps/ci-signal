package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"ci-signal/internal/review"
)

type LookupEnv func(string) (string, bool)

type Secret string

func (Secret) String() string {
	return "[redacted]"
}

func (s Secret) Value() string {
	return string(s)
}

type Secrets struct {
	JobToken      Secret `json:"-"`
	APIToken      Secret `json:"-"`
	ProviderToken Secret `json:"-"`
}

type Loaded struct {
	Config  Config  `json:"config"`
	Secrets Secrets `json:"-"`
}

func LoadFromEnvironment(lookup LookupEnv) (Loaded, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	configPath, ok := lookup("REVIEWER_CONFIG_FILE")
	if !ok || !filepath.IsAbs(configPath) {
		return Loaded{}, errors.New("REVIEWER_CONFIG_FILE must name an absolute host-controlled JSON file")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return Loaded{}, fmt.Errorf("read reviewer config: %w", err)
	}
	var result Config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Loaded{}, fmt.Errorf("decode reviewer config: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Loaded{}, err
	}
	resolveRelativePaths(&result, filepath.Dir(configPath))
	result.applyDefaults()
	if err := applyEnvironment(&result, lookup); err != nil {
		return Loaded{}, err
	}
	if err := result.Validate(); err != nil {
		return Loaded{}, fmt.Errorf("validate reviewer config: %w", err)
	}
	secrets, err := resolveSecrets(result, lookup)
	if err != nil {
		return Loaded{}, err
	}
	return Loaded{Config: result, Secrets: secrets}, nil
}

func resolveRelativePaths(result *Config, base string) {
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(base, value)
	}
	result.Repository.ProjectDir = resolve(result.Repository.ProjectDir)
	result.Copilot.HomeDir = resolve(result.Copilot.HomeDir)
	result.Copilot.StateDir = resolve(result.Copilot.StateDir)
	for index := range result.Guidance.CoreSkillDirs {
		result.Guidance.CoreSkillDirs[index] = resolve(result.Guidance.CoreSkillDirs[index])
	}
	for index := range result.Guidance.CoreInstructionDirs {
		result.Guidance.CoreInstructionDirs[index] = resolve(result.Guidance.CoreInstructionDirs[index])
	}
	for index := range result.Guidance.ProjectSkillDirs {
		result.Guidance.ProjectSkillDirs[index] = resolve(result.Guidance.ProjectSkillDirs[index])
	}
	result.Guidance.ReviewPromptFile = resolve(result.Guidance.ReviewPromptFile)
	result.Diagnostics.LogFile = resolve(result.Diagnostics.LogFile)
	for index := range result.Tools.StructuralScans {
		result.Tools.StructuralScans[index].RulePath = resolve(result.Tools.StructuralScans[index].RulePath)
	}
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("reviewer config contains more than one JSON value")
	}
	return fmt.Errorf("decode trailing reviewer config data: %w", err)
}

func applyEnvironment(result *Config, lookup LookupEnv) error {
	if value, ok := lookup("REVIEWER_GITLAB_BASE_URL"); ok {
		result.GitLab.BaseURL = value
	}
	if value, ok := lookup("REVIEWER_GITLAB_PROJECT"); ok {
		result.GitLab.Project = value
	}
	if value, ok := lookup("REVIEWER_GITLAB_MR_IID"); ok {
		parsed, err := parsePositiveInt(value, "REVIEWER_GITLAB_MR_IID")
		if err != nil {
			return err
		}
		result.GitLab.MRIID = parsed
	}
	if value, ok := lookup("REVIEW_PROJECT_DIR"); ok {
		result.Repository.ProjectDir = value
	} else if value, ok := lookup("CI_PROJECT_DIR"); ok {
		result.Repository.ProjectDir = value
	}
	if value, ok := lookup("REVIEWER_COPILOT_HOME"); ok {
		result.Copilot.HomeDir = value
	}
	if value, ok := lookup("REVIEWER_STATE_DIR"); ok {
		result.Copilot.StateDir = value
	}
	if value, ok := lookup("REVIEWER_BYOK_PROVIDER"); ok {
		result.Copilot.Provider.Name = value
	}
	if value, ok := lookup("REVIEWER_BYOK_ENDPOINT"); ok {
		result.Copilot.Provider.Endpoint = value
	}
	if value, ok := lookup("REVIEWER_BYOK_MODEL"); ok {
		result.Copilot.Provider.Model = value
	}
	if value, ok := lookup("REVIEWER_BYOK_CREDENTIAL_ENV"); ok {
		result.Copilot.Provider.CredentialEnv = value
	}
	if value, ok := lookup("REVIEWER_REVIEW_SCOPE"); ok {
		result.Review.Scope = reviewScope(value)
	}
	if value, ok := lookup("REVIEWER_DRY_RUN"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("REVIEWER_DRY_RUN must be a boolean")
		}
		result.Diagnostics.DryRun = parsed
	}
	return nil
}

func reviewScope(value string) review.Scope {
	return review.Scope(value)
}

func resolveSecrets(result Config, lookup LookupEnv) (Secrets, error) {
	provider, ok := lookup(result.Copilot.Provider.CredentialEnv)
	if !ok || provider == "" {
		return Secrets{}, fmt.Errorf("provider credential environment variable %s is missing", result.Copilot.Provider.CredentialEnv)
	}
	api, ok := lookup(result.GitLab.APITokenEnv)
	if !ok || api == "" {
		return Secrets{}, fmt.Errorf("GitLab API token environment variable %s is missing", result.GitLab.APITokenEnv)
	}
	job, _ := lookup(result.GitLab.JobTokenEnv)
	return Secrets{JobToken: Secret(job), APIToken: Secret(api), ProviderToken: Secret(provider)}, nil
}
