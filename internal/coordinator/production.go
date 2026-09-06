package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ci-signal/internal/config"
	"ci-signal/internal/copilot"
	"ci-signal/internal/gitlab"
	"ci-signal/internal/markdown"
	"ci-signal/internal/repository"
	"ci-signal/internal/review"

	sdk "github.com/github/copilot-sdk/go"
)

func NewProduction(loaded config.Loaded, logger *slog.Logger) (*Coordinator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	client, err := gitlab.New(loaded, logger)
	if err != nil {
		return nil, err
	}
	codec := markdown.NewCodec()
	publisher, err := gitlab.NewPublisher(client, codec)
	if err != nil {
		return nil, err
	}
	goRule, err := findGoRule(loaded.Config.Guidance.CoreSkillDirs)
	if err != nil {
		return nil, err
	}
	analyzer, err := repository.NewAnalyzer(repository.Options{
		RepositoryDir: loaded.Config.Repository.ProjectDir, ExcludedPaths: loaded.Config.Repository.ExcludedPaths, GitPath: loaded.Config.Tools.GitPath,
		ASTGrepPath: loaded.Config.Tools.ASTGrepPath, GoRulePath: goRule,
		MaxMetadataBytes: loaded.Config.Limits.MaxDiffBytes, MaxASTSourceBytes: int64(loaded.Config.Limits.MaxSourceBytes), MaxASTOutputBytes: loaded.Config.Limits.MaxToolOutputBytes,
	})
	if err != nil {
		return nil, err
	}
	validator, err := review.NewValidator()
	if err != nil {
		return nil, err
	}
	acceptor, err := review.NewAcceptor(validator, loaded.Config.Copilot.StateDir)
	if err != nil {
		return nil, err
	}

	var currentCapture Capture
	hooks := Hooks{}
	hooks.Capture = func(ctx context.Context, at time.Time) (Capture, error) {
		if err := analyzer.VerifyTools(ctx); err != nil {
			return Capture{}, err
		}
		mrContext, err := client.FetchContext(ctx)
		if err != nil {
			return Capture{}, err
		}
		if !mrContext.Complete || mrContext.MergeRequest == nil {
			return Capture{}, errors.New("GitLab context is incomplete")
		}
		refs := mrContext.MergeRequest.DiffRefs
		snapshot, err := analyzer.CaptureSnapshot(ctx, refs.BaseSha, refs.HeadSha, at)
		if err != nil {
			return Capture{}, err
		}
		inventory, err := analyzer.Inventory(ctx, snapshot, loaded.Config.Review.Scope)
		if err != nil {
			return Capture{}, err
		}
		runtimeID, err := randomID("snapshot_")
		if err != nil {
			return Capture{}, err
		}
		runtimeDir := filepath.Join(loaded.Config.Copilot.StateDir, "runtime", runtimeID)
		guidance, err := analyzer.MaterializeGuidance(ctx, snapshot, runtimeDir, 1000, int64(loaded.Config.Limits.MaxToolOutputBytes))
		if err != nil {
			return Capture{}, err
		}
		relevant, err := trustedInputFiles(loaded.Config, guidance)
		if err != nil {
			return Capture{}, err
		}
		currentCapture = Capture{Context: mrContext, Snapshot: snapshot, Inventory: inventory, Guidance: guidance, RelevantFiles: relevant}
		return currentCapture, nil
	}
	hooks.Recover = publisher.Recover
	hooks.Review = func(ctx context.Context, request BatchRequest) (copilot.Result, error) {
		registry, err := copilot.NewAssignmentRegistry(request.Assignment)
		if err != nil {
			return copilot.Result{}, err
		}
		tools := repositoryTools(analyzer, request.Snapshot, registry, loaded.Config.Limits, loaded.Config.Permissions.NativeTools, request.Scans)
		settings := loaded.Config
		settings.Copilot.HomeDir = filepath.Join(loaded.Config.Copilot.HomeDir, string(request.Acceptance.SubmissionID))
		settings.Guidance.CoreSkillDirs = append(append([]string(nil), loaded.Config.Guidance.CoreSkillDirs...), loaded.Config.Guidance.ProjectSkillDirs...)
		if err := os.MkdirAll(settings.Copilot.HomeDir, 0o700); err != nil {
			return copilot.Result{}, err
		}
		engine, err := copilot.New(copilot.Options{Config: settings, Secrets: loaded.Secrets, Acceptor: acceptor, NativeTools: tools})
		if err != nil {
			return copilot.Result{}, err
		}
		return engine.Run(ctx, copilot.RunRequest{
			SnapshotID: request.Snapshot.ID, RuntimeDirectory: request.Guidance.RuntimeDirectory,
			InstructionDirectories: request.Guidance.InstructionDirectories, SkillDirectories: request.Guidance.SkillDirectories,
			Prompt: request.Prompt, Registry: registry, Acceptance: request.Acceptance,
		})
	}
	hooks.Recheck = func(ctx context.Context, captured Capture) error {
		fresh, err := client.FetchContext(ctx)
		if err != nil {
			return err
		}
		probe := captured
		probe.Context = fresh
		expected, err := Fingerprint(captured, loaded.Config)
		if err != nil {
			return err
		}
		actual, err := Fingerprint(probe, loaded.Config)
		if err != nil {
			return err
		}
		if actual != expected || fresh.MergeRequest == nil || fresh.MergeRequest.DiffRefs.HeadSha != captured.Snapshot.HeadCommit || fresh.MergeRequest.DiffRefs.BaseSha != captured.Snapshot.BaseCommit {
			return errors.New("merge-request source or relevant context changed")
		}
		return nil
	}
	hooks.Publish = func(ctx context.Context, request PublicationRequest) (review.State, error) {
		expected := gitlab.PublicationExpectation{}
		if request.Recovery.Current != nil && request.Recovery.Current.State.Publication != nil {
			expected = gitlab.PublicationExpectation{NoteID: request.Recovery.Current.NoteID, Generation: request.Recovery.Current.State.Publication.Generation}
		}
		result, err := publisher.Publish(ctx, gitlab.PublishRequest{
			State: request.State, Generation: request.Generation, Expected: expected, PublishedAt: time.Now().UTC(), MaxReportBytes: loaded.Config.Limits.MaxReportBytes,
			SourceKinds: publicationSourceKinds(request.State, request.Captured),
			Freshness:   func(ctx context.Context, _ review.State) error { return hooks.Recheck(ctx, request.Captured) }, Labels: &request.Labels, CreateMissingLabels: loaded.Config.Labels.CreateMissing,
		})
		return result.State, err
	}
	hooks.HandleStale = func(ctx context.Context, request PublicationRequest) error {
		_, err := hooks.Publish(ctx, request)
		return err
	}
	hooks.RepairLabels = func(ctx context.Context, state review.State, plan gitlab.LabelPlan, captured Capture) error {
		if state.Publication == nil {
			return nil
		}
		_, err := publisher.SyncPublishedLabels(ctx, gitlab.PublicationExpectation{NoteID: state.Publication.NoteID, Generation: state.Publication.Generation}, state, plan, loaded.Config.Labels.CreateMissing, func(ctx context.Context, _ review.State) error { return hooks.Recheck(ctx, captured) })
		return err
	}
	return New(loaded.Config, hooks)
}

