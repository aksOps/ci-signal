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

type StructuralMatch struct {
	Path      string `json:"path"`
	Text      string `json:"text"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type StructuralScanEvidence struct {
	Name          string                 `json:"name"`
	Matches       []StructuralMatch      `json:"matches"`
	Complete      bool                   `json:"complete"`
	NextAfterPath string                 `json:"next_after_path,omitempty"`
	Limitations   []string               `json:"limitations,omitempty"`
	Reference     review.SourceReference `json:"source_reference"`
}

// RunStructuralScan applies one host-configured rule to every eligible Go file
// in the pinned source-head tree. It does not add those files to AI coverage.
func (a *Analyzer) RunStructuralScan(ctx context.Context, snapshot Snapshot, name, language, rulePath, afterPath string) (StructuralScanEvidence, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return StructuralScanEvidence{}, err
	}
	if name == "" || language != "go" || !filepath.IsAbs(rulePath) {
		return StructuralScanEvidence{}, fmt.Errorf("structural scan requires a name, language go, and absolute rule path")
	}
	files, err := a.fullProjectFiles(ctx, snapshot, nil)
	if err != nil {
		return StructuralScanEvidence{}, err
	}
	var matches []StructuralMatch
	var evidence bytes.Buffer
	var limitations []string
	lastPath := ""
	for _, file := range files {
		filePath := file.HeadPath
		if afterPath != "" && filePath <= afterPath {
			continue
		}
		if filepath.Ext(filePath) != ".go" || a.reviewPathExclusion(filePath) != "" || a.isGenerated(filePath) {
			continue
		}
		mode, binary, err := a.objectClassification(ctx, snapshot, SideHead, filePath)
		if err != nil {
			return StructuralScanEvidence{}, err
		}
		if mode != "100644" && mode != "100755" || binary {
			continue
		}
		remaining := a.maxASTOutputBytes - evidence.Len()
		if remaining < 1 {
			return a.structuralResult(snapshot, name, afterPath, matches, limitations, evidence.Bytes(), false, lastPath), nil
		}
		output, err := a.scanFile(ctx, snapshot, filePath, rulePath, remaining)
		if err != nil {
			if errors.Is(err, ErrOutputLimit) {
				if lastPath == "" {
					limitations = append(limitations, fmt.Sprintf("%s exceeded the per-result structural output limit", filePath))
					lastPath = filePath
					continue
				}
				return a.structuralResult(snapshot, name, afterPath, matches, limitations, evidence.Bytes(), false, lastPath), nil
			}
			return StructuralScanEvidence{}, err
		}
		evidence.Write(output)
		decoded, err := decodeStructuralMatches(filePath, output)
		if err != nil {
			return StructuralScanEvidence{}, err
		}
		matches = append(matches, decoded...)
		lastPath = filePath
	}
	return a.structuralResult(snapshot, name, afterPath, matches, limitations, evidence.Bytes(), true, ""), nil
}

func (a *Analyzer) structuralResult(snapshot Snapshot, name, afterPath string, matches []StructuralMatch, limitations []string, evidence []byte, complete bool, next string) StructuralScanEvidence {
	return StructuralScanEvidence{Name: name, Matches: matches, Complete: complete, NextAfterPath: next, Limitations: limitations, Reference: sourceReference(snapshot, review.SourceRepositoryAST, SideHead, "structural-scan:"+name+":"+afterPath, 0, evidence)}
}

func (a *Analyzer) scanFile(ctx context.Context, snapshot Snapshot, filePath, rulePath string, maxOutput int) ([]byte, error) {
	object, _, err := a.blobObject(ctx, snapshot.HeadCommit, filePath)
	if err != nil {
		return nil, err
	}
	sizeRaw, err := a.runGit(ctx, 64, "cat-file", "-s", object)
	if err != nil {
		return nil, err
	}
	var size int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(sizeRaw)), &size); err != nil {
		return nil, err
	}
	if size > a.maxASTSourceBytes {
		return nil, fmt.Errorf("%w: structural scan source %q exceeds %d bytes", ErrOutputLimit, filePath, a.maxASTSourceBytes)
	}
	source, err := a.runGit(ctx, int(size)+1, "cat-file", "blob", object)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "ci-signal-structural-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	temporary := filepath.Join(directory, "snapshot.go")
	if err := os.WriteFile(temporary, source, 0o600); err != nil {
		return nil, err
	}
	return a.run(ctx, a.astGrepPath, []string{"scan", "--rule", rulePath, "--json=stream", "--threads", "1", temporary}, maxOutput)
}

func decodeStructuralMatches(filePath string, output []byte) ([]StructuralMatch, error) {
	var result []StructuralMatch
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), len(output)+1)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var match astMatch
		if err := json.Unmarshal(scanner.Bytes(), &match); err != nil {
			return nil, fmt.Errorf("decode structural scan match: %w", err)
		}
		result = append(result, StructuralMatch{Path: filePath, Text: match.Text, StartLine: match.Range.Start.Line + 1, EndLine: match.Range.End.Line + 1})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
