//go:build unix

package sops

// The real-signal test: a child copy of this test binary runs SetValue for
// real, parks inside the sops edit call with the plaintext value staged in the
// work directory, and the parent delivers a real SIGINT or SIGTERM to it.
//
//   [x] The work directory, plaintext included, is gone after the signal
//   [x] The child died BY the signal it received (re-raised), not by exit;
//       for SIGQUIT and SIGABRT, by Go's own default (goroutine dump, exit 2)
//   [x] Every signal in guardedSignals on unix: SIGTERM, SIGINT, SIGHUP,
//       SIGQUIT, SIGABRT — and the table below is checked against that list,
//       so a signal added there cannot go untested
//   [x] The window was real: the staged value existed when the signal went in
//   [x] The signal landed after the sops edit launched, so the ciphertext
//       backup is kept beside the target and no plaintext survives anywhere
//   [x] SIGKILL leaves the work directory; the next set refuses, names it and
//       its backup, and deletes nothing
//   [x] A signal in the read-back (sops already wrote the target) keeps the
//       backup, and the next set refuses with the unverified-value message
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

// signalChildModeEnv selects the child runner's behaviour. Empty parks in the
// edit call with the value staged and the target untouched.
const signalChildModeEnv = "FORGECTL_SOPS_SIGNAL_CHILD_MODE"

// A SOPS-shaped document with no real ciphertext: IsSOPSFile and
// ReadPlaintextRules are all setLocked asks of it before staging.
const signalFixture = "a: b\nsops:\n    unencrypted_suffix: _unencrypted\n"

// parkingRunner stands in for sops. It announces that the value is staged,
// then blocks until the process is killed.
type parkingRunner struct{ repo string }

func (r parkingRunner) RunSensitive(context.Context, fcexec.SensitiveCommand) (fcexec.SensitiveResult, error) {
	dir := findWorkDir(r.repo)
	if dir == "" {
		_, _ = os.Stdout.WriteString("NOWORKDIR\n")
		os.Exit(3)
	}
	if _, err := os.Stat(filepath.Join(dir, "value")); err != nil {
		_, _ = os.Stdout.WriteString("NOVALUE\n")
		os.Exit(3)
	}
	_, _ = os.Stdout.WriteString("READY\n")
	time.Sleep(time.Minute)
	os.Exit(4)
	return fcexec.SensitiveResult{}, nil
}

// findWorkDir returns the work directory, never the preserved backup beside
// it, which shares the prefix.
// childModeVerify parks the child in the read-back instead: the fake edit has
// already rewritten the target, and a decrypted read-back sits in the work
// directory. That is the late-signal window of cameronsjo/forgectl#560.
const childModeVerify = "verify"

// changedFixture is what the fake sops edit leaves in the target. It is
// ciphertext-shaped and carries no plaintext value.
const changedFixture = "a: ENC[AES256_GCM,data:changed,type:str]\nsops:\n    unencrypted_suffix: _unencrypted\n"

type verifyParkingRunner struct{ repo string }

func (r verifyParkingRunner) RunSensitive(_ context.Context, cmd fcexec.SensitiveCommand) (fcexec.SensitiveResult, error) {
	if cmd.Kind == fcexec.KindSopsEdit {
		if err := os.WriteFile(filepath.Join(r.repo, "secrets.sops.yaml"), []byte(changedFixture), 0o600); err != nil {
			_, _ = os.Stdout.WriteString("NOEDIT\n")
			os.Exit(3)
		}
		return fcexec.SensitiveResult{}, nil
	}
	dir := findWorkDir(r.repo)
	if dir == "" {
		_, _ = os.Stdout.WriteString("NOWORKDIR\n")
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(dir, "landed"), []byte("s3cr3t-value"), 0o600); err != nil {
		_, _ = os.Stdout.WriteString("NOLANDED\n")
		os.Exit(3)
	}
	_, _ = os.Stdout.WriteString("READY\n")
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
		if e.IsDir() && strings.HasPrefix(e.Name(), ".forgectl-sops-") {
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
		_, _ = os.Stdout.WriteString("RESOLVE " + err.Error() + "\n")
		os.Exit(3)
	}
	var runner fcexec.SensitiveRunner = parkingRunner{repo: repo}
	if os.Getenv(signalChildModeEnv) == childModeVerify {
		runner = verifyParkingRunner{repo: repo}
	}
	_, err = NewClient(runner).SetValue(context.Background(), target, "a", "s3cr3t-value")
	_, _ = os.Stdout.WriteString("RETURNED\n")
	_ = err
	os.Exit(5)
}

