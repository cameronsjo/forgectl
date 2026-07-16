package workflow

import (
	"errors"
	"testing"
)

// TestAcquireRunLock_ExclusiveAndReleasable documents the advisory-lock contract
// the CLI relies on: a second acquire while the first is held is refused with
// ErrWorkflowRunning, and releasing frees it for re-acquisition. flock keys on
// the open file description, so two opens in one process contend — the
// concurrent-run case in miniature.
func TestAcquireRunLock_ExclusiveAndReleasable(t *testing.T) {
	redirectStateDir(t)

	first, err := AcquireRunLock("demo")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := AcquireRunLock("demo"); !errors.Is(err, ErrWorkflowRunning) {
		t.Fatalf("second acquire while held must fail with ErrWorkflowRunning, got %v", err)
	}

	// A different workflow name is an independent lock.
	other, err := AcquireRunLock("other")
	if err != nil {
		t.Fatalf("a different workflow must lock independently, got %v", err)
	}
	other.Release()

	first.Release()
	regained, err := AcquireRunLock("demo")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	regained.Release()

	// Release is idempotent-safe on an already-released lock.
	regained.Release()
}
