//go:build unix

package gitenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// includeRepo makes a repository with global and system config cut off and
// returns its directory.
func includeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(Bin); err != nil {
		t.Skip("git is not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	repoGit(t, dir, "init", "-q", "-b", "main")
	return dir
}

func repoGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := Command(t.Context(), Transport, append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every file an include names is read whatever its condition, relative paths
// from the including file's directory and "~/" from HOME, and the includes
// those files name are followed in turn; an include naming no file is
// skipped.
// Mutations: read only includes whose condition holds (onbranch:evil is
// dropped); resolve a relative path from dir instead of the including file
// (evil.inc and nested.inc are not found).
func TestIncludedHookNamesReadsEveryInclude(t *testing.T) {
	dir := includeRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(dir, ".git", "evil.inc"), "[hook \"on.branch\"]\n\tcommand = x\n\tevent = post-checkout\n[include]\n\tpath = sub/nested.inc\n")
	if err := os.Mkdir(filepath.Join(dir, ".git", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(dir, ".git", "sub", "nested.inc"), "[hook \"nested\"]\n\tevent = reference-transaction\n")
	gitdirInc := filepath.Join(t.TempDir(), "gitdir.inc")
	writeConfig(t, gitdirInc, "[hook \"by-gitdir\"]\n\tcommand = y\n")
	writeConfig(t, filepath.Join(home, "home.inc"), "[hook \"home\"]\n\tcommand = z\n")
	repoGit(t, dir, "config", "includeIf.onbranch:evil.path", "evil.inc")
	repoGit(t, dir, "config", "includeIf.gitdir:**/worktrees/w/.path", gitdirInc)
	repoGit(t, dir, "config", "--add", "include.path", "~/home.inc")
	repoGit(t, dir, "config", "--add", "include.path", "missing.inc")

	got, err := includedHookNames(t.Context(), fexec.OSRunner{}, Bin, dir)
	if err != nil {
		t.Fatalf("includedHookNames: %v", err)
	}
	if want := []string{"by-gitdir", "home", "nested", "on.branch"}; !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}

// An include it cannot read the way git would fails the listing closed:
// a FIFO (promptly, never opened), a directory, "~user/" and "%(prefix)/"
// paths, and a hook name -c cannot switch off.
func TestIncludedHookNamesFailsClosed(t *testing.T) {
	cases := map[string]struct {
		path  func(t *testing.T, dir string) string
		wants error
	}{
		"fifo": {func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "fifo.inc")
			if err := unix.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}, errIncludeNotRegular},
		"directory":           {func(t *testing.T, _ string) string { return t.TempDir() }, errIncludeNotRegular},
		"another user's home": {func(*testing.T, string) string { return "~root/x.inc" }, errIncludeUnresolvable},
		"runtime prefix":      {func(*testing.T, string) string { return "%(prefix)/etc/x.inc" }, errIncludeUnresolvable},
		"unoverridable hook name": {func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "bad.inc")
			writeConfig(t, p, "[hook \"a=b\"]\n\tcommand = x\n")
			return p
		}, errUnoverridableHook},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := includeRepo(t)
			repoGit(t, dir, "config", "includeIf.onbranch:nope.path", c.path(t, dir))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := includedHookNames(ctx, fexec.OSRunner{}, Bin, dir); !errors.Is(err, c.wants) {
				t.Fatalf("includedHookNames = %v, want %v", err, c.wants)
			}
			if ctx.Err() != nil {
				t.Fatal("includedHookNames blocked")
			}
		})
	}
}

// resolveInclude builds paths by string as git does, never cleaning them,
// and refuses the shapes it does not emulate.
// Mutations: build with filepath.Join (lnk/./x.inc and a//b.inc come back
// cleaned); drop the ".." refusal; drop the config.worktree refusal.
func TestResolveInclude(t *testing.T) {
	t.Setenv("HOME", "/home/op")
	for _, c := range []struct{ from, path, want string }{
		{"/r/.git/config", "x.inc", "/r/.git/x.inc"},
		{"/r/.git/config", "lnk/./x.inc", "/r/.git/lnk/./x.inc"},
		{"/r/.git/config", "a//b.inc", "/r/.git/a//b.inc"},
		{".git/config", "x.inc", ".git/x.inc"},
		{"config", "x.inc", "x.inc"},
		{"/r/.git/config", "/abs/./x.inc", "/abs/./x.inc"},
		{"", "/abs/x.inc", "/abs/x.inc"},
		{"/r/.git/config", "~/x.inc", "/home/op/x.inc"},
		{"/r/.git/config.worktree", "/abs/x.inc", "/abs/x.inc"},
	} {
		if got, err := resolveInclude(c.from, c.path); err != nil || got != c.want {
			t.Errorf("resolveInclude(%q, %q) = %q, %v; want %q", c.from, c.path, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{
		{"", "rel.inc"},
		{"/r/c", ""},
		{"/r/c", "~op/x"},
		{"/r/c", "%(prefix)/x"},
		{"/r/.git/config", "../x.inc"},
		{"/r/.git/config", "lnk/../x.inc"},
		{"/r/.git/config", "/abs/../x.inc"},
		{"/r/.git/config", "~/../x.inc"},
		{"/r/.git/lnk/../config", "x.inc"},
		{"/r/.git/config.worktree", "x.inc"},
		{"config.worktree", "x.inc"},
	} {
		if _, err := resolveInclude(bad[0], bad[1]); !errors.Is(err, errIncludeUnresolvable) {
			t.Errorf("resolveInclude(%q, %q) = %v, want errIncludeUnresolvable", bad[0], bad[1], err)
		}
	}
}
