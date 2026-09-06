package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var hostIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

type Acceptor struct {
	validator *Validator
	stateDir  string
}

func NewAcceptor(validator *Validator, stateDir string) (*Acceptor, error) {
	if validator == nil {
		return nil, errors.New("validator is required")
	}
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("state directory must be absolute")
	}
	return &Acceptor{validator: validator, stateDir: filepath.Clean(stateDir)}, nil
}

func (a *Acceptor) Accept(ctx context.Context, raw []byte, assignment Assignment, metadata AcceptanceMetadata) (AcceptedSubmission, error) {
	if err := ctx.Err(); err != nil {
		return AcceptedSubmission{}, err
	}
	if !hostIDPattern.MatchString(string(metadata.RunID)) || !hostIDPattern.MatchString(string(metadata.SubmissionID)) {
		return AcceptedSubmission{}, errors.New("host run and submission IDs must be safe nonempty identifiers")
	}
	if metadata.AcceptedAt.IsZero() {
		return AcceptedSubmission{}, errors.New("host acceptance timestamp is required")
	}
	submission, err := a.validator.Validate(raw, assignment)
	if err != nil {
		return AcceptedSubmission{}, err
	}
	canonical, err := json.Marshal(submission)
	if err != nil {
		return AcceptedSubmission{}, fmt.Errorf("encode accepted submission: %w", err)
	}
	digestBytes := sha256.Sum256(canonical)
	digest := hex.EncodeToString(digestBytes[:])
	receipt := SubmissionReceipt{
		RunID:        metadata.RunID,
		SubmissionID: metadata.SubmissionID,
		Digest:       digest,
		AcceptedAt:   metadata.AcceptedAt.UTC(),
	}
	record := AcceptedSubmission{Receipt: receipt, Value: submission, Raw: canonical}
	persisted, err := json.Marshal(record)
	if err != nil {
		return AcceptedSubmission{}, fmt.Errorf("encode accepted submission record: %w", err)
	}
	if err := a.persist(ctx, metadata, persisted); err != nil {
		return AcceptedSubmission{}, err
	}
	return record, nil
}

func (a *Acceptor) persist(ctx context.Context, metadata AcceptanceMetadata, data []byte) error {
	dir := filepath.Join(a.stateDir, "submissions", string(metadata.RunID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create submission state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect submission state directory: %w", err)
	}
	finalPath := filepath.Join(dir, string(metadata.SubmissionID)+".json")
	if existing, err := os.ReadFile(finalPath); err == nil {
		if string(existing) == string(data) {
			return nil
		}
		return fmt.Errorf("accepted submission %q already exists with different content", metadata.SubmissionID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing accepted submission: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".accepted-*")
	if err != nil {
		return fmt.Errorf("create accepted submission temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set accepted submission permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write accepted submission: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync accepted submission: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close accepted submission: %w", err)
	}
	if err := os.Link(temporaryPath, finalPath); err != nil {
		if existing, readErr := os.ReadFile(finalPath); readErr == nil && string(existing) == string(data) {
			return nil
		}
		return fmt.Errorf("install accepted submission: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open accepted submission directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync accepted submission directory: %w", err)
	}
	return nil
}
