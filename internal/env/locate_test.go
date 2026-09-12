package env

// Test plan for locate.go
//
// locate == ResolveTarget + Target.Clear (Classification: filesystem safety
// rail, real temp-dir fixtures)
//   [x] Happy: a file inside the repo resolves, Exists=true (incl. a
//       non-.env name like .env.prod — ResolveTarget has no opinion on
//       filename; Clear is what holds one)
//   [x] Happy: a not-yet-existing file inside the repo resolves,
//       Exists=false, when its parent directory is inside the repo
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
//   [x] Target.Rel is repo-relative, never the absolute resolved path
//   [x] A containment refusal names what the caller typed, not the resolved
//       path (which is by definition outside the repo)

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// locate composes the two production halves internal/cli's resolveEnvTarget
// composes: one ResolveTarget, then Target.Clear. It carries no copy of the
// refusal wording, so a test cannot keep passing while production's message
// drifts.
//
// It deliberately has no allowAnyFile parameter. An earlier version took one
// and bypassed Clear unconditionally, which made every test using it assert
// against a fixture rather than production: the real bypass is gated behind
// isTerminal() AND an operator confirmation, neither of which a helper can
// stand in for. The --any-file path is covered where it actually lives, in
// internal/cli's TestResolveAllowAnyFile_SymlinkBindsToResolvedPath and
// TestEnvSetCmd_ConfirmedPathIsWrittenPath.
func locate(fileFlag, cwd string) (Target, error) {
	target, err := ResolveTarget(fileFlag, cwd)
	if err != nil {
		return Target{}, err
	}
	if err := target.Clear(); err != nil {
		return Target{}, err
	}
	return target, nil
}

// mustTarget resolves and clears fileFlag or fails the test — the fixture
// equivalent of what the CLI hands a Client method. Tests take this route
// rather than building a Target literal so a fixture cannot assert against a
// target production would have refused.
func mustTarget(t *testing.T, fileFlag, cwd string) Target {
	t.Helper()
	target, err := locate(fileFlag, cwd)
	if err != nil {
		t.Fatalf("locate(%q): %v", fileFlag, err)
	}
	return target
}

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
// /private/var/folders/..., and resolution goes through EvalSymlinks, so an
// expected-path comparison must do the same. Mirrors
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

	target, err := locate(".env.prod", root)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if !target.Exists {
		t.Error("Exists = false, want true")
	}
	if want := filepath.Join(resolvedPath(t, root), ".env.prod"); target.path != want {
		t.Errorf("Path = %q, want %q", target.path, want)
	}
	if want := resolvedPath(t, root); target.root != want {
		t.Errorf("Root = %q, want %q", target.root, want)
	}
}

func TestLocate_NewFileInRepo_Allowed(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)

	target, err := locate(".env.new", root)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if target.Exists {
		t.Error("Exists = true, want false")
	}
	if want := filepath.Join(resolvedPath(t, root), ".env.new"); target.path != want {
		t.Errorf("Path = %q, want %q", target.path, want)
	}
}

func TestLocate_WorktreeGitFile_OK(t *testing.T) {
	root := t.TempDir()
	// A worktree's .git is a plain file (a "gitdir: …" pointer), not a
	// directory.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git: %v", err)
	}

	target, err := locate(".env", root)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if target.Exists {
		t.Error("Exists = true, want false (file wasn't created)")
	}
}

