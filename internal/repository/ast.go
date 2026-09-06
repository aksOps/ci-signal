package repository

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ci-signal/internal/review"
)

const SupportedASTGrepVersion = "0.45.3"

func (a *Analyzer) VerifyTools(ctx context.Context) error {
	version, err := a.run(ctx, a.astGrepPath, []string{"--version"}, 1024)
	if err != nil {
		return fmt.Errorf("verify ast-grep: %w", err)
	}
	if strings.TrimSpace(string(version)) != "ast-grep "+SupportedASTGrepVersion {
		return fmt.Errorf("ast-grep version is %q, require %s", strings.TrimSpace(string(version)), SupportedASTGrepVersion)
	}
	if _, err := a.runGit(ctx, 1024, "--version"); err != nil {
		return fmt.Errorf("verify Git: %w", err)
	}
	return nil
}

func (a *Analyzer) ExtractDeclarations(ctx context.Context, snapshot Snapshot, side Side, filePath string) (DeclarationEvidence, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return DeclarationEvidence{}, err
	}
	if err := a.validateReviewPath(filePath); err != nil {
		return DeclarationEvidence{}, err
	}
	if strings.ToLower(filepath.Ext(filePath)) != ".go" {
		return DeclarationEvidence{}, fmt.Errorf("structural extraction is unsupported for %q", filePath)
	}
	commit, err := snapshot.commit(side)
	if err != nil {
		return DeclarationEvidence{}, err
	}
	object, _, err := a.blobObject(ctx, commit, filePath)
	if err != nil {
		return DeclarationEvidence{}, err
	}
	sizeRaw, err := a.runGit(ctx, 64, "cat-file", "-s", object)
	if err != nil {
		return DeclarationEvidence{}, err
	}
	var size int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(sizeRaw)), &size); err != nil {
		return DeclarationEvidence{}, fmt.Errorf("parse source size: %w", err)
	}
	if size > a.maxASTSourceBytes {
		return DeclarationEvidence{}, fmt.Errorf("%w: source file is %d bytes, AST limit is %d", ErrOutputLimit, size, a.maxASTSourceBytes)
	}
	source, err := a.runGit(ctx, int(size)+1, "cat-file", "blob", object)
	if err != nil {
		return DeclarationEvidence{}, err
	}
	temporaryDir, err := os.MkdirTemp("", "ci-signal-ast-*")
	if err != nil {
		return DeclarationEvidence{}, fmt.Errorf("create AST workspace: %w", err)
	}
	defer os.RemoveAll(temporaryDir)
	temporaryFile := filepath.Join(temporaryDir, "snapshot.go")
	if err := os.WriteFile(temporaryFile, source, 0o600); err != nil {
		return DeclarationEvidence{}, fmt.Errorf("write AST source: %w", err)
	}
	output, err := a.run(ctx, a.astGrepPath, []string{"scan", "--rule", a.goRulePath, "--json=stream", "--threads", "1", temporaryFile}, a.maxASTOutputBytes)
	if err != nil {
		return DeclarationEvidence{}, err
	}
	declarations, err := decodeDeclarations(output)
	if err != nil {
		return DeclarationEvidence{}, err
	}
	return DeclarationEvidence{Declarations: declarations, Reference: sourceReference(snapshot, review.SourceRepositoryAST, side, filePath, 0, output)}, nil
}

type astMatch struct {
	Text  string `json:"text"`
	Range struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
		End struct {
			Line int `json:"line"`
		} `json:"end"`
		ByteOffset struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"byteOffset"`
	} `json:"range"`
	MetaVariables struct {
		Single map[string]struct {
			Text string `json:"text"`
		} `json:"single"`
	} `json:"metaVariables"`
}

func decodeDeclarations(output []byte) ([]Declaration, error) {
	result := make([]Declaration, 0)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), len(output)+1)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var match astMatch
		if err := json.Unmarshal(scanner.Bytes(), &match); err != nil {
			return nil, fmt.Errorf("decode ast-grep match: %w", err)
		}
		name := match.MetaVariables.Single["NAME"].Text
		if name == "" {
			return nil, errors.New("ast-grep declaration match omitted NAME")
		}
		kind := "type"
		if strings.HasPrefix(match.Text, "func (") {
			kind = "method"
		} else if strings.HasPrefix(match.Text, "func ") {
			kind = "function"
		}
		result = append(result, Declaration{
			Kind:      kind,
			Name:      name,
			StartLine: match.Range.Start.Line + 1,
			EndLine:   match.Range.End.Line + 1,
			Bytes:     match.Range.ByteOffset.End - match.Range.ByteOffset.Start,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ast-grep output: %w", err)
	}
	return result, nil
}