func publicationSourceKinds(state review.State, captured Capture) map[review.SourceReferenceID]review.SourceReferenceKind {
	result := make(map[review.SourceReferenceID]review.SourceReferenceKind)
	for id, reference := range captured.SourceRefs {
		result[id] = reference.Kind
	}
	for _, source := range state.Sources {
		result[source.ID] = source.Kind
	}
	// Repository evidence IDs are generated exclusively by the host analyzer.
	// Their content-addressed src_ namespace is enough for cleanup to know the
	// evidence remains in the pinned repository rather than the retiring note.
	for id := range findingSourceIDs(state.Findings) {
		if _, ok := result[id]; !ok && strings.HasPrefix(string(id), "src_") {
			result[id] = review.SourceRepositorySource
		}
	}
	return result
}

func findingSourceIDs(findings []review.Finding) map[review.SourceReferenceID]struct{} {
	result := make(map[review.SourceReferenceID]struct{})
	for _, finding := range findings {
		for _, evidence := range finding.Evidence {
			for _, id := range evidence.SourceRefs {
				result[id] = struct{}{}
			}
		}
		if finding.Acknowledgement != nil {
			for _, id := range finding.Acknowledgement.SourceRefs {
				result[id] = struct{}{}
			}
		}
		for _, event := range finding.History {
			for _, id := range event.SourceRefs {
				result[id] = struct{}{}
			}
		}
	}
	return result
}

func findGoRule(roots []string) (string, error) {
	for _, root := range roots {
		for _, candidate := range []string{filepath.Join(root, "ast-analysis", "rules", "go-declarations.yml"), filepath.Join(root, "rules", "go-declarations.yml")} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", errors.New("core ast-analysis/rules/go-declarations.yml was not found")
}

func trustedInputFiles(settings config.Config, guidance repository.GuidanceSnapshot) (map[string][]byte, error) {
	result := make(map[string][]byte)
	prompt, err := os.ReadFile(settings.Guidance.ReviewPromptFile)
	if err != nil {
		return nil, err
	}
	result["review_prompt"] = prompt
	for _, scan := range settings.Tools.StructuralScans {
		content, err := os.ReadFile(scan.RulePath)
		if err != nil {
			return nil, fmt.Errorf("read structural scan rule %q: %w", scan.Name, err)
		}
		result["structural_scan/"+scan.Name] = content
	}
	for _, name := range guidance.Files {
		content, err := os.ReadFile(filepath.Join(guidance.RuntimeDirectory, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		result["project/"+filepath.ToSlash(name)] = content
	}
	roots := append(append(append([]string(nil), settings.Guidance.CoreSkillDirs...), settings.Guidance.CoreInstructionDirs...), settings.Guidance.ProjectSkillDirs...)
	for rootIndex, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("trusted guidance symlink %q is not allowed", path)
			}
			if !entry.IsDir() {
				content, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				result[fmt.Sprintf("core/%d/%s", rootIndex, filepath.ToSlash(relative))] = content
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for name, content := range result {
		if len(content) > settings.Limits.MaxToolOutputBytes {
			return nil, fmt.Errorf("guidance file %q exceeds configured limit", name)
		}
	}
	return result, nil
}

func marshalToolResult(value any) (sdk.ToolResult, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return sdk.ToolResult{}, err
	}
	return sdk.ToolResult{TextResultForLLM: string(raw), ResultType: "success"}, nil
}
