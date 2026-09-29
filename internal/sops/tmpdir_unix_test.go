//go:build unix

package sops

// sops' own plaintext temp file (cameronsjo/forgectl#560 item 2).
//
// `sops edit` decrypts the whole document into a file under os.TempDir while
// its editor runs. Measured on 3.13.3: SIGINT and SIGTERM make sops remove it,
// SIGHUP and SIGQUIT do not. So the edit call points sops' TMPDIR at the work
// directory, which the plaintext guard and the leftover scan already cover.
//
//   [x] The edit command carries TMPDIR = the work directory (no sops needed)
//   [x] Against real sops (gated): the decrypted copy is created under the
//       TMPDIR sops is given, which is the premise the mitigation rests on,
//       and after SIGHUP whatever sops leaves is under it and nowhere else
//   [x] Against real sops through SetValue (gated): the edit call's sops
//       process really sees TMPDIR = a work directory beside the target, and
//       no other sops call is redirected

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/env"
	fcexec "github.com/cameronsjo/forgectl/internal/exec"
)

// envRecordingRunner records the edit command's environment mutations and
// the work directory at the moment sops would run, then refuses the edit.
type envRecordingRunner struct {
	repo    string
	env     *[]fcexec.EnvMutation
	workDir *string
}

func (r envRecordingRunner) RunSensitive(_ context.Context, cmd fcexec.SensitiveCommand) (fcexec.SensitiveResult, error) {
	if cmd.Kind == fcexec.KindSopsEdit {
		*r.env = append([]fcexec.EnvMutation(nil), cmd.Env...)
		*r.workDir = findWorkDir(r.repo)
	}
	return fcexec.SensitiveResult{ExitCode: 1}, os.ErrInvalid
}

func TestEditPointsSopsTmpdirAtTheWorkDir(t *testing.T) {
	repo, path := signalRepo(t)
	t.Setenv("PATH", path)
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()

	var got []fcexec.EnvMutation
	var workDir string
	_, _ = NewClient(envRecordingRunner{repo: repo, env: &got, workDir: &workDir}).SetValue(t.Context(), target, "a", "s3cr3t-value")
	if workDir == "" {
		t.Fatal("the edit ran with no work directory beside the target")
	}
	// The work directory is created under target.Abs(), which resolves
	// symlinks: on macOS the repo sits under /var, a link to /private/var, so
	// the directory found through the unresolved repo path spells the same
	// directory differently.
	want := fcexec.ReplaceSopsTmpdir(filepath.Join(filepath.Dir(target.Abs()), filepath.Base(workDir)))
	for _, m := range got {
		if m.Equal(want) {
			return
		}
	}
	t.Errorf("the sops edit does not point TMPDIR at its work directory %s; sops would decrypt the whole document into the system temp directory", workDir)
}

// TestIntegration_SopsTempFileFollowsTmpdir measures the upstream behaviour
// the mitigation depends on, with a parked editor holding the window open.
func TestIntegration_SopsTempFileFollowsTmpdir(t *testing.T) {
	repo, target := sopsFixture(t)
	scratch := t.TempDir()
	tmp := filepath.Join(scratch, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scratch, "editing")
	editor := filepath.Join(scratch, "park.sh")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > '" + marker + ".tmp'\nmv '" + marker + ".tmp' '" + marker + "'\nsleep 60\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile editor: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sops", "--disable-version-check", "edit", target.Abs()) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "EDITOR="+editor)
	// Its own process group, so the parked editor can be reaped with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sops: %v", err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()

	var edited string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Clean(marker)); err == nil {
			edited = string(b)
			break
		}
	}
	if edited == "" {
		t.Fatal("sops never launched the editor")
	}
	rel, err := filepath.Rel(tmp, edited)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("sops put its decrypted copy at %s, outside the TMPDIR it was given (%s); pointing TMPDIR at the work directory would not confine it", edited, tmp)
	}
	if doc, err := os.ReadFile(filepath.Clean(edited)); err != nil || !strings.Contains(string(doc), "seedvalue") {
		t.Fatalf("the editor's file is not the decrypted document (%v); the measurement is not of what it claims", err)
	}

	// SIGHUP, the terminal-close case. Only sops gets it, as in the
	// measurement; the editor is reaped by the deferred group kill.
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(filepath.Clean(edited)); err == nil { //nolint:gosec // G703: the path this test's own editor stub recorded, checked above to lie under the test's TMPDIR
		t.Logf("measured: sops left its decrypted copy after SIGHUP (%s); it is under the given TMPDIR, which is what confines it", edited)
	} else {
		t.Logf("measured: sops removed its decrypted copy on SIGHUP; the TMPDIR mitigation is now belt and braces")
	}
	assertNoPlaintextUnder(t, repo, "seedvalue")
}

// TestIntegration_EditRunsWithTmpdirInWorkDir drives SetValue with real sops
// behind a wrapper that records each call's TMPDIR, proving the mutation
// reaches the sops process rather than only the command struct.
func TestIntegration_EditRunsWithTmpdirInWorkDir(t *testing.T) {
	repo, target := sopsFixture(t)
	buildForgectl(t)

	realSops, err := exec.LookPath("sops")
	if err != nil {
		t.Fatalf("LookPath sops: %v", err)
	}
	wrapDir := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	wrapper := "#!/bin/sh\nprintf '%s %s\\n' \"$TMPDIR\" \"$3\" >> '" + log + "'\nexec '" + realSops + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapDir, "sops"), []byte(wrapper), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile wrapper: %v", err)
	}
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	outer := t.TempDir()
	t.Setenv("TMPDIR", outer)

	if _, err := testClient(t).SetValue(t.Context(), target, "agentgateway.llm_key_hermes", "fresh-value"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	calls, err := os.ReadFile(filepath.Clean(log))
	if err != nil {
		t.Fatalf("the wrapper never ran: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) < 2 {
		t.Fatalf("want an edit call and a read-back call, got %q", lines)
	}
	// Argument 3 is the target on the edit call (--disable-version-check --
	// <file>) and `--extract` on the read-back.
	var sawEdit bool
	for _, line := range lines {
		tmpdir, third, _ := strings.Cut(line, " ")
		if third == target.Abs() {
			sawEdit = true
			if filepath.Dir(tmpdir) != filepath.Dir(target.Abs()) || !strings.HasPrefix(filepath.Base(tmpdir), target.SopsWorkDirPattern()) {
				t.Errorf("the edit's sops ran with TMPDIR=%s, want this run's work directory beside the target", tmpdir)
			}
			continue
		}
		if tmpdir != outer {
			t.Errorf("a non-edit sops call ran with TMPDIR=%s, want the inherited %s", tmpdir, outer)
		}
	}
	if !sawEdit {
		t.Errorf("no recorded call was the edit: %q", lines)
	}
	entries, _ := os.ReadDir(outer)
	for _, e := range entries {
		t.Errorf("the run left %s in the inherited TMPDIR", e.Name())
	}
	assertNoPlaintextUnder(t, repo, "fresh-value")
}
