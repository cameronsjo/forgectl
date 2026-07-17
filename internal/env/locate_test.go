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
//   [x] Refused: an existing target that resolves to a directory, not a
//       regular file
//   [x] Refused: an existing target that resolves to a FIFO, not a regular
//       file

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

	real, exists, err := Locate(".env.prod", root, false)
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

	real, exists, err := Locate(".env.new", root, false)
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

	_, exists, err := Locate(".env", root, false)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if exists {
		t.Error("exists = true, want false (file wasn't created)")
	}
}

func TestLocate_NotARepo_Refused(t *testing.T) {
	root := t.TempDir() // no .git anywhere up from here

	_, _, err := Locate(".env", root, false)
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

	_, _, err := Locate("../outside/secret.env", repo, false)
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

	_, _, err := Locate(".env", repo, false)
	if err == nil {
		t.Fatal("Locate through a symlinked file escaping the repo returned nil error, want a refusal")
	}
}

func TestLocate_ExistingDirectory_Refused(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	// A directory named ".env" — EvalSymlinks resolves it fine (it's not a
	// symlink issue), but it is not a regular file: os.Open/parseFile on a
	// directory errors oddly rather than reading it as .env content.
	if err := os.Mkdir(filepath.Join(root, ".env"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	_, _, err := Locate(".env", root, false)
	if err == nil {
		t.Fatal("Locate against a directory target returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want it to name the regular-file rule", err.Error())
	}
}

func TestLocate_ExistingFIFO_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs are a unix concept; forgectl only ships linux/darwin builds")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	fifoPath := filepath.Join(root, ".env")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo unsupported in this environment: %v", err)
	}

	// A FIFO with no writer would block os.Open/parseFile forever — Locate
	// must refuse it before any caller ever opens it.
	_, _, err := Locate(".env", root, false)
	if err == nil {
		t.Fatal("Locate against a FIFO target returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want it to name the regular-file rule", err.Error())
	}
}

func TestIsEnvFileName_Allowlist(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{".env", true},
		{".env.local", true},
		{".env.prod", true},
		{".env.staging", true},
		{".env.example", true},
		{"prod.env", true},
		{"config", false},
		{".gitconfig", false},
		{"Makefile", false},
		// .envrc starts with ".env" but not ".env." (no separating dot
		// before "rc"), and doesn't end with ".env" either — it's a real
		// RCE sink (direnv executes it) and must NOT be on the allowlist.
		{".envrc", false},
	}
	for _, c := range cases {
		if got := IsEnvFileName(c.base); got != c.want {
			t.Errorf("IsEnvFileName(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

func TestLocate_EnvShapedNames_Accepted(t *testing.T) {
	names := []string{".env", ".env.local", ".env.prod", "prod.env"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			initGitRepo(t, root)
			if err := os.WriteFile(filepath.Join(root, name), []byte("KEY=1\n"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			_, exists, err := Locate(name, root, false)
			if err != nil {
				t.Fatalf("Locate(%q): %v", name, err)
			}
			if !exists {
				t.Errorf("exists = false, want true for %q", name)
			}
		})
	}
}

func TestLocate_NonEnvFile_Refused(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	const original = "[core]\n\trepositoryformatversion = 0\n"
	gitConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitConfig, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := Locate(".git/config", repo, false)
	if err == nil {
		t.Fatal("Locate against .git/config returned nil error, want a refusal (not an env file)")
	}
	if !strings.Contains(err.Error(), "not an env file") {
		t.Errorf("error = %q, want it to name the not-an-env-file rule", err.Error())
	}

	got, rerr := os.ReadFile(gitConfig)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(got) != original {
		t.Errorf(".git/config content changed: %q, want unchanged %q", got, original)
	}
}

func TestLocate_NonEnvFile_AllowAnyFileSkipsCheck(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	gitConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitConfig, []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	real, exists, err := Locate(".git/config", repo, true)
	if err != nil {
		t.Fatalf("Locate with allowAnyFile=true: %v", err)
	}
	if !exists {
		t.Error("exists = false, want true")
	}
	if want := filepath.Join(resolvedPath(t, repo), ".git", "config"); real != want {
		t.Errorf("real = %q, want %q", real, want)
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
	_, _, err := Locate(filepath.Join("escape", ".env"), repo, false)
	if err == nil {
		t.Fatal("Locate through a symlinked directory escaping the repo returned nil error, want a refusal")
	}
}
