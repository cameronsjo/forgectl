package env

// Test plan for locate.go
//
// Locate (Classification: filesystem safety rail, real temp-dir fixtures)
//   [x] Happy: a file inside the repo resolves, exists=true (incl. a
//       non-.env name like .env.prod — Locate has no opinion on filename)
//   [x] Happy: a not-yet-existing file inside the repo resolves,
//       exists=false, when its parent directory is inside the repo
//   [x] Happy: a worktree's .git (a FILE, not a directory) is recognized
//   [x] Refused: no .git found walking up from cwd at all
//   [x] Refused: a plain ../ escape outside the repo root
//   [x] Refused: a symlinked FILE whose target resolves outside the repo
//   [x] Refused: a symlinked intermediate DIRECTORY whose target resolves
//       outside the repo (the not-yet-existing-file path)

import (
	"os"
	"path/filepath"
	"testing"
)

// initGitRepo makes dir a real (enough) git repo for findRepoRoot's walk-up
// — it only ever needs a .git directory to exist, no real git binary
// required. Mirrors internal/clean/clean_test.go's identical helper.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
}

// resolvedPath resolves symlinks in path — t.TempDir() on macOS returns a
// path under /var/folders/... that is itself a symlink to
// /private/var/folders/..., and Locate resolves everything through
// EvalSymlinks, so an expected-path comparison must do the same. Mirrors
// internal/clean/clean_test.go's identical helper.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func TestLocate_InRepo_OK(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	envPath := filepath.Join(root, ".env.prod")
	if err := os.WriteFile(envPath, []byte("KEY=1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	real, exists, err := Locate(".env.prod", root)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if !exists {
		t.Error("exists = false, want true")
	}
	if want := filepath.Join(resolvedPath(t, root), ".env.prod"); real != want {
		t.Errorf("real = %q, want %q", real, want)
	}
}

func TestLocate_NewFileInRepo_Allowed(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)

	real, exists, err := Locate(".env.new", root)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if exists {
		t.Error("exists = true, want false")
	}
	if want := filepath.Join(resolvedPath(t, root), ".env.new"); real != want {
		t.Errorf("real = %q, want %q", real, want)
	}
}

func TestLocate_WorktreeGitFile_OK(t *testing.T) {
	root := t.TempDir()
	// A worktree's .git is a plain file (a "gitdir: …" pointer), not a
	// directory.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git: %v", err)
	}

	_, exists, err := Locate(".env", root)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if exists {
		t.Error("exists = true, want false (file wasn't created)")
	}
}

func TestLocate_NotARepo_Refused(t *testing.T) {
	root := t.TempDir() // no .git anywhere up from here

	_, _, err := Locate(".env", root)
	if err == nil {
		t.Fatal("Locate outside any git repo returned nil error, want a refusal")
	}
}

func TestLocate_OutsideRepo_Refused(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	initGitRepo(t, repo)
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.env"), []byte("KEY=1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := Locate("../outside/secret.env", repo)
	if err == nil {
		t.Fatal("Locate with a ../ escape returned nil error, want a refusal")
	}
}

func TestLocate_SymlinkedFileEscape_Refused(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.env")
	if err := os.WriteFile(victim, []byte("KEY=1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	link := filepath.Join(repo, ".env")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, _, err := Locate(".env", repo)
	if err == nil {
		t.Fatal("Locate through a symlinked file escaping the repo returned nil error, want a refusal")
	}
}

func TestLocate_SymlinkedDirEscape_Refused(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	outside := t.TempDir()

	link := filepath.Join(repo, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// escape/.env doesn't exist yet — the not-yet-existing-file path, whose
	// PARENT (escape, a symlink to outside) must still be re-checked for
	// containment rather than trusted just because it's "inside" repo
	// lexically.
	_, _, err := Locate(filepath.Join("escape", ".env"), repo)
	if err == nil {
		t.Fatal("Locate through a symlinked directory escaping the repo returned nil error, want a refusal")
	}
}
