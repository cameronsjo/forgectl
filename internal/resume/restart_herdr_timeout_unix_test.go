//go:build unix

package resume

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// deadlineRunner records how far off the deadline of each Run call's context
// was when the call arrived.
type deadlineRunner struct {
	exec.Runner
	left time.Duration
	ok   bool
}

func (r *deadlineRunner) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	d, ok := ctx.Deadline()
	r.left, r.ok = time.Until(d), ok
	return "", nil
}

// TestHerdrCallTimeoutPinned pins the production bound on a herdr call and
// that an env without its own bound uses it: a longer bound leaves a stopped
// session waiting on a wedged herdr for longer, a much shorter one fails a
// healthy but slow reply.
func TestHerdrCallTimeoutPinned(t *testing.T) {
	if HerdrCallTimeout != 10*time.Second {
		t.Fatalf("HerdrCallTimeout = %s, want 10s", HerdrCallTimeout)
	}
	r := &deadlineRunner{}
	e := SystemRestartEnv{runner: r, forgectl: "/usr/local/bin/forgectl"}
	if err := e.ClearInput(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	if !r.ok || r.left > HerdrCallTimeout || r.left < HerdrCallTimeout-time.Second {
		t.Fatalf("herdr call deadline: set=%v, %s left; want about %s", r.ok, r.left, HerdrCallTimeout)
	}
}

// hungHerdrScript answers pane get and process-info with a pane whose shell
// (pid 42) holds the foreground. It hangs on the subcommand named in
// $dir/hang, recording its pid (its process group's id) first, and fails at
// once, as herdr refusing a call does, on the one named in $dir/fail.
const hungHerdrScript = `#!/bin/sh
dir=$(dirname "$0")
if [ "$2" = "$(cat "$dir/hang" 2>/dev/null)" ]; then
	echo $$ > "$dir/hung.pid"
	exec sleep 30
fi
if [ "$2" = "$(cat "$dir/fail" 2>/dev/null)" ]; then
	echo '{"error":{"code":"internal"}}' >&2
	exit 1
fi
case "$2" in
get) echo '{"result":{"pane":{}}}' ;;
process-info) echo '{"result":{"process_info":{"foreground_process_group_id":42,"shell_pid":42}}}' ;;
esac
`

// stoppedSessionEnv is the real herdr side of SystemRestartEnv with the
// process side faked: the signal succeeds, the pid is gone with its registry
// file, and nothing has resumed the session.
type stoppedSessionEnv struct{ SystemRestartEnv }

func (stoppedSessionEnv) Terminate(int) error                      { return nil }
func (stoppedSessionEnv) Alive(int) bool                           { return false }
func (stoppedSessionEnv) ReadEntry(int) (RegistryEntry, bool)      { return RegistryEntry{}, false }
func (stoppedSessionEnv) LiveSession(string) (RegistryEntry, bool) { return RegistryEntry{}, false }

// TestRestartWedgedHerdrAfterStopIsBounded: once a session is stopped the run
// ignores cancellation, and each herdr call is in a group of its own, so
// only the per-call bound stands between a wedged herdr and a run that hangs
// forever with the session stopped. At the bound the session is reported
// failed with the command to resume it by hand; a pane run killed there is
// reported as a delivery nobody can vouch for, not as a failed send
// (forgectl#951), and one herdr refused stays a failed send.
//
// The bound is 2s so the fake's instant answers to the other calls clear it
// by seconds even on a loaded host; only the wedged call reaches it.
func TestRestartWedgedHerdrAfterStopIsBounded(t *testing.T) {
	cases := map[string]struct{ file, sub, want string }{
		"send-keys hangs": {"hang", "send-keys", "clearing pane p1's input line failed"},
		"run hangs":       {"hang", "run", "timed out and may or may not have arrived"},
		"run refused":     {"fail", "run", "sending `"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.sub), 0o600); err != nil {
				t.Fatal(err)
			}
			herdr := filepath.Join(dir, "herdr")
			if err := os.WriteFile(herdr, []byte(hungHerdrScript), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(herdr, 0o700); err != nil { //nolint:gosec // G302: the fake herdr must be executable
				t.Fatal(err)
			}
			t.Cleanup(func() { killRecordedGroup(filepath.Join(dir, "hung.pid")) })

			const bound = 2 * time.Second
			env := stoppedSessionEnv{SystemRestartEnv{
				runner: exec.OSRunner{}, forgectl: "/usr/local/bin/forgectl",
				herdr: herdr, callTimeout: bound,
			}}
			s := OutdatedSession{SessionID: "0", Pid: 4242, Pane: "p1"}
			opts := RestartOptions{Tick: 10 * time.Millisecond, ConfirmWait: 100 * time.Millisecond}
			opts.fill()

			done := make(chan RestartEvent, 1)
			start := time.Now()
			go func() { done <- restartNow(context.Background(), env, s, 42, opts) }()
			var ev RestartEvent
			select {
			case ev = <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("herdr %s: the run hung past 10s (bound %s)", tc.sub, bound)
			}
			took := time.Since(start)
			if tc.file == "hang" && took < bound {
				t.Fatalf("returned in %s, before the %s bound: herdr %s did not hang", took, bound, tc.sub)
			}
			if tc.file == "fail" && took >= bound {
				t.Fatalf("returned in %s: a refused call must not wait out the %s bound", took, bound)
			}
			if ev.State != StateFailed || !strings.Contains(ev.Detail, tc.want) {
				t.Fatalf("event %+v; want failed with %q", ev, tc.want)
			}
			if tc.file == "fail" && strings.Contains(ev.Detail, "may or may not") {
				t.Fatalf("event %+v; a refused send is not a delivery in doubt", ev)
			}
			if ev.Manual != ManualResume("0") {
				t.Fatalf("manual = %q, want %q", ev.Manual, ManualResume("0"))
			}
		})
	}
}

// killRecordedGroup SIGKILLs the process group whose id a fake herdr wrote to
// pidFile, if it wrote one; a test that failed must not leave it running.
func killRecordedGroup(pidFile string) {
	data, err := os.ReadFile(filepath.Clean(pidFile))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
