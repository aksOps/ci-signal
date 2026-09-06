package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"ci-signal/internal/review"
)

func (a *Analyzer) Inventory(ctx context.Context, snapshot Snapshot, scope review.Scope) (Inventory, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return Inventory{}, err
	}
	if scope != review.ScopeMRImpact && scope != review.ScopeFullProject {
		return Inventory{}, fmt.Errorf("unsupported review scope %q", scope)
	}
	changes, err := a.changedFiles(ctx, snapshot)
	if err != nil {
		return Inventory{}, err
	}
	for i := range changes {
		filePath, side := changes[i].HeadPath, SideHead
		if filePath == "" {
			filePath, side = changes[i].BasePath, SideBase
		}
		_, binary, err := a.objectClassification(ctx, snapshot, side, filePath)
		if err != nil {
			return Inventory{}, err
		}
		changes[i].Binary = binary
	}
	files := changes
	if scope == review.ScopeFullProject {
		files, err = a.fullProjectFiles(ctx, snapshot, changes)
		if err != nil {
			return Inventory{}, err
		}
	}
	result := Inventory{SnapshotID: snapshot.ID, Scope: scope, Changes: changes}
	packageUnits := make(map[string]struct{})
	for i := range files {
		change := &files[i]
		// A rename across the policy boundary retains only the production side.
		originalBase := change.BasePath
		if reason := a.reviewPathExclusion(change.BasePath); reason != "" {
			result.Exclusions = append(result.Exclusions, Exclusion{Path: change.BasePath, Reason: reason, Excluded: true})
			change.BasePath = ""
			change.Kind = ChangeAdded
		}
		if reason := a.reviewPathExclusion(change.HeadPath); reason != "" {
			if change.HeadPath != originalBase {
				result.Exclusions = append(result.Exclusions, Exclusion{Path: change.HeadPath, Reason: reason, Excluded: true})
			}
			change.HeadPath = ""
			change.Kind = ChangeDeleted
		}
		if change.BasePath == "" && change.HeadPath == "" {
			continue
		}
		selectedPath := change.HeadPath
		selectedSide := SideHead
		if selectedPath == "" {
			selectedPath = change.BasePath
			selectedSide = SideBase
		}
		mode, binary, err := a.objectClassification(ctx, snapshot, selectedSide, selectedPath)
		if err != nil {
			return Inventory{}, err
		}
		change.Binary = binary
		if mode == "160000" {
			result.Exclusions = append(result.Exclusions, Exclusion{Path: selectedPath, Reason: ExclusionSubmodule, Excluded: true})
			continue
		}
		if mode == "120000" {
			result.Exclusions = append(result.Exclusions, Exclusion{Path: selectedPath, Reason: ExclusionSymlink, Excluded: true})
			continue
		}
		if a.isGenerated(selectedPath) {
			result.Exclusions = append(result.Exclusions, Exclusion{Path: selectedPath, Reason: ExclusionGenerated, Excluded: true})
			continue
		}
		if binary {
			result.Exclusions = append(result.Exclusions, Exclusion{Path: selectedPath, Reason: ExclusionBinary, Excluded: true})
			continue
		}

		fileUnit := Unit{ID: unitID(UnitFile, change.BasePath, change.HeadPath, ""), Kind: UnitFile, Language: languageForPath(selectedPath), Change: change.Kind, Fallback: !strings.EqualFold(filepath.Ext(selectedPath), ".go")}
		if change.BasePath != "" {
			fileUnit.Base = &Span{Path: change.BasePath}
		}
		if change.HeadPath != "" {
			fileUnit.Head = &Span{Path: change.HeadPath}
		}
		fileUnitIndex := len(result.Units)
		result.Units = append(result.Units, fileUnit)

		if !strings.EqualFold(filepath.Ext(selectedPath), ".go") {
			result.Exclusions = append(result.Exclusions, Exclusion{Path: selectedPath, Reason: ExclusionUnsupportedStructure, Detail: "file-level review unit retained", Excluded: false, FallbackUnitID: fileUnit.ID})
			continue
		}
		packagePath := path.Dir(selectedPath)
		if _, seen := packageUnits[packagePath]; !seen {
			packageUnits[packagePath] = struct{}{}
			result.Units = append(result.Units, Unit{ID: unitID(UnitPackage, packagePath, packagePath, ""), Kind: UnitPackage, Language: "go", Symbol: packagePath, Change: change.Kind})
		}
		declarations, extractionErr := a.declarationUnits(ctx, snapshot, *change)
		if extractionErr != nil {
			fileUnit.Fallback = true
			fileUnit.Oversized = errors.Is(extractionErr, ErrOutputLimit)
			fileUnit.NeedsEnclosing = fileUnit.Oversized
			result.Units[fileUnitIndex] = fileUnit
			reason := ExclusionExtractionFailed
			if fileUnit.Oversized {
				reason = ExclusionExtractionFailed
			}
			result.Exclusions = append(result.Exclusions, Exclusion{Path: selectedPath, Reason: reason, Detail: extractionErr.Error(), Excluded: false, FallbackUnitID: fileUnit.ID})
			continue
		}
		result.Units = append(result.Units, declarations...)
	}
	return result, nil
}

