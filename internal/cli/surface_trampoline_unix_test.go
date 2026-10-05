//go:build unix

package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// TestTrampoline_SurvivesATerminalSignalAndStillReaps is the Ctrl-C property.
//
// The harness stays in the pane's foreground process group so Ctrl-C reaches
// it, but the trampoline is in that group too — and with no handling installed,
// Go's default disposition kills it mid-Wait. The session is then orphaned on
// the tty and the exit status this file exists to mirror is discarded.
//
// The signal is delivered to this process, which is what a terminal does to the
// whole group. If the handling is removed, this test does not fail politely:
// the test binary itself dies. That is the correct signal — it is exactly what
// happens to the trampoline in a real pane.
func TestTrampoline_SurvivesATerminalSignalAndStillReaps(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	// The harness ignores SIGINT and exits 7 — a claude session traps it and
	// carries on, so the reaper must outlive the signal to see the real status.
	committed := serviceOn(t, listener, nonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", `trap '' INT; sleep 0.4; exit 7`},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     "/",
	})

	go func() {
		// Late enough that Run has installed its handling and is in Wait.
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if runErr == nil {
		t.Fatal("the harness exited 7 but the trampoline reported success")
	}
	if got := ExitCode(runErr); got != 7 {
		t.Errorf("exit code = %d, want 7 — the reaper did not survive the signal "+
			"to observe the harness's real status", got)
	}
	if err := <-committed; err != nil {
		t.Errorf("the service did not commit: %v", err)
	}
}
