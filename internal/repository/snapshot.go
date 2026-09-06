package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ci-signal/internal/review"
)

var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func (a *Analyzer) CaptureSnapshot(ctx context.Context, baseCommit, headCommit string, at time.Time) (Snapshot, error) {
	if !commitPattern.MatchString(baseCommit) || !commitPattern.MatchString(headCommit) {
		return Snapshot{}, errors.New("base and head must be full lowercase Git commit IDs")
	}
	root, err := a.runGit(ctx, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		return Snapshot{}, fmt.Errorf("locate Git repository: %w", err)
	}
	if strings.TrimSpace(string(root)) != a.repositoryReal {
		return Snapshot{}, fmt.Errorf("Git root %q does not match configured repository %q", strings.TrimSpace(string(root)), a.repositoryReal)
	}
	shallowRaw, err := a.runGit(ctx, 32, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return Snapshot{}, err
	}
	shallow := strings.TrimSpace(string(shallowRaw)) == "true"
	for _, commit := range []string{baseCommit, headCommit} {
		resolved, resolveErr := a.runGit(ctx, 128, "rev-parse", "--verify", commit+"^{commit}")
		if resolveErr != nil || strings.TrimSpace(string(resolved)) != commit {
			return Snapshot{}, fmt.Errorf("%w: commit %s unavailable (shallow=%t)", ErrMissingHistory, commit, shallow)
		}
	}
	checkedOutRaw, err := a.runGit(ctx, 128, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve checked-out commit: %w", err)
	}
	checkedOut := strings.TrimSpace(string(checkedOutRaw))
	formatRaw, err := a.runGit(ctx, 32, "rev-parse", "--show-object-format")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve Git object format: %w", err)
	}
	if at.IsZero() {
		return Snapshot{}, errors.New("snapshot timestamp is required")
	}
	format := strings.TrimSpace(string(formatRaw))
	digest := sha256.Sum256([]byte(strings.Join([]string{a.repositoryReal, baseCommit, headCommit, format}, "\x00")))
	return Snapshot{
		ID:                     review.SnapshotID("snapshot-v1:" + hex.EncodeToString(digest[:])),
		RepositoryRoot:         a.repositoryReal,
		BaseCommit:             baseCommit,
		HeadCommit:             headCommit,
		CheckedOutCommit:       checkedOut,
		CheckedOutIsSourceHead: checkedOut == headCommit,
		ObjectFormat:           format,
		Shallow:                shallow,
		CapturedAt:             at.UTC(),
	}, nil
}

func (a *Analyzer) validateSnapshot(snapshot Snapshot) error {
	if snapshot.RepositoryRoot != a.repositoryReal || !commitPattern.MatchString(snapshot.BaseCommit) || !commitPattern.MatchString(snapshot.HeadCommit) || snapshot.ID == "" {
		return ErrStaleSnapshot
	}
	return nil
}

func (snapshot Snapshot) commit(side Side) (string, error) {
	switch side {
	case SideBase:
		return snapshot.BaseCommit, nil
	case SideHead:
		return snapshot.HeadCommit, nil
	default:
		return "", fmt.Errorf("unsupported snapshot side %q", side)
	}
}