func (a *Analyzer) changedFiles(ctx context.Context, snapshot Snapshot) ([]FileChange, error) {
	raw, err := a.runGit(ctx, a.maxMetadataBytes, "diff", "--name-status", "-z", "--find-renames=50%", "--no-ext-diff", "--no-textconv", snapshot.BaseCommit, snapshot.HeadCommit, "--")
	if err != nil {
		return nil, fmt.Errorf("inventory changed paths: %w", err)
	}
	return parseNameStatus(raw)
}

func parseNameStatus(raw []byte) ([]FileChange, error) {
	fields := bytes.Split(raw, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	result := make([]FileChange, 0, len(fields)/2)
	for i := 0; i < len(fields); {
		status := string(fields[i])
		i++
		if status == "" {
			return nil, errors.New("Git name-status output contains an empty status")
		}
		kind := ChangeModified
		record := FileChange{}
		switch status[0] {
		case 'A':
			kind = ChangeAdded
		case 'D':
			kind = ChangeDeleted
		case 'M':
			kind = ChangeModified
		case 'T':
			kind = ChangeType
		case 'R':
			kind = ChangeRenamed
			record.Similarity, _ = strconv.Atoi(status[1:])
		default:
			return nil, fmt.Errorf("unsupported Git change status %q", status)
		}
		record.Kind = kind
		if i >= len(fields) {
			return nil, errors.New("Git name-status output omitted a path")
		}
		first, err := literalPath(string(fields[i]))
		if err != nil {
			return nil, err
		}
		i++
		switch kind {
		case ChangeAdded:
			record.HeadPath = first
		case ChangeDeleted:
			record.BasePath = first
		case ChangeRenamed:
			if i >= len(fields) {
				return nil, errors.New("Git rename output omitted its destination")
			}
			second, err := literalPath(string(fields[i]))
			if err != nil {
				return nil, err
			}
			i++
			record.BasePath, record.HeadPath = first, second
		default:
			record.BasePath, record.HeadPath = first, first
		}
		result = append(result, record)
	}
	return result, nil
}

func (a *Analyzer) fullProjectFiles(ctx context.Context, snapshot Snapshot, changed []FileChange) ([]FileChange, error) {
	raw, err := a.runGit(ctx, a.maxMetadataBytes, "ls-tree", "-r", "-z", "--name-only", snapshot.HeadCommit, "--")
	if err != nil {
		return nil, fmt.Errorf("inventory full project: %w", err)
	}
	changeByHead := make(map[string]FileChange, len(changed))
	for _, change := range changed {
		if change.HeadPath != "" {
			changeByHead[change.HeadPath] = change
		}
	}
	result := make([]FileChange, 0)
	for _, item := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		if len(item) == 0 {
			continue
		}
		filePath, err := literalPath(string(item))
		if err != nil {
			return nil, err
		}
		if change, ok := changeByHead[filePath]; ok {
			result = append(result, change)
		} else {
			result = append(result, FileChange{Kind: ChangeUnchanged, BasePath: filePath, HeadPath: filePath})
		}
	}
	for _, change := range changed {
		if change.Kind == ChangeDeleted {
			result = append(result, change)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].HeadPath, result[j].HeadPath
		if left == "" {
			left = result[i].BasePath
		}
		if right == "" {
			right = result[j].BasePath
		}
		return left < right
	})
	return result, nil
}

func (a *Analyzer) objectClassification(ctx context.Context, snapshot Snapshot, side Side, filePath string) (string, bool, error) {
	commit, err := snapshot.commit(side)
	if err != nil {
		return "", false, err
	}
	object, mode, err := a.blobObject(ctx, commit, filePath)
	if errors.Is(err, ErrUnsupportedObject) {
		return "160000", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if mode == "120000" {
		return mode, false, nil
	}
	data, err := a.runGit(ctx, 8193, "cat-file", "blob", object)
	if errors.Is(err, ErrOutputLimit) {
		chunk, chunkErr := a.runGitChunk(ctx, 0, 8192, "cat-file", "blob", object)
		if chunkErr != nil {
			return "", false, chunkErr
		}
		data = chunk.Data
	} else if err != nil {
		return "", false, err
	}
	return mode, bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data), nil
}