func TestSignalDuringPlaintextWindowRemovesWorkDir(t *testing.T) {
	if repo := os.Getenv(signalChildEnv); repo != "" {
		runSignalChild(repo)
		return
	}

	cases := []struct {
		name string
		sig  syscall.Signal
		// goExit2 marks SIGQUIT and SIGABRT: after the guard re-raises it, the Go
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
		{"SIGABRT", syscall.SIGABRT, true},
	}
	// Every guarded signal must have a row: a signal added to guardedSignals
	// with no real-process test is a claim nobody watched hold.
	for _, g := range guardedSignals {
		found := false
		for _, tc := range cases {
			if tc.sig == g {
				found = true
			}
		}
		if !found {
			t.Errorf("guarded signal %v has no real-signal case", g)
		}
	}
	for _, tc := range cases {
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
			assertNoPlaintextUnder(t, repo, "s3cr3t-value")
			// The child was parked inside the edit call, after the mutation
			// span opened, so it could not know whether sops had written the
			// target: the ciphertext backup must be beside it.
			target, err := env.ResolveTarget("secrets.sops.yaml", repo)
			if err != nil {
				t.Fatalf("ResolveTarget: %v", err)
			}
			defer target.Close()
			got, err := os.ReadFile(filepath.Clean(target.SopsBackupPath()))
			if err != nil || string(got) != signalFixture {
				t.Errorf("kept backup = %q, %v; want the pre-run ciphertext", got, err)
			}
		})
	}
}

