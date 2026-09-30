//go:build unix

package resume

import (
	"bytes"
	"context"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// sighupHelperEnv names the scratch dir of a re-executed test binary playing
// a restart run; unset, the helper test is a no-op.
const (
	sighupHelperEnv = "FORGECTL_TEST_SIGHUP_HELPER_DIR"
	sighupCallEnv   = "FORGECTL_TEST_SIGHUP_HELPER_CALL"
)

// fakeHerdrScript, on the subcommand named in $dir/slow only, records its pid
// (its process group's id), marks its start, and cannot finish until the test
// creates $dir/go, which it does only after sending the hangup: the call is in
// flight when the signal lands by construction, never by timing. It gives up
// after about 10s. It answers pane get / process-info with the minimal JSON
// their parsers accept.
const fakeHerdrScript = `#!/bin/sh
dir=$(dirname "$0")
if [ "$2" = "$(cat "$dir/slow")" ]; then
	echo $$ > "$dir/herdr.pid"
	: > "$dir/started"
	i=0
	until [ -e "$dir/go" ]; do
		i=$((i+1))
		[ "$i" -gt 200 ] && exit 3
		sleep 0.05
	done
	: > "$dir/done"
fi
case "$2" in
get) echo '{"result":{"pane":{}}}' ;;
process-info) echo '{"result":{"process_info":{}}}' ;;
esac
`

// sighupCalls maps each herdr subcommand SystemRestartEnv runs to the method
// that runs it.
var sighupCalls = map[string]func(context.Context, SystemRestartEnv) error{
	"get":          func(ctx context.Context, e SystemRestartEnv) error { _, err := e.Pane(ctx, "p1"); return err },
	"process-info": func(ctx context.Context, e SystemRestartEnv) error { _, err := e.Pane(ctx, "p1"); return err },
	"read":         func(ctx context.Context, e SystemRestartEnv) error { _, err := e.Screen(ctx, "p1"); return err },
	"send-keys":    func(ctx context.Context, e SystemRestartEnv) error { return e.ClearInput(ctx, "p1") },
	"run":          func(ctx context.Context, e SystemRestartEnv) error { return e.Relaunch(ctx, "p1", "0") },
}

// TestRestartHerdrCallSurvivesSIGHUP: a SIGHUP delivered to a restart run's
// process group, as a closed terminal delivers it, does not kill the herdr
// call in flight (forgectl#877). The run is this test binary re-executed in a
// group of its own, owning SIGHUP the way RestartOutdated's HandleSignals
// does; every herdr subcommand the restart env runs is covered.
func TestRestartHerdrCallSurvivesSIGHUP(t *testing.T) {
	for sub := range sighupCalls {
		t.Run(sub, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "slow"), []byte(sub), 0o600); err != nil {
				t.Fatal(err)
			}
			herdr := filepath.Join(dir, "herdr")
			if err := os.WriteFile(herdr, []byte(fakeHerdrScript), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(herdr, 0o700); err != nil { //nolint:gosec // G302: the fake herdr must be executable
				t.Fatal(err)
			}
			t.Cleanup(func() { killRecordedGroup(filepath.Join(dir, "herdr.pid")) })
			cmd := osexec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRestartHerdrSIGHUPHelper$", "-test.count=1") //nolint:gosec // G204, G702: re-executes this test binary
			cmd.Env = append(os.Environ(), sighupHelperEnv+"="+dir, sighupCallEnv+"="+sub)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			out := &bytes.Buffer{}
			// One writer for both: os/exec then serializes the writes.
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if !waitForFile(filepath.Join(dir, "started"), 10*time.Second) {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
				t.Fatalf("herdr %s never started; helper output:\n%s", sub, out.String())
			}
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP); err != nil {
				t.Fatal(err)
			}
			// kill(2) has made the hangup pending on every member of the
			// group by the time it returns, so a herdr in the run's group
			// dies of it before it can see this file.
			if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("the run's herdr %s call died with the hangup: %v\n%s", sub, err, out.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "done")); err != nil {
				t.Fatalf("herdr %s did not finish: %v", sub, err)
			}
		})
	}
}

// TestRestartHerdrSIGHUPHelper is the re-executed run for
// TestRestartHerdrCallSurvivesSIGHUP; it does nothing on its own.
func TestRestartHerdrSIGHUPHelper(t *testing.T) {
	dir := os.Getenv(sighupHelperEnv)
	if dir == "" {
		t.Skip("helper for TestRestartHerdrCallSurvivesSIGHUP")
	}
	call, ok := sighupCalls[os.Getenv(sighupCallEnv)]
	if !ok {
		t.Fatalf("unknown call %q", os.Getenv(sighupCallEnv))
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	e := SystemRestartEnv{runner: exec.OSRunner{}, forgectl: "/usr/local/bin/forgectl", herdr: filepath.Join(dir, "herdr")}
	if err := call(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hup:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never saw the SIGHUP")
	}
}

// waitForFile polls for path until it exists or d passes.
func waitForFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