func (a *Analyzer) declarationUnits(ctx context.Context, snapshot Snapshot, change FileChange) ([]Unit, error) {
	var baseDeclarations, headDeclarations []Declaration
	var err error
	if change.BasePath != "" {
		var evidence DeclarationEvidence
		evidence, err = a.ExtractDeclarations(ctx, snapshot, SideBase, change.BasePath)
		if err != nil {
			return nil, err
		}
		baseDeclarations = evidence.Declarations
	}
	if change.HeadPath != "" {
		var evidence DeclarationEvidence
		evidence, err = a.ExtractDeclarations(ctx, snapshot, SideHead, change.HeadPath)
		if err != nil {
			return nil, err
		}
		headDeclarations = evidence.Declarations
	}
	type candidate struct {
		base []Declaration
		head []Declaration
	}
	byKey := make(map[string]*candidate)
	for _, declaration := range baseDeclarations {
		key := declaration.Kind + "\x00" + declaration.Name
		if byKey[key] == nil {
			byKey[key] = &candidate{}
		}
		byKey[key].base = append(byKey[key].base, declaration)
	}
	for _, declaration := range headDeclarations {
		key := declaration.Kind + "\x00" + declaration.Name
		if byKey[key] == nil {
			byKey[key] = &candidate{}
		}
		byKey[key].head = append(byKey[key].head, declaration)
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]Unit, 0, len(keys))
	for _, key := range keys {
		matches := byKey[key]
		if len(matches.base) > 1 || len(matches.head) > 1 {
			parts := strings.SplitN(key, "\x00", 2)
			for i, declaration := range matches.base {
				result = append(result, Unit{
					ID:       unitID(UnitDeclaration, change.BasePath, "", key+"\x00base"+fmt.Sprint(i)),
					Kind:     UnitDeclaration,
					Language: "go",
					Symbol:   parts[1],
					Base:     &Span{Path: change.BasePath, StartLine: declaration.StartLine, EndLine: declaration.EndLine},
					Change:   change.Kind,
				})
			}
			for i, declaration := range matches.head {
				result = append(result, Unit{
					ID:       unitID(UnitDeclaration, "", change.HeadPath, key+"\x00head"+fmt.Sprint(i)),
					Kind:     UnitDeclaration,
					Language: "go",
					Symbol:   parts[1],
					Head:     &Span{Path: change.HeadPath, StartLine: declaration.StartLine, EndLine: declaration.EndLine},
					Change:   change.Kind,
				})
			}
			continue
		}
		count := len(matches.base)
		if len(matches.head) > count {
			count = len(matches.head)
		}
		for i := 0; i < count; i++ {
			parts := strings.SplitN(key, "\x00", 2)
			unit := Unit{ID: unitID(UnitDeclaration, change.BasePath, change.HeadPath, key+fmt.Sprint(i)), Kind: UnitDeclaration, Language: "go", Symbol: parts[1], Change: change.Kind}
			if i < len(matches.base) {
				declaration := matches.base[i]
				unit.Base = &Span{Path: change.BasePath, StartLine: declaration.StartLine, EndLine: declaration.EndLine}
				if declaration.Bytes > int(a.maxASTSourceBytes) {
					unit.Oversized, unit.NeedsEnclosing = true, true
				}
			}
			if i < len(matches.head) {
				declaration := matches.head[i]
				unit.Head = &Span{Path: change.HeadPath, StartLine: declaration.StartLine, EndLine: declaration.EndLine}
				if declaration.Bytes > int(a.maxASTSourceBytes) {
					unit.Oversized, unit.NeedsEnclosing = true, true
				}
			}
			result = append(result, unit)
		}
	}
	return result, nil
}

func (a *Analyzer) isExcludedDirectory(filePath string) bool {
	for _, segment := range strings.Split(filePath, "/") {
		if _, excluded := a.excludedDirs[segment]; excluded {
			return true
		}
	}
	return false
}

func (a *Analyzer) isGenerated(filePath string) bool {
	base := path.Base(filePath)
	if strings.HasPrefix(base, "zz_generated.") {
		return true
	}
	for _, suffix := range a.generatedSuffixes {
		if strings.HasSuffix(filePath, suffix) {
			return true
		}
	}
	return false
}

func languageForPath(filePath string) string {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".py":
		return "python"
	case ".java":
		return "java"
	default:
		return "text"
	}
}

func unitID(kind UnitKind, basePath, headPath, symbol string) review.ReviewUnitID {
	identityPath := basePath
	if identityPath == "" {
		identityPath = headPath
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"review-unit-v1", string(kind), identityPath, symbol}, "\x00")))
	return review.ReviewUnitID("u_" + hex.EncodeToString(digest[:16]))
}
