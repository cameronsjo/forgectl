//go:build unix

package sops

// SIGHUP to forgectl and a REAL sops together, parked inside sops' editor
// span (cameronsjo/forgectl#692 item 2).
//
// The other signal tests use fake runners, which cannot hold a real sops
// open with its decrypted copy of the whole document on disk. This one
// drives the real runner, the real sops, and a forgectl built from this tree
// as the editor. A wrapper around that forgectl is the parking seam: it runs
// the real `__sops-edit`, so the value is written into sops' decrypted copy,
// then records that copy's path and sleeps. A terminal close sends SIGHUP to
// the whole foreground process group, so the test sends it to the group, and
// sops, the wrapper, and forgectl all receive it.
//
//   [x] The window was real: sops' decrypted copy sat inside the work
//       directory holding the new value, and the staged value file existed
//   [x] forgectl died BY SIGHUP
//   [x] No plaintext survives anywhere under the repository or in the
//       inherited TMPDIR, neither the new value nor the rest of the document
//   [x] The work directory is gone, the target is unchanged, and its pre-run
//       ciphertext is kept beside it
//
// Gated like the other integration tests: FORGECTL_REQUIRE_SOPS_INTEGRATION=1
// turns a missing sops, age-keygen, or go into a failure rather than a skip.

import (
	"context"
	"errors"
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

// sighupChildEnv carries the repo to the child copy of this test binary, and
// sighupEditorEnv the parking wrapper it must use as its editor.
const (
	sighupChildEnv  = "FORGECTL_SOPS_SIGHUP_CHILD"
	sighupEditorEnv = "FORGECTL_SOPS_SIGHUP_EDITOR"
)

// sighupValue is the value the child writes. It is what must not survive.
const sighupValue = "fresh-sighup-value"

// runSighupChild is the child's body: a real SetValue whose editor is the
// parking wrapper. It never returns normally.
func runSighupChild(repo, editor string) {
	executablePath = func() (string, error) { return editor, nil }
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		_, _ = os.Stdout.WriteString("RESOLVE " + err.Error() + "\n")
		os.Exit(3)
	}
	_, err = NewClient(fcexec.NewOSSensitiveRunner()).SetValue(context.Background(), target, "agentgateway.llm_key_hermes", sighupValue)
	_, _ = os.Stdout.WriteString("RETURNED\n")
	_ = err
	os.Exit(5)
}

func TestIntegration_SighupDuringRealSopsEdit(t *testing.T) {
	if repo := os.Getenv(sighupChildEnv); repo != "" {
		runSighupChild(repo, os.Getenv(sighupEditorEnv))
		return
	}
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored in this process, so the child inherits the ignore")
	}

	repo, target := sopsFixture(t)
	buildForgectl(t)
	forgectl, err := executablePath()
	if err != nil {
		t.Fatalf("executablePath: %v", err)
	}
	before, err := os.ReadFile(target.Abs()) //nolint:gosec // G304: a fixture this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	scratch := t.TempDir()
	marker := filepath.Join(scratch, "editing")
	wrapper := filepath.Join(scratch, "park.sh")
	// $1 is __sops-edit and $2 is sops' decrypted copy. The real editor runs
	// first, so the copy holds the new value when the signal lands.
	script := "#!/bin/sh\n'" + forgectl + "' \"$@\" || exit $?\n" +
		"printf '%s' \"$2\" > '" + marker + ".tmp'\nmv '" + marker + ".tmp' '" + marker + "'\nexec sleep 60\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile wrapper: %v", err)
	}
	outer := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIntegration_SighupDuringRealSopsEdit$") //nolint:gosec // G204: re-exec of this test binary
	cmd.Env = append(os.Environ(), sighupChildEnv+"="+repo, sighupEditorEnv+"="+wrapper, "TMPDIR="+outer)
	// Its own process group, which the test signals as a terminal would.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	var edited string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Clean(marker)); err == nil {
			edited = string(b)
			break
		}
	}
	if edited == "" {
		t.Fatal("sops never parked in the editor")
	}
	// The first component of the copy's path below the target's directory is
	// the work directory, however deep sops nests the copy inside it.
	rel, err := filepath.Rel(filepath.Dir(target.Abs()), edited)
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	if err != nil || !strings.HasPrefix(first, target.SopsWorkDirPattern()) || first == rel {
		t.Fatalf("sops' decrypted copy is at %s, not inside a work directory beside the target", edited)
	}
	work := filepath.Join(filepath.Dir(target.Abs()), first)
	doc, err := os.ReadFile(filepath.Clean(edited))
	if err != nil || !strings.Contains(string(doc), sighupValue) || !strings.Contains(string(doc), "untouched") {
		t.Fatalf("the window was not real: sops' copy does not hold the edited document (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(work, "value")); err != nil {
		t.Fatalf("the window was not real: no staged value: %v", err)
	}

	if err := syscall.Kill(-pgid, syscall.SIGHUP); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGHUP {
		t.Errorf("forgectl status %v; want death by SIGHUP", cmd.ProcessState)
	}
	// Every process in the group must be gone before the disk is read, or a
	// late writer could make the assertions below pass early.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a process in the group outlived SIGHUP")
		}
	}

	for _, secret := range []string{sighupValue, "seedvalue", "untouched"} {
		assertNoPlaintextUnder(t, repo, secret)
		assertNoPlaintextUnder(t, outer, secret)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("the work directory survived SIGHUP: %s (stat err %v)", work, err)
	}
	after, err := os.ReadFile(target.Abs()) //nolint:gosec // G304: a fixture this test created
	if err != nil || string(after) != string(before) {
		t.Errorf("the target changed under a SIGHUP in the editor span (%v)", err)
	}
	kept, err := os.ReadFile(filepath.Clean(target.SopsBackupPath()))
	if err != nil || string(kept) != string(before) {
		t.Errorf("the pre-run ciphertext was not kept at %s (%v)", target.SopsBackupPath(), err)
	}
}
