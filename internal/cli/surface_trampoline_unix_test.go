//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
	//
	// The harness sends the SIGINT to its parent, this process, itself. It
	// runs only after Run installed its handling and before Run can return,
	// so the signal lands while the reaper is waiting whatever the host's
	// load. A timer in the test raced Run's setup: on a loaded host it could
	// fire before the handling was installed and kill the test binary with
	// no failing test named (forgectl#1120).
	//
	// The harness then exits only once Run's handling has taken the signal
	// (the seen file), not after a fixed sleep. kill returns when the signal
	// is queued; this process handles it later, on its main thread. A harness
	// that exited first let Run return and release the handling before a
	// late signal arrived, and the signal then killed the test binary. The
	// 30 s cap only keeps the harness from waiting forever.
	seen := filepath.Join(t.TempDir(), "seen")
	var once sync.Once
	prev := reaperSignalSeen
	reaperSignalSeen = func(os.Signal) {
		once.Do(func() { _ = os.WriteFile(seen, nil, 0o600) })
	}
	t.Cleanup(func() { reaperSignalSeen = prev })
	committed := serviceOn(t, listener, nonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args: []string{"-c", `trap '' INT; kill -INT "$PPID"; i=0
while [ ! -e "$SEEN" ] && [ "$i" -lt 300 ]; do sleep 0.1; i=$((i+1)); done
exit 7`},
		Env: []string{"PATH=/usr/bin:/bin", "SEEN=" + seen},
		CWD: "/",
	})

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
