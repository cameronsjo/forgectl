package env

// writeAtomic's scratch directory (cameronsjo/forgectl#737).
//
// A plain `env set` killed mid-write used to leave `.env-<hash>.<rand>.tmp`,
// the whole new document, beside the target, where `git add -A` commits it.
// These tests use a REAL git repository, because the claim is about what git
// does, not about a file existing.
//
//   [x] A run that dies with the document written and synced (simulated by a
//       panic from the scratchWritten seam: writeAtomic defers no cleanup, so
//       it strands the directory exactly as SIGKILL would) leaves it inside a
//       scoped `.forgectl-env-` directory holding a `*` .gitignore
//   [x] `git add -A` stages the control file and nothing from that directory;
//       naming the directory or the file in it stages nothing either; no
//       staged content carries the value; `git status` does not list it
//   [x] The next set refuses, naming the directory, and removes nothing
//   [x] A successful write leaves no scratch directory behind

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// envGitRepo builds a real git repository holding one committed-to-be control
// file, so a `git add -A` that staged nothing at all cannot pass for one that
// ignored the leftover.
func envGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	repo := t.TempDir()
	if out, err := runEnvGit(t, repo, "init", "-q", "."); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "control.txt"), []byte("control\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return repo
}

// runEnvGit runs git in repo with the user's own config shut out, so a global
// excludes file cannot be what ignores the leftover.
func runEnvGit(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// killedMidWrite runs a set on .env in dir that dies once the whole new
// document is on disk inside the scratch directory, and returns that
// directory's name.
func killedMidWrite(t *testing.T, dir string) string {
	t.Helper()
	type killed struct{}
	var seen string
	prev := scratchWritten
	scratchWritten = func(name string) {
		seen = name
		panic(killed{})
	}
	t.Cleanup(func() { scratchWritten = prev })

	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(killed); !ok {
					panic(r)
				}
			}
		}()
		_ = setOn(t, dir, ".env")
		t.Fatal("the set was never interrupted: scratchWritten did not run")
	}()
	scratchWritten = prev
	if seen == "" {
		t.Fatal("writeAtomic never reported its scratch directory")
	}
	return seen
}

func TestKilledWriteLeftoverIsNeverStagedButStillRefused(t *testing.T) {
	warn := captureWarnings(t)
	repo := envGitRepo(t)
	scratch := killedMidWrite(t, repo)

	tg := pinnedTarget(t, repo, ".env")
	if !strings.HasPrefix(scratch, tg.envScratchDirPrefix()) {
		t.Errorf("scratch directory %q is not scoped to .env (%s…)", scratch, tg.envScratchDirPrefix())
	}
	entries, err := os.ReadDir(filepath.Join(repo, scratch))
	if err != nil {
		t.Fatalf("the killed run's scratch directory is not there: %v", err)
	}
	var doc string
	sawIgnore := false
	for _, e := range entries {
		switch {
		case e.Name() == ScratchIgnoreName:
			sawIgnore = true
		case strings.HasPrefix(e.Name(), scratchTempPrefix):
			doc = e.Name()
		}
	}
	if !sawIgnore || doc == "" || len(entries) != 2 {
		t.Fatalf("the scratch directory holds %v, want its .gitignore and the temp file", entries)
	}
	// The premise: the stranded file really is the whole new document.
	assertHoldsValue(t, filepath.Join(repo, scratch, doc))

	// -A, then the directory and the file in it named explicitly. git exits
	// non-zero on an ignored path it is handed by name, which is the point, so
	// only the index decides.
	if out, err := runEnvGit(t, repo, "add", "-A"); err != nil {
		t.Fatalf("git add -A: %v\n%s", err, out)
	}
	_, _ = runEnvGit(t, repo, "add", "--", scratch)
	_, _ = runEnvGit(t, repo, "add", "--", filepath.Join(scratch, doc))
	staged, err := runEnvGit(t, repo, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v\n%s", err, staged)
	}
	if !strings.Contains(staged, "control.txt") {
		t.Fatalf("git add -A staged %q; the control file is missing, so the probe proves nothing", staged)
	}
	for _, line := range strings.Split(strings.TrimSpace(staged), "\n") {
		if strings.HasPrefix(line, envScratchPrefix) {
			t.Errorf("git staged %s from the scratch directory", line)
		}
	}
	if patch, err := runEnvGit(t, repo, "diff", "--cached"); err != nil || strings.Contains(patch, leftoverSecret) {
		t.Errorf("the staged content carries the value (err %v)", err)
	}
	if status, err := runEnvGit(t, repo, "status", "--porcelain", "--untracked-files=all"); err != nil || strings.Contains(status, envScratchPrefix) {
		t.Errorf("git status lists the scratch directory: %q, %v", status, err)
	}

	err = setOn(t, repo, ".env")
	if err == nil {
		t.Fatal("a set over the killed run's scratch directory succeeded")
	}
	if !strings.Contains(err.Error(), scratch) {
		t.Errorf("refusal %q does not name the leftover %s", err, scratch)
	}
	assertHoldsValue(t, filepath.Join(repo, scratch, doc))
	assertNoSecretInOutput(t, leftoverSecret, "", err.Error(), warn.String())
}

// assertHoldsValue requires path to still exist and carry the value set.
func assertHoldsValue(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("the leftover %s was removed or became unreadable: %v", path, err)
	}
	if !strings.Contains(string(got), leftoverSecret) {
		t.Fatalf("the leftover %s does not hold the value, so it is not the document this test is about", path)
	}
}

func TestSuccessfulWriteLeavesNoScratchDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := setOn(t, dir, ".env"); err != nil {
		t.Fatalf("set: %v", err)
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range names {
		if strings.HasPrefix(e.Name(), envScratchPrefix) || strings.HasPrefix(e.Name(), tempPrefix) {
			t.Errorf("a successful set left %s behind", e.Name())
		}
	}
	// And the next write is not refused on anything the first one left.
	if err := setOn(t, dir, ".env"); err != nil {
		t.Errorf("a second set was refused: %v", err)
	}
}
