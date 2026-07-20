//go:build unix

package cli

import (
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// TestWorkflowRun_ConcurrentRunRefused proves the advisory lock serializes runs
// of the same workflow: while one holds the lock, a second run fails fast rather
// than clobbering the shared state sidecar, and the lock frees on release. It is
// constrained to unix (matching flock_unix.go) because AcquireRunLock is a
// documented no-op on !unix (flock_other.go), where the second run would not be
// refused.
func TestWorkflowRun_ConcurrentRunRefused(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	held, err := workflow.AcquireRunLock("multi")
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	_, err = execRun(t, &exec.FakeRunner{}, "multi")
	if !errors.Is(err, workflow.ErrWorkflowRunning) {
		t.Fatalf("a run while the lock is held must fail with ErrWorkflowRunning; got %v", err)
	}

	// Once released, a run proceeds normally.
	held.Release()
	if _, err := execRun(t, &exec.FakeRunner{}, "multi"); err != nil {
		t.Fatalf("run after lock release should succeed: %v", err)
	}
}
