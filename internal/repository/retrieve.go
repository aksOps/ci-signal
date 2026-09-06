package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"ci-signal/internal/review"
)

func (a *Analyzer) ReadSource(ctx context.Context, snapshot Snapshot, side Side, filePath string, offset int64, limit int) (EvidenceChunk, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return EvidenceChunk{}, err
	}
	if err := a.validateReviewPath(filePath); err != nil {
		return EvidenceChunk{}, err
	}
	commit, err := snapshot.commit(side)
	if err != nil {
		return EvidenceChunk{}, err
	}
	object, _, err := a.blobObject(ctx, commit, filePath)
	if err != nil {
		return EvidenceChunk{}, err
	}
	sizeRaw, err := a.runGit(ctx, 64, "cat-file", "-s", object)
	if err != nil {
		return EvidenceChunk{}, err
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(sizeRaw)), 10, 64)
	if err != nil {
		return EvidenceChunk{}, fmt.Errorf("parse Git blob size: %w", err)
	}
	chunk, err := a.runGitChunk(ctx, offset, limit, "cat-file", "blob", object)
	if err != nil {
		return EvidenceChunk{}, err
	}
	chunk.TotalBytes = total
	chunk.Complete = chunk.NextOffset == 0
	return EvidenceChunk{Chunk: chunk, Reference: sourceReference(snapshot, review.SourceRepositorySource, side, filePath, offset, chunk.Data)}, nil
}

func (a *Analyzer) ReadDiff(ctx context.Context, snapshot Snapshot, filePath string, offset int64, limit int) (EvidenceChunk, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return EvidenceChunk{}, err
	}
	if err := a.validateReviewPath(filePath); err != nil {
		return EvidenceChunk{}, err
	}
	// The tool accepts one file, never a directory or an unbounded full diff.
	if _, _, err := a.blobObject(ctx, snapshot.HeadCommit, filePath); err != nil {
		if _, _, baseErr := a.blobObject(ctx, snapshot.BaseCommit, filePath); baseErr != nil {
			return EvidenceChunk{}, err
		}
	}
	arguments := []string{"diff", "--no-ext-diff", "--no-textconv", "--find-renames=50%", "--unified=80", snapshot.BaseCommit, snapshot.HeadCommit, "--", ":(literal)" + filePath}
	chunk, err := a.runGitChunk(ctx, offset, limit, arguments...)
	if err != nil {
		return EvidenceChunk{}, err
	}
	return EvidenceChunk{Chunk: chunk, Reference: sourceReference(snapshot, review.SourceRepositoryDiff, SideHead, filePath, offset, chunk.Data)}, nil
}

func (a *Analyzer) Search(ctx context.Context, snapshot Snapshot, side Side, query string, paths []string, offset int64, limit int) (EvidenceChunk, error) {
	if err := a.validateSnapshot(snapshot); err != nil {
		return EvidenceChunk{}, err
	}
	if query == "" || strings.ContainsRune(query, 0) {
		return EvidenceChunk{}, errors.New("search query must be nonempty and cannot contain NUL")
	}
	commit, err := snapshot.commit(side)
	if err != nil {
		return EvidenceChunk{}, err
	}
	arguments := []string{"grep", "-n", "-z", "-I", "-F", "-e", query, commit, "--"}
	for _, filePath := range paths {
		if err := a.validateReviewPath(filePath); err != nil {
			return EvidenceChunk{}, err
		}
		arguments = append(arguments, ":(literal)"+filePath)
	}
	excluded, err := a.excludedSearchPaths(ctx, commit)
	if err != nil {
		return EvidenceChunk{}, err
	}
	arguments = append(arguments, excluded...)
	chunk, err := a.runGitChunk(ctx, offset, limit, arguments...)
	if err != nil && strings.Contains(err.Error(), "exit status 1") {
		chunk = Chunk{Offset: offset, Complete: true, TotalBytes: 0}
		err = nil
	}
	if err != nil {
		return EvidenceChunk{}, err
	}
	subject := query + "\x00" + strings.Join(paths, "\x00")
	return EvidenceChunk{Chunk: chunk, Reference: sourceReference(snapshot, review.SourceRepositorySearch, side, subject, offset, chunk.Data)}, nil
}

func sourceReference(snapshot Snapshot, kind review.SourceReferenceKind, side Side, subject string, offset int64, data []byte) review.SourceReference {
	commit, _ := snapshot.commit(side)
	digest := sha256.Sum256([]byte(strings.Join([]string{string(snapshot.ID), string(kind), string(side), subject, strconv.FormatInt(offset, 10), hex.EncodeToString(dataDigest(data))}, "\x00")))
	filePath := subject
	if kind == review.SourceRepositorySearch {
		filePath = ""
	}
	return review.SourceReference{ID: review.SourceReferenceID("src_" + hex.EncodeToString(digest[:16])), Kind: kind, Commit: commit, Path: filePath}
}

func dataDigest(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

func (a *Analyzer) blobObject(ctx context.Context, commit, filePath string) (string, string, error) {
	cleaned, err := literalPath(filePath)
	if err != nil {
		return "", "", err
	}
	raw, err := a.runGit(ctx, a.maxMetadataBytes, "ls-tree", "-z", commit, "--", cleaned)
	if err != nil {
		return "", "", err
	}
	entries := bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0})
	if len(entries) != 1 || len(entries[0]) == 0 {
		return "", "", fmt.Errorf("path %q is absent from commit %s", cleaned, commit)
	}
	metadata, actualPath, ok := bytes.Cut(entries[0], []byte{'\t'})
	if !ok || string(actualPath) != cleaned {
		return "", "", fmt.Errorf("Git returned an unexpected path for %q", cleaned)
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 || fields[1] != "blob" || !commitPattern.MatchString(fields[2]) {
		return "", "", fmt.Errorf("%w at path %q", ErrUnsupportedObject, cleaned)
	}
	return fields[2], fields[0], nil
}

func literalPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path %q is not a literal repository-relative path", value)
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned != value || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q is not a literal repository-relative path", value)
	}
	return cleaned, nil
}
