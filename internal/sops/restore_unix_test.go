//go:build unix

package sops

// The failure paths of SetValue that no signal reaches (cameronsjo/forgectl#652).
//
//   [x] A restore that fails after sops refused the edit keeps the ciphertext
//       backup beside the target, names it in the error, and leaves no
//       plaintext anywhere under the repository
//   [x] The same after verification failed, where sops HAS rewritten the target
//   [x] A sops refusal writes nothing of sops' output anywhere, $TMPDIR
//       included, and the error names no log file
//
// The runners are fakes. The restore is made to fail by replacing the target
// with a directory, which the atomic rename cannot replace. That works under
// root too, unlike a permission-based failure.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/env"
	fcexec "github.com/cameronsjo/forgectl/internal/exec"
)

// restoreBreakingRunner makes the restore impossible at a chosen step.
// failAt is the command kind at which the target becomes a directory and the
// command fails. Before that, an edit rewrites the target and succeeds.
type restoreBreakingRunner struct {
	target string
	failAt fcexec.CommandKind
}

func (r restoreBreakingRunner) RunSensitive(_ context.Context, cmd fcexec.SensitiveCommand) (fcexec.SensitiveResult, error) {
	if cmd.Kind != r.failAt {
		if err := os.WriteFile(r.target, []byte(changedFixture), 0o600); err != nil {
			return fcexec.SensitiveResult{ExitCode: 1}, err
		}
		return fcexec.SensitiveResult{}, nil
	}
	if cmd.Kind == fcexec.KindSopsExtract {
		// The read-back sat on disk before the failure, as a real one would.
		if dir := findWorkDir(filepath.Dir(r.target)); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "landed"), []byte("s3cr3t-value"), 0o600)
		}
	}
	if err := os.Remove(r.target); err != nil {
		return fcexec.SensitiveResult{ExitCode: 1}, err
	}
	if err := os.Mkdir(r.target, 0o700); err != nil {
		return fcexec.SensitiveResult{ExitCode: 1}, err
	}
	return fcexec.SensitiveResult{ExitCode: 1}, errors.New("exit status 1")
}

func TestFailedRestoreKeepsTheBackup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failAt fcexec.CommandKind
	}{
		{"sops refused the edit", fcexec.KindSopsEdit},
		{"verification failed", fcexec.KindSopsExtract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, path := signalRepo(t)
			t.Setenv("PATH", path)
			target, err := env.ResolveTarget("secrets.sops.yaml", repo)
			if err != nil {
				t.Fatalf("ResolveTarget: %v", err)
			}
			defer target.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			runner := restoreBreakingRunner{target: filepath.Join(repo, "secrets.sops.yaml"), failAt: tc.failAt}
			_, err = NewClient(runner).SetValue(ctx, target, "a", "s3cr3t-value")
			if err == nil {
				t.Fatal("SetValue succeeded with a restore that cannot take")
			}
			keep := target.SopsBackupPath()
			for _, want := range []string{"could NOT be restored", filepath.Base(keep)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "has been restored") {
				t.Errorf("error %q claims a restore that failed", err)
			}
			got, readErr := os.ReadFile(filepath.Clean(keep))
			if readErr != nil {
				t.Fatalf("the ciphertext backup was lost after a failed restore: %v", readErr)
			}
			if string(got) != signalFixture {
				t.Errorf("the kept backup = %q, want the pre-run ciphertext %q", got, signalFixture)
			}
			if dir := findWorkDir(repo); dir != "" {
				t.Errorf("the work directory survived though the backup was moved out: %s", dir)
			}
			assertNoPlaintextUnder(t, repo, "s3cr3t-value")
		})
	}
}

// sops' output can quote `key: '<the secret>'`. It used to go to a 0600 file
// under $TMPDIR that nothing ever removed; now it goes nowhere.
func TestSopsRefusalWritesNoOutputFile(t *testing.T) {
	repo, _ := signalRepo(t)
	const quoted = "key: 'captured-s3cr3t'"
	binDir := t.TempDir()
	stub := "#!/bin/sh\necho \"" + quoted + "\" >&2\necho \"" + quoted + "\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "sops"), []byte(stub), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile sops stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, err = NewClient(fcexec.NewOSSensitiveRunner()).SetValue(ctx, target, "a", "s3cr3t-value")
	if err == nil {
		t.Fatal("SetValue succeeded through a sops that exits 1")
	}
	if !strings.Contains(err.Error(), "sops refused the edit") {
		t.Fatalf("error %q is not the sops refusal", err)
	}
	for _, leak := range []string{"captured-s3cr3t", tmp, "forgectl-sops-output"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q", err, leak)
		}
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir TMPDIR: %v", err)
	}
	for _, e := range entries {
		t.Errorf("a sops refusal left %s in TMPDIR", filepath.Join(tmp, e.Name()))
	}
	assertNoPlaintextUnder(t, repo, "captured-s3cr3t")
	got, err := os.ReadFile(filepath.Clean(filepath.Join(repo, "secrets.sops.yaml")))
	if err != nil || string(got) != signalFixture {
		t.Errorf("the target changed under a refusal: %q, %v", got, err)
	}
}
