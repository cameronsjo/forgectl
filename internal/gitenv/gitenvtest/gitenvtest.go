// Package gitenvtest holds fixtures for tests that prove a git call site runs
// under gitenv's Local profile: a partial clone whose lazy fetch would run a
// canary, and a model of a git too old to honour GIT_NO_LAZY_FETCH. Only
// tests import it.
package gitenvtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// RequireGit skips t when git is not on PATH.
func RequireGit(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath(gitenv.Bin)
	if err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	return path
}

// WithoutLazyFetchPin models a git older than 2.44, which ignores
// GIT_NO_LAZY_FETCH: it puts first on PATH a git that removes that variable
// and runs the real git. Every other pin reaches git unchanged, so
// GIT_ALLOW_PROTOCOL is the only control left between a repository and its
// transport. Unix only: the wrapper is a shell script.
func WithoutLazyFetchPin(t testing.TB) {
	t.Helper()
	gitPath := RequireGit(t)
	gitPath, err := filepath.Abs(gitPath)
	if err != nil {
		t.Fatalf("Abs(%s): %v", gitPath, err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nunset GIT_NO_LAZY_FETCH\nexec '" + gitPath + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile git wrapper: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Strip returns args without a leading gitenv profile's options, for a fake
// Runner that answers on git's subcommand. args is returned unchanged when it
// does not begin with one. That a call carries its profile is gitenv's tests'
// and TestProductionGitGoesThroughGitenv's to prove, not every fake's.
func Strip(args []string) []string {
	for _, p := range []gitenv.Profile{gitenv.Local, gitenv.Transport} {
		prefix := gitenv.Args(p)
		if len(args) >= len(prefix) && slices.Equal(args[:len(prefix)], prefix) {
			return args[len(prefix):]
		}
	}
	return args
}

// Git runs git with args in dir for fixture setup, under gitenv's Transport
// profile so an inherited GIT_DIR cannot redirect it. It fails t on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := gitenv.Command(t.Context(), gitenv.Transport, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Canary is a partial clone at Dir whose promisor remote, on any lazy fetch,
// creates the file at Path, through an ext:: URL the repository itself allows
// with protocol.ext.allow=always (which `-c protocol.allow=never` does not
// override). HEAD names a commit the repository lacks, so any call that reads
// HEAD's commit must fetch it.
type Canary struct {
	Dir  string
	Path string
}

// NewCanary builds a Canary repository. It clears the inherited variables
// that would replace or widen the canary's transport; t.Setenv restores them.
func NewCanary(t testing.TB) Canary {
	t.Helper()
	RequireGit(t)
	for _, key := range []string{"GIT_SSH_COMMAND", "GIT_SSH", "GIT_ALLOW_PROTOCOL", "GIT_NO_LAZY_FETCH"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv %s: %v", key, err)
		}
	}
	dir := t.TempDir()
	c := Canary{Dir: filepath.Join(dir, "repo"), Path: filepath.Join(dir, "canary")}
	if err := os.Mkdir(c.Dir, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	Git(t, c.Dir, "init", "-q", "-b", "main")
	Git(t, c.Dir, "commit", "-q", "--allow-empty", "-m", "control")
	for _, kv := range [][2]string{
		{"core.repositoryformatversion", "1"},
		{"extensions.partialClone", "origin"},
		{"remote.origin.promisor", "true"},
		{"protocol.ext.allow", "always"},
		{"remote.origin.url", "ext::sh -c touch% " + c.Path},
	} {
		Git(t, c.Dir, "config", kv[0], kv[1])
	}
	// HEAD names a commit that is not in the object store.
	missing := strings.Repeat("ab", 20)
	if err := os.WriteFile(filepath.Join(c.Dir, ".git", "refs", "heads", "main"), []byte(missing+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile refs/heads/main: %v", err)
	}
	return c
}

// Ran reports whether the canary's transport command ran.
func (c Canary) Ran(t testing.TB) bool {
	t.Helper()
	_, err := os.Lstat(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("Lstat canary: %v", err)
	}
	return true
}

// AssertLive proves the fixture can fire: an unhardened git reading HEAD's
// commit runs the canary. A test that asserts the canary never ran calls this
// first, on a second Canary, so a fixture that cannot fire fails loudly
// rather than passing every call site.
func (c Canary) AssertLive(t testing.TB) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), gitenv.Bin, "-C", c.Dir, "cat-file", "-e", "HEAD^{commit}") //nolint:gosec // G204: git with arguments this fixture built
	_ = cmd.Run()
	if !c.Ran(t) {
		t.Fatal("an unhardened git did not run the canary; the fixture cannot fire, so a passing test would prove nothing")
	}
}