func TestLocate_NotARepo_Refused(t *testing.T) {
	root := t.TempDir() // no .git anywhere up from here

	_, err := locate(".env", root)
	if err == nil {
		t.Fatal("locate outside any git repo returned nil error, want a refusal")
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

	_, err := locate("../outside/secret.env", repo)
	if err == nil {
		t.Fatal("locate with a ../ escape returned nil error, want a refusal")
	}
	// The refusal must name what the caller typed. filepath.Base alone would
	// render this as "secret.env", which names nothing the operator can act
	// on and does not distinguish it from an in-repo file of the same name.
	if !strings.Contains(err.Error(), "outside/secret.env") {
		t.Errorf("error = %q, want it to name the argument as typed", err.Error())
	}
	// And it must NOT name the resolved path, which is by definition outside
	// the repository and carries a machine-specific prefix (forgectl#481).
	if strings.Contains(err.Error(), resolvedPath(t, outside)) {
		t.Errorf("error = %q, want it to omit the resolved absolute path", err.Error())
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

	_, err := locate(".env", repo)
	if err == nil {
		t.Fatal("locate through a symlinked file escaping the repo returned nil error, want a refusal")
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

	_, err := locate(".env", root)
	if err == nil {
		t.Fatal("locate against a directory target returned nil error, want a refusal")
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

	// A FIFO with no writer would block os.Open/parseFile forever — resolution
	// must refuse it before any caller ever opens it.
	_, err := locate(".env", root)
	if err == nil {
		t.Fatal("locate against a FIFO target returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want it to name the regular-file rule", err.Error())
	}
}

func TestTargetRel_IsRepoRelative(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	sub := filepath.Join(root, "svc", "api")
	// Ordinary source-tree modes, deliberately: a loose-permission .env is
	// exactly the input this rail has to accept and then tighten.
	if err := os.MkdirAll(sub, 0o755); err != nil { //nolint:gosec // G301: an ordinary source subdirectory
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".env"), []byte("KEY=1\n"), 0o644); err != nil { //nolint:gosec // G306: see above
		t.Fatalf("WriteFile: %v", err)
	}

	target, err := locate(filepath.Join("svc", "api", ".env"), root)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if want := filepath.Join("svc", "api", ".env"); target.Rel() != want {
		t.Errorf("Rel() = %q, want %q", target.Rel(), want)
	}
	if filepath.IsAbs(target.Rel()) {
		t.Errorf("Rel() = %q, want a relative path", target.Rel())
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
		// Byte-exact, deliberately. APFS is case-insensitive, so ".ENV"
		// names the same file as ".env" — and this refusing means the
		// allowlist errs toward refusing a legitimate file, never toward
		// admitting a non-env one. Failing in that direction is the point.
		{".ENV", false},
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

			target, err := locate(name, root)
			if err != nil {
				t.Fatalf("locate(%q): %v", name, err)
			}
			if !target.Exists {
				t.Errorf("Exists = false, want true for %q", name)
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

	_, err := locate(".git/config", repo)
	if err == nil {
		t.Fatal("locate against .git/config returned nil error, want a refusal (not an env file)")
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

// TestResolveTarget_NonEnvFile_ResolvesButDoesNotClear pins the split between
// the two halves: ResolveTarget has no opinion on the filename and resolves a
// non-env target successfully, and Clear is the only thing that refuses it.
// That separation is what lets the CLI learn WHICH path it is about to bypass
// the allowlist for before asking a human about it.
//
// This replaces a test that drove the old locate helper with allowAnyFile=true.
// That helper's bypass was unconditional while production's is gated on a tty
// and a confirmation, so the test asserted the fixture's behaviour rather than
// the command's. The bypass itself is covered in internal/cli, against the real
// command tree.
func TestResolveTarget_NonEnvFile_ResolvesButDoesNotClear(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	repoConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(repoConfig, []byte("[core]\n"), 0o644); err != nil { //nolint:gosec // G306: matches the mode git itself writes
		t.Fatalf("WriteFile: %v", err)
	}

	target, err := ResolveTarget(filepath.Join(".git", "config"), repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if !target.Exists {
		t.Error("Exists = false, want true")
	}
	if want := filepath.Join(resolvedPath(t, repo), ".git", "config"); target.path != want {
		t.Errorf("Path = %q, want %q", target.path, want)
	}
	if err := target.Clear(); err == nil {
		t.Error("Clear() returned nil, want the not-an-env-file refusal")
	}
}

// TestTargetClear_UnresolvedTarget_Refused proves the unexported path fields
// do their job: the only Target another package can construct is one with no
// path, and every entry point refuses it rather than acting on an empty string.
func TestTargetClear_UnresolvedTarget_Refused(t *testing.T) {
	if err := (Target{Exists: true}).Clear(); err == nil {
		t.Error("Clear() on an unresolved Target returned nil, want a refusal")
	}
	if _, err := OpenTarget(Target{Exists: true}); err == nil {
		t.Error("OpenTarget on an unresolved Target returned nil, want a refusal")
	}
	client := NewClient(nil)
	if _, err := client.SetValue(Target{Exists: true}, "KEY", "value"); err == nil {
		t.Error("SetValue on an unresolved Target returned nil, want a refusal")
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
	_, err := locate(filepath.Join("escape", ".env"), repo)
	if err == nil {
		t.Fatal("locate through a symlinked directory escaping the repo returned nil error, want a refusal")
	}
}
