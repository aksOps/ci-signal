package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcceptorPersistsBeforeReceiptAndRejectsConflictingRetry(t *testing.T) {
	validator := mustValidator(t)
	stateDir := t.TempDir()
	runDir := filepath.Join(stateDir, "submissions", "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	acceptor, err := NewAcceptor(validator, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata := AcceptanceMetadata{RunID: "run-1", SubmissionID: "submission-1", AcceptedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	raw := []byte(`{"verdict":"approved","completion":"complete","findings":[],"reassessments":[],"acknowledgement_changes":[],"coverage":[{"unit_id":"unit-1","outcome":"complete"}],"limitations":[]}`)
	accepted, err := acceptor.Accept(context.Background(), raw, testAssignment(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Receipt.Digest == "" {
		t.Fatal("receipt digest is empty")
	}
	path := filepath.Join(acceptor.stateDir, "submissions", "run-1", "submission-1.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("persisted file mode/error = %v, %v", info, err)
	}
	if info, err := os.Stat(runDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("submission directory mode/error = %v, %v", info, err)
	}
	if _, err := acceptor.Accept(context.Background(), raw, testAssignment(), metadata); err != nil {
		t.Fatalf("identical retry failed: %v", err)
	}
	conflict := []byte(`{"verdict":"needs_review","completion":"complete","findings":[],"reassessments":[],"acknowledgement_changes":[],"coverage":[{"unit_id":"unit-1","outcome":"complete"}],"limitations":[]}`)
	if _, err := acceptor.Accept(context.Background(), conflict, testAssignment(), metadata); err == nil {
		t.Fatal("conflicting retry succeeded")
	}
}
