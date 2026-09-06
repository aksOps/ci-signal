package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"ci-signal/internal/config"
	"ci-signal/internal/gitlab"
	"ci-signal/internal/review"

	gitlabapi "gitlab.com/gitlab-org/api/client-go/v2"
)

const fingerprintVersion = "review-input-v1"

type fingerprintInput struct {
	Version       string             `json:"version"`
	ExcludedPaths []string           `json:"excluded_paths,omitempty"`
	Base          string             `json:"base"`
	Head          string             `json:"head"`
	Scope         review.Scope       `json:"scope"`
	MR            fingerprintMR      `json:"mr"`
	HumanNotes    []fingerprintNote  `json:"human_notes"`
	Guidance      map[string]string  `json:"guidance"`
	Provider      config.Provider    `json:"provider"`
	Limits        fingerprintLimits  `json:"limits"`
	Tools         fingerprintTools   `json:"tools"`
	Permissions   config.Permissions `json:"permissions"`
	Gating        []review.Category  `json:"gating"`
}

type fingerprintMR struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Target      string `json:"target"`
	Source      string `json:"source"`
}

type fingerprintNote struct {
	ID         int64             `json:"id"`
	Body       string            `json:"body"`
	Author     string            `json:"author"`
	AuthorKind gitlab.AuthorKind `json:"author_kind"`
	UpdatedAt  string            `json:"updated_at"`
	Resolvable bool              `json:"resolvable"`
	Resolved   bool              `json:"resolved"`
}

type fingerprintLimits struct {
	OverallTimeout  string `json:"overall_timeout"`
	SessionTimeout  string `json:"session_timeout"`
	ToolTimeout     string `json:"tool_timeout"`
	Concurrency     int    `json:"concurrency"`
	Sessions        int    `json:"sessions"`
	Corrections     int    `json:"corrections"`
	UnitsPerSession int    `json:"units_per_session"`
	SourceBytes     int    `json:"source_bytes"`
	DiffBytes       int    `json:"diff_bytes"`
	ToolBytes       int    `json:"tool_bytes"`
	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
}

type fingerprintTools struct {
	Git             string                  `json:"git"`
	ASTGrep         string                  `json:"ast_grep"`
	StructuralScans []config.StructuralScan `json:"structural_scans"`
	ExternalMCP     []config.MCPServer      `json:"external_mcp"`
}

func Fingerprint(capture Capture, settings config.Config) (review.Fingerprint, error) {
	notes := make([]fingerprintNote, 0, len(capture.Context.Notes))
	for _, note := range capture.Context.Notes {
		if note == nil || note.System || note.Internal || note.Confidential || strings.Contains(note.Body, "<!-- ci-signal-state") {
			continue
		}
		notes = append(notes, normalizedNote(note, capture.Context.AuthorKinds[note.Author.ID]))
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
	guidance := make(map[string]string, len(capture.RelevantFiles))
	for name, content := range capture.RelevantFiles {
		digest := sha256.Sum256(content)
		guidance[name] = hex.EncodeToString(digest[:])
	}
	gating := append([]review.Category(nil), settings.Review.GatingCategories...)
	sort.Slice(gating, func(i, j int) bool { return gating[i] < gating[j] })
	excluded := append([]string(nil), settings.Repository.ExcludedPaths...)
	sort.Strings(excluded)
	excluded = slices.Compact(excluded)
	mr := capture.Context.MergeRequest
	input := fingerprintInput{
		Version: fingerprintVersion, Base: capture.Snapshot.BaseCommit, Head: capture.Snapshot.HeadCommit,
		Scope: settings.Review.Scope, ExcludedPaths: excluded, HumanNotes: notes, Guidance: guidance,
		Provider: settings.Copilot.Provider, Permissions: settings.Permissions, Gating: gating,
		Limits: fingerprintLimits{
			OverallTimeout: settings.Limits.OverallTimeout.Value().String(), SessionTimeout: settings.Limits.SessionTimeout.Value().String(), ToolTimeout: settings.Limits.ToolTimeout.Value().String(),
			Concurrency: settings.Limits.MaxConcurrency, Sessions: settings.Limits.MaxSessions, Corrections: settings.Limits.MaxCorrectionAttempts, UnitsPerSession: settings.Limits.MaxUnitsPerSession,
			SourceBytes: settings.Limits.MaxSourceBytes, DiffBytes: settings.Limits.MaxDiffBytes, ToolBytes: settings.Limits.MaxToolOutputBytes,
			InputTokens: settings.Limits.MaxInputTokens, OutputTokens: settings.Limits.MaxOutputTokens,
		},
		Tools: fingerprintTools{Git: settings.Tools.GitPath, ASTGrep: settings.Tools.ASTGrepPath, StructuralScans: settings.Tools.StructuralScans, ExternalMCP: settings.Tools.ExternalMCP},
	}
	if mr != nil {
		input.MR = fingerprintMR{mr.Title, mr.Description, mr.TargetBranch, mr.SourceBranch}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return review.Fingerprint("fp_" + hex.EncodeToString(digest[:])), nil
}

func normalizedNote(note *gitlabapi.Note, authorKind gitlab.AuthorKind) fingerprintNote {
	if authorKind == "" {
		authorKind = gitlab.AuthorUnknown
	}
	result := fingerprintNote{ID: note.ID, Body: note.Body, AuthorKind: authorKind, Resolvable: note.Resolvable, Resolved: note.Resolved}
	result.Author = note.Author.Username
	if note.UpdatedAt != nil {
		result.UpdatedAt = note.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	return result
}
