package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (a *Analyzer) MaterializeGuidance(ctx context.Context, snapshot Snapshot, destination string, maxFiles int, maxBytes int64) (GuidanceSnapshot, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return GuidanceSnapshot{}, err
	}
	if !filepath.IsAbs(destination) || maxFiles < 1 || maxBytes < 1 {
		return GuidanceSnapshot{}, errors.New("absolute guidance destination and positive limits are required")
	}
	destination = filepath.Clean(destination)
	if relative, err := filepath.Rel(a.repositoryReal, destination); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return GuidanceSnapshot{}, errors.New("guidance destination cannot be inside the reviewed repository")
	}
	if entries, err := os.ReadDir(destination); err == nil && len(entries) != 0 {
		return GuidanceSnapshot{}, errors.New("guidance destination must be empty")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return GuidanceSnapshot{}, fmt.Errorf("read guidance destination: %w", err)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return GuidanceSnapshot{}, fmt.Errorf("create guidance destination: %w", err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return GuidanceSnapshot{}, fmt.Errorf("protect guidance destination: %w", err)
	}
	realDestination, err := filepath.EvalSymlinks(destination)
	if err != nil || filepath.Clean(realDestination) != destination {
		return GuidanceSnapshot{}, errors.New("guidance destination cannot contain symlinks")
	}

	raw, err := a.runGit(ctx, a.maxMetadataBytes, "ls-tree", "-r", "-z", "--format=%(objectmode)%x09%(objectname)%x09%(path)", snapshot.HeadCommit, "--")
	if err != nil {
		return GuidanceSnapshot{}, fmt.Errorf("list source-head guidance: %w", err)
	}
	result := GuidanceSnapshot{RuntimeDirectory: destination}
	instructionDirs := make(map[string]struct{})
	skillDirs := make(map[string]struct{})
	var total int64
	for _, entry := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		parts := bytes.SplitN(entry, []byte{'\t'}, 3)
		if len(parts) != 3 {
			return GuidanceSnapshot{}, errors.New("Git tree output for guidance is malformed")
		}
		mode, object, filePath := string(parts[0]), string(parts[1]), string(parts[2])
		if a.isExcludedDirectory(filePath) {
			continue
		}
		kind, root := guidanceKind(filePath)
		if kind == "" {
			continue
		}
		if mode == "120000" || mode == "160000" {
			return GuidanceSnapshot{}, fmt.Errorf("guidance path %q is a symlink or submodule", filePath)
		}
		if mode != "100644" && mode != "100755" {
			return GuidanceSnapshot{}, fmt.Errorf("guidance path %q has unsupported mode %s", filePath, mode)
		}
		if len(result.Files) >= maxFiles {
			return GuidanceSnapshot{}, fmt.Errorf("%w: guidance file count exceeds %d", ErrOutputLimit, maxFiles)
		}
		sizeRaw, err := a.runGit(ctx, 64, "cat-file", "-s", object)
		if err != nil {
			return GuidanceSnapshot{}, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(sizeRaw)), 10, 64)
		if err != nil || size < 0 || total+size > maxBytes {
			return GuidanceSnapshot{}, fmt.Errorf("%w: guidance bytes exceed %d", ErrOutputLimit, maxBytes)
		}
		content, err := a.runGit(ctx, int(size)+1, "cat-file", "blob", object)
		if err != nil {
			return GuidanceSnapshot{}, err
		}
		target := filepath.Join(destination, filepath.FromSlash(filePath))
		if err := ensureContained(destination, target); err != nil {
			return GuidanceSnapshot{}, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return GuidanceSnapshot{}, err
		}
		if err := os.WriteFile(target, content, 0o400); err != nil {
			return GuidanceSnapshot{}, err
		}
		result.Files = append(result.Files, target)
		total += size
		if kind == "instruction" {
			result.InstructionFiles = append(result.InstructionFiles, target)
			instructionDirs[filepath.Dir(target)] = struct{}{}
		} else {
			skillDirs[filepath.Join(destination, filepath.FromSlash(root))] = struct{}{}
		}
	}
	result.InstructionDirectories = mapKeys(instructionDirs)
	result.SkillDirectories = mapKeys(skillDirs)
	sort.Strings(result.Files)
	sort.Strings(result.InstructionFiles)
	return result, nil
}

func guidanceKind(filePath string) (string, string) {
	cleaned, err := literalPath(filePath)
	if err != nil {
		return "", ""
	}
	if pathBase := filepath.Base(filepath.FromSlash(cleaned)); pathBase == "AGENTS.md" {
		return "instruction", ""
	}
	if cleaned == ".github/copilot-instructions.md" || strings.HasPrefix(cleaned, ".github/instructions/") && strings.HasSuffix(cleaned, ".instructions.md") {
		return "instruction", ""
	}
	for _, root := range []string{".github/skills", ".agents/skills"} {
		if strings.HasPrefix(cleaned, root+"/") {
			return "skill", root
		}
	}
	return "", ""
}

func ensureContained(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("materialized path %q escapes destination", target)
	}
	return nil
}

func mapKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
