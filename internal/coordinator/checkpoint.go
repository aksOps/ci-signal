package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"ci-signal/internal/copilot"
	"ci-signal/internal/review"
)

type checkpointStore struct{ directory string }

type checkpoint struct {
	Fingerprint review.Fingerprint `json:"fingerprint"`
	BatchKey    string             `json:"batch_key"`
	Result      copilot.Result     `json:"result"`
}

func newCheckpointStore(directory string) (*checkpointStore, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("checkpoint directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect checkpoint directory: %w", err)
	}
	return &checkpointStore{directory: directory}, nil
}

func (s *checkpointStore) load(fingerprint review.Fingerprint, batchKey string) (*copilot.Result, error) {
	raw, err := os.ReadFile(s.path(fingerprint, batchKey))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved checkpoint
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, fmt.Errorf("decode review checkpoint: %w", err)
	}
	if saved.Fingerprint != fingerprint || saved.BatchKey != batchKey || saved.Result.Accepted == nil {
		return nil, errors.New("review checkpoint is incompatible or incomplete")
	}
	if saved.Result.Accepted.Value.Completion != review.SubmissionComplete || !completeCoverage(saved.Result.Accepted.Value.Coverage) {
		return nil, nil
	}
	return &saved.Result, nil
}

func (s *checkpointStore) save(fingerprint review.Fingerprint, batchKey string, result copilot.Result) error {
	if result.Accepted == nil {
		return errors.New("cannot checkpoint an unaccepted review result")
	}
	raw, err := json.Marshal(checkpoint{Fingerprint: fingerprint, BatchKey: batchKey, Result: result})
	if err != nil {
		return err
	}
	path := s.path(fingerprint, batchKey)
	temporary, err := os.CreateTemp(s.directory, ".checkpoint-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func (s *checkpointStore) path(fingerprint review.Fingerprint, batchKey string) string {
	digest := sha256.Sum256([]byte(string(fingerprint) + "\x00" + batchKey))
	return filepath.Join(s.directory, hex.EncodeToString(digest[:])+".json")
}