// Every guarded signal gets its 128+N fallback status, derived rather than
// tabled, so a signal added to guardedSignals cannot fall through to 1.
func TestExitStatusForGuardedSignals(t *testing.T) {
	want := map[syscall.Signal]int{syscall.SIGINT: 130, syscall.SIGTERM: 143, syscall.SIGHUP: 129, syscall.SIGQUIT: 131, syscall.SIGABRT: 134}
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

// signalRepo builds the fixture repository and a PATH holding a sops stub,
// which SetValue's LookPath needs and the fake runners never execute.
func signalRepo(t *testing.T) (repo, path string) {
	t.Helper()
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
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "sops"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile sops stub: %v", err)
	}
	return repo, binDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// parkChild starts a child SetValue that parks inside the sops edit call with
// the value staged, and returns once it is parked. mode selects what the
// child's runner does; see signalChildModeEnv.
func parkChild(ctx context.Context, t *testing.T, repo, path, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalDuringPlaintextWindowRemovesWorkDir$") //nolint:gosec // G204: re-exec of this test binary
	cmd.Env = append(os.Environ(), signalChildEnv+"="+repo, signalChildModeEnv+"="+mode, "GOTRACEBACK=single", "PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	scanner := bufio.NewScanner(stdout)
	var seen []string
	for scanner.Scan() {
		line := scanner.Text()
		seen = append(seen, line)
		if line == "READY" {
			// Drain so the child never blocks on a full pipe.
			go func() {
				for scanner.Scan() {
				}
			}()
			return cmd
		}
	}
	_ = cmd.Wait()
	t.Fatalf("the child never parked; it printed %q", seen)
	return nil
}

// refusingRunner fails the test if SetValue ever reaches sops.
type refusingRunner struct{ t *testing.T }

func (r refusingRunner) RunSensitive(context.Context, fcexec.SensitiveCommand) (fcexec.SensitiveResult, error) {
	r.t.Error("SetValue reached sops despite a previous run's leftover")
	return fcexec.SensitiveResult{}, nil
}

// SIGKILL runs no handler, so the work directory, plaintext included, stays.
// The next SetValue on the same target must refuse and name it, and must not
// delete it: a sops child that outlived its parent may still be using it.
func TestSigkillLeftoverRefusesNextSet(t *testing.T) {
	repo, path := signalRepo(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := parkChild(ctx, t, repo, path, "")
	dir := findWorkDir(repo)
	if dir == "" {
		t.Fatal("no work directory while the child was parked")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(filepath.Join(dir, "value")); err != nil {
		t.Fatalf("SIGKILL should have left the staged value behind (that is the residual under test): %v", err)
	}

	t.Setenv("PATH", path)
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()
	_, err = NewClient(refusingRunner{t}).SetValue(ctx, target, "a", "another-value")
	if err == nil {
		t.Fatal("SetValue succeeded over a SIGKILLed run's work directory")
	}
	if !strings.Contains(err.Error(), filepath.Base(dir)) {
		t.Errorf("refusal %q does not name the leftover %s", err, filepath.Base(dir))
	}
	if !strings.Contains(err.Error(), filepath.Join(filepath.Base(dir), "backup")) {
		t.Errorf("refusal %q does not name the ciphertext backup inside the leftover", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-value") || strings.Contains(err.Error(), "another-value") {
		t.Errorf("refusal %q carries a value", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "value")); err != nil {
		t.Errorf("the refusal removed the leftover: %v", err)
	}
	got, err := os.ReadFile(filepath.Clean(filepath.Join(repo, "secrets.sops.yaml")))
	if err != nil || string(got) != signalFixture {
		t.Errorf("the target changed under a refusal: %q, %v", got, err)
	}
}

// A signal after sops wrote the target and before forgectl verified it: the
// plaintext goes, the ciphertext backup stays beside the target, and the next
// set refuses and points at it (cameronsjo/forgectl#560).
func TestLateSignalKeepsBackupAndNextSetRefuses(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skipf("%v is ignored in this process, so the child inherits the ignore", sig)
			}
			repo, path := signalRepo(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			cmd := parkChild(ctx, t, repo, path, childModeVerify)
			dir := findWorkDir(repo)
			if dir == "" {
				t.Fatal("no work directory while the child was parked")
			}
			if _, err := os.Stat(filepath.Join(dir, "landed")); err != nil {
				t.Fatalf("the window was not real: no read-back on disk: %v", err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatalf("Signal: %v", err)
			}
			_ = cmd.Wait()
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != sig {
				t.Errorf("child status %v; want death by %v", cmd.ProcessState, sig)
			}

			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the work directory survived: %s (stat err %v)", dir, err)
			}
			assertNoPlaintextUnder(t, repo, "s3cr3t-value")

			t.Setenv("PATH", path)
			target, err := env.ResolveTarget("secrets.sops.yaml", repo)
			if err != nil {
				t.Fatalf("ResolveTarget: %v", err)
			}
			defer target.Close()
			keep := target.SopsBackupPath()
			got, err := os.ReadFile(filepath.Clean(keep))
			if err != nil {
				t.Fatalf("the ciphertext backup was not kept at %s: %v", keep, err)
			}
			if string(got) != signalFixture {
				t.Errorf("the kept backup = %q, want the pre-run ciphertext %q", got, signalFixture)
			}

			_, err = NewClient(refusingRunner{t}).SetValue(ctx, target, "a", "another-value")
			if err == nil {
				t.Fatal("SetValue succeeded with an interrupted run's backup beside the target")
			}
			for _, want := range []string{filepath.Base(keep), "unverified value"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not mention %q", err, want)
				}
			}
			if _, err := os.Stat(keep); err != nil {
				t.Errorf("the refusal removed the backup: %v", err)
			}
		})
	}
}

// assertNoPlaintextUnder fails if any regular file under root holds secret.
func assertNoPlaintextUnder(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return err
		}
		if strings.Contains(string(data), secret) {
			t.Errorf("plaintext survived the signal in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}
