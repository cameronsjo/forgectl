//go:build unix

package sops

// The work directory's .gitignore (cameronsjo/forgectl#698).
//
// An uncatchable kill leaves the work directory, plaintext included, inside
// the repository. These tests use a REAL git repository, because the claim is
// about what git does, not about a file existing.
//
//   [x] `git add -A` stages the target beside a planted leftover and none of
//       the leftover (the target staging is the control proving add ran)
//   [x] Naming the work directory, or a file in it, to `git add` stages
//       nothing either
//   [x] The leftover scan still finds the directory and refuses, naming it,
//       and removes nothing
//   [x] stage fails, writing nothing through it, on anything already at the
//       backup, value or nonce name (not git: the #736 review's O_EXCL nit)

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
)

// gitRepo builds a real git repository holding the signal fixture as its
// target, and a PATH with the sops stub SetValue's LookPath needs.
func gitRepo(t *testing.T) (repo, path string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	repo, err := os.MkdirTemp("", "fcgit")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repo) })
	if out, err := runGit(t, repo, "init", "-q", "."); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
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

// runGit runs git in repo with the user's own config shut out, so a global
// excludes file cannot be what ignores the leftover.
func runGit(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestWorkDirLeftoverIsNeverStagedButStillRefused(t *testing.T) {
	repo, path := gitRepo(t)
	t.Setenv("PATH", path)
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()

	// What an uncatchable kill mid-edit leaves: the staged value, the backup,
	// the nonce, and sops' decrypted copy of the whole document in a
	// subdirectory of its own.
	work, err := newWorkDir(target)
	if err != nil {
		t.Fatalf("newWorkDir: %v", err)
	}
	if err := work.stage([]byte(signalFixture), "s3cr3t-value"); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := os.Mkdir(filepath.Join(work.dir, "123456"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work.dir, "123456", "secrets.sops.yaml"), []byte("a: s3cr3t-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// -A, then the directory and a file in it named explicitly. git exits
	// non-zero on an ignored path it is handed by name, which is the point, so
	// only the index decides.
	if out, err := runGit(t, repo, "add", "-A"); err != nil {
		t.Fatalf("git add -A: %v\n%s", err, out)
	}
	_, _ = runGit(t, repo, "add", "--", filepath.Base(work.dir))
	_, _ = runGit(t, repo, "add", "--", filepath.Join(filepath.Base(work.dir), "value"))
	staged, err := runGit(t, repo, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v\n%s", err, staged)
	}
	if !strings.Contains(staged, "secrets.sops.yaml") {
		t.Fatalf("git add -A staged %q; the control file is missing, so the probe proves nothing", staged)
	}
	for _, line := range strings.Split(strings.TrimSpace(staged), "\n") {
		if strings.HasPrefix(line, ".forgectl-sops-") {
			t.Errorf("git staged %s from the work directory", line)
		}
	}

	_, err = NewClient(refusingRunner{t}).SetValue(t.Context(), target, "a", "another-value")
	if err == nil {
		t.Fatal("SetValue succeeded over a gitignored work directory")
	}
	if !strings.Contains(err.Error(), filepath.Base(work.dir)) {
		t.Errorf("refusal %q does not name the leftover %s", err, filepath.Base(work.dir))
	}
	if _, err := os.Stat(filepath.Join(work.dir, "value")); err != nil {
		t.Errorf("the refusal removed the leftover: %v", err)
	}
}

// stage creates the backup, the value and the nonce exclusively (the #736
// review). Something already at one of those names inside the work directory
// was put there by someone else: a planted symlink must not carry the value
// or the ciphertext to wherever it points, and stage must fail rather than
// write through it.
func TestStageRefusesAnythingAlreadyAtItsNames(t *testing.T) {
	for _, name := range []string{"backup", "value", "nonce"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			w := &workDir{dir: dir, nonce: "NONCE", backup: filepath.Join(dir, "backup")}
			if err := w.stage([]byte(signalFixture), "s3cr3t-value"); err == nil {
				t.Errorf("stage succeeded with a symlink planted at %s", name)
			}
			got, err := os.ReadFile(filepath.Clean(outside))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("stage wrote %d bytes through the symlink planted at %s", len(got), name)
			}
		})
	}
}
