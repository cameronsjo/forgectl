//go:build unix

package sops

// The real-signal test: a child copy of this test binary runs SetValue for
// real, parks inside the sops edit call with the plaintext value staged in the
// work directory, and the parent delivers a real SIGINT or SIGTERM to it.
//
//   [x] The work directory, plaintext included, is gone after the signal
//   [x] The child died BY the signal it received (re-raised), not by exit;
//       for SIGQUIT, by Go's own default (goroutine dump, exit 2)
//   [x] Every signal in guardedSignals on unix: SIGTERM, SIGINT, SIGHUP, SIGQUIT
//   [x] The window was real: the staged value existed when the signal went in
//
// sops itself is not needed. The runner is a fake that blocks, which is the
// point: the window under test is forgectl's, and a fake that parks forever
// holds it open for as long as the parent needs.

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/env"
	fcexec "github.com/cameronsjo/forgectl/internal/exec"
)

const signalChildEnv = "FORGECTL_SOPS_SIGNAL_CHILD"

// A SOPS-shaped document with no real ciphertext: IsSOPSFile and
// ReadPlaintextRules are all setLocked asks of it before staging.
const signalFixture = "a: b\nsops:\n    unencrypted_suffix: _unencrypted\n"

// parkingRunner stands in for sops. It announces that the value is staged,
// then blocks until the process is killed.
type parkingRunner struct{ repo string }

func (r parkingRunner) RunSensitive(context.Context, fcexec.SensitiveCommand) (fcexec.SensitiveResult, error) {
	dir := findWorkDir(r.repo)
	if dir == "" {
		os.Stdout.WriteString("NOWORKDIR\n")
		os.Exit(3)
	}
	if _, err := os.Stat(filepath.Join(dir, "value")); err != nil {
		os.Stdout.WriteString("NOVALUE\n")
		os.Exit(3)
	}
	os.Stdout.WriteString("READY\n")
	time.Sleep(time.Minute)
	os.Exit(4)
	return fcexec.SensitiveResult{}, nil
}

func findWorkDir(repo string) string {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".forgectl-sops-") {
			return filepath.Join(repo, e.Name())
		}
	}
	return ""
}

// runSignalChild is the child's body: SetValue against the fixture, through
// the parking runner. It never returns normally.
func runSignalChild(repo string) {
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		os.Stdout.WriteString("RESOLVE " + err.Error() + "\n")
		os.Exit(3)
	}
	_, err = NewClient(parkingRunner{repo: repo}).SetValue(context.Background(), target, "a", "s3cr3t-value")
	os.Stdout.WriteString("RETURNED\n")
	_ = err
	os.Exit(5)
}

func TestSignalDuringPlaintextWindowRemovesWorkDir(t *testing.T) {
	if repo := os.Getenv(signalChildEnv); repo != "" {
		runSignalChild(repo)
		return
	}

	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		// goExit2 marks SIGQUIT: after the guard re-raises it, the Go
		// runtime's default takes over, which dumps goroutines and exits 2
		// rather than dying by the signal. That is also what an unguarded
		// forgectl does, so the status cannot tell the two apart — the
		// work-directory assertion is what does.
		goExit2 bool
	}{
		{"SIGTERM", syscall.SIGTERM, false},
		{"SIGINT", syscall.SIGINT, false},
		{"SIGHUP", syscall.SIGHUP, false},
		{"SIGQUIT", syscall.SIGQUIT, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A child inherits an ignored signal, and the guard deliberately
			// leaves an inherited ignore alone (nohup's SIGHUP, a background
			// job's SIGINT and SIGQUIT) — so there is nothing to test then.
			if signal.Ignored(tc.sig) {
				t.Skipf("%v is ignored in this process, so the child inherits the ignore", tc.sig)
			}

			repo, err := os.MkdirTemp("", "fcsig")
			if err != nil {
				t.Fatalf("MkdirTemp: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(repo) })
			if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
				t.Fatalf("Mkdir .git: %v", err)
			}
			if err := os.WriteFile(filepath.Join(repo, "secrets.sops.yaml"), []byte(signalFixture), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			// SetValue's first check is a PATH lookup for sops; the
			// parking runner never executes what it finds.
			binDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(binDir, "sops"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: an executable stub
				t.Fatalf("WriteFile sops stub: %v", err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalDuringPlaintextWindowRemovesWorkDir$") //nolint:gosec // G204: re-exec of this test binary
			cmd.Env = append(os.Environ(), signalChildEnv+"="+repo, "GOTRACEBACK=single", "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("StdoutPipe: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}

			ready := false
			var seen []string
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				seen = append(seen, line)
				if line == "READY" {
					ready = true
					break
				}
			}
			if !ready {
				_ = cmd.Wait()
				t.Fatalf("the child never reached the plaintext window; it printed %q", seen)
			}
			dir := findWorkDir(repo)
			if dir == "" {
				t.Fatal("no work directory while the child was parked in the window")
			}

			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("Signal: %v", err)
			}
			// Drain so the child never blocks on a full pipe.
			go func() {
				for scanner.Scan() {
				}
			}()
			_ = cmd.Wait()

			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("unexpected process state %T", cmd.ProcessState.Sys())
			}
			switch {
			case tc.goExit2:
				if ws.Signaled() || ws.ExitStatus() != 2 {
					t.Errorf("child status: signaled=%v signal=%v exit=%d; want Go's SIGQUIT default, exit 2", ws.Signaled(), ws.Signal(), ws.ExitStatus())
				}
			case !ws.Signaled() || ws.Signal() != tc.sig:
				t.Errorf("child status: signaled=%v signal=%v exit=%d; want death by %v", ws.Signaled(), ws.Signal(), ws.ExitStatus(), tc.sig)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the work directory survived the signal: %s (stat err %v)", dir, err)
			}
			assertNoWorkDirLeft(t, repo)
		})
	}
}

// Every guarded signal gets its 128+N fallback status, derived rather than
// tabled, so a signal added to guardedSignals cannot fall through to 1.
func TestExitStatusForGuardedSignals(t *testing.T) {
	want := map[syscall.Signal]int{syscall.SIGINT: 130, syscall.SIGTERM: 143, syscall.SIGHUP: 129, syscall.SIGQUIT: 131}
	for _, sig := range guardedSignals {
		s, ok := sig.(syscall.Signal)
		if !ok {
			t.Fatalf("guarded signal %v is not a syscall.Signal", sig)
		}
		if got := exitStatusFor(sig); got != want[s] {
			t.Errorf("exitStatusFor(%v) = %d, want %d", sig, got, want[s])
		}
	}
}
