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
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
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

// FilterListing reports whether args, a git argv as a fake Runner records it,
// are one of gitenv.RunUnfiltered's listings ahead of its call: the filter
// drivers, `[-C dir] config -z [--show-scope] --name-only --get-regexp
// ^filter...`, or the submodules, `[-C dir] ls-files -z --stage -- :/`.
func FilterListing(args []string) bool {
	return driverListing(args) || submoduleListing(args)
}

// atStripped returns args without a leading profile and -C dir.
func atStripped(args []string) []string {
	args = Strip(args)
	if len(args) >= 2 && args[0] == "-C" {
		args = args[2:]
	}
	return args
}

func driverListing(args []string) bool {
	args = atStripped(args)
	return len(args) >= 5 && args[0] == "config" && args[len(args)-2] == "--get-regexp" && strings.HasPrefix(args[len(args)-1], `^filter\.`)
}

func submoduleListing(args []string) bool {
	return slices.Equal(atStripped(args), []string{"ls-files", "-z", "--stage", "--", ":/"})
}

// ErrNoFilterDrivers is what git returns for a filter listing that matches
// nothing: exit status 1 and no output.
func ErrNoFilterDrivers(name string, args []string) error {
	return &fexec.CommandError{Name: name, Args: args, ExitCode: 1, Err: errors.New("exit status 1")}
}

// AnswerListing answers a FilterListing as git does for a repository with no
// filter driver and no submodule: the driver listing exits 1 with no output,
// and the submodule listing prints nothing.
func AnswerListing(name string, args []string) (string, error) {
	if driverListing(args) {
		return "", ErrNoFilterDrivers(name, args)
	}
	return "", nil
}

// NoFilters wraps a fake Runner's RunFunc so RunUnfiltered's listings are
// answered as AnswerListing answers them, and every other call reaches fn.
// gitenv.RunUnfiltered then runs the caller's argv unchanged.
func NoFilters(fn func(name string, args []string) (string, error)) func(name string, args []string) (string, error) {
	return func(name string, args []string) (string, error) {
		if FilterListing(args) {
			return AnswerListing(name, args)
		}
		return fn(name, args)
	}
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

// FilterCanary is a repository whose .gitattributes route two committed
// files through filter drivers its own .git/config defines, each of which
// creates the file at Path when git runs it: "evil", a clean filter, on
// x.txt, and "dot.ted", a process filter whose name holds a dot, on y.txt.
// Both files are stat-dirty, so any status re-hashes them through their
// drivers (#977).
type FilterCanary struct {
	Dir  string
	Path string
}

// NewFilterCanary builds a FilterCanary repository; extra are further
// "name=driver" .gitattributes entries, as path and driver name, each
// defined as a clean filter that fires the same canary. Unix only: the
// drivers are shell commands.
func NewFilterCanary(t testing.TB, extra ...[2]string) FilterCanary {
	t.Helper()
	RequireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("the filter canary's drivers are sh commands")
	}
	dir := t.TempDir()
	c := FilterCanary{Dir: filepath.Join(dir, "repo"), Path: filepath.Join(dir, "canary")}
	if err := os.Mkdir(c.Dir, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	files := [][2]string{{"x.txt", "evil"}, {"y.txt", "dot.ted"}}
	files = append(files, extra...)
	var attrs strings.Builder
	for _, f := range files {
		attrs.WriteString(f[0] + " filter=" + f[1] + "\n")
		if err := os.WriteFile(filepath.Join(c.Dir, f[0]), []byte("payload\n"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", f[0], err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.Dir, ".gitattributes"), []byte(attrs.String()), 0o600); err != nil {
		t.Fatalf("WriteFile .gitattributes: %v", err)
	}
	Git(t, c.Dir, "init", "-q", "-b", "main")
	Git(t, c.Dir, "add", ".")
	Git(t, c.Dir, "commit", "-q", "-m", "control")
	// The drivers are defined only after the commit, so committing ran none.
	fire := "touch '" + c.Path + "'"
	Git(t, c.Dir, "config", "filter.evil.clean", fire+"; cat")
	Git(t, c.Dir, "config", "filter.dot.ted.process", fire+"; exit 1")
	for _, f := range extra {
		Git(t, c.Dir, "config", "filter."+f[1]+".clean", fire+"; cat")
	}
	later := time.Now().Add(time.Hour)
	for _, f := range files {
		if err := os.Chtimes(filepath.Join(c.Dir, f[0]), later, later); err != nil {
			t.Fatalf("Chtimes %s: %v", f[0], err)
		}
	}
	return c
}

// Ran reports whether any of the canary's drivers ran.
func (c FilterCanary) Ran(t testing.TB) bool {
	t.Helper()
	return Canary{Path: c.Path}.Ran(t)
}

// AssertLive proves the fixture can fire: a git status with Local's options
// and environment but no filter overrides runs a driver. A test that asserts
// the canary never ran calls this first, on a second FilterCanary.
func (c FilterCanary) AssertLive(t testing.TB) {
	t.Helper()
	cmd := gitenv.Command(t.Context(), gitenv.Local, "-C", c.Dir, "status", "--porcelain")
	_ = cmd.Run()
	if !c.Ran(t) {
		t.Fatal("a Local git status ran no filter driver; the fixture cannot fire, so a passing test would prove nothing")
	}
}

// NewSubmoduleFilterCanary builds a FilterCanary whose own configuration
// defines no filter driver, holding a populated submodule "s" that holds a
// populated submodule "n". Each submodule's own .git/config defines a clean
// filter that fires the canary on its stat-dirty x.txt: "sub" in s, and
// "nested" in n. Status in the superproject runs a child git in each
// submodule, which runs that submodule's driver (#977). The submodules are
// embedded repositories added as gitlinks, with no .gitmodules: git status
// enters them all the same (measured on git 2.43). Unix only.
func NewSubmoduleFilterCanary(t testing.TB) FilterCanary {
	t.Helper()
	RequireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("the filter canary's drivers are sh commands")
	}
	dir := t.TempDir()
	c := FilterCanary{Dir: filepath.Join(dir, "repo"), Path: filepath.Join(dir, "canary")}
	sub := filepath.Join(c.Dir, "s")
	nested := filepath.Join(sub, "n")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	repos := []struct{ dir, driver string }{{nested, "nested"}, {sub, "sub"}, {c.Dir, ""}}
	for _, r := range repos {
		Git(t, r.dir, "init", "-q", "-b", "main")
		if r.driver != "" {
			if err := os.WriteFile(filepath.Join(r.dir, "x.txt"), []byte("payload\n"), 0o600); err != nil {
				t.Fatalf("WriteFile x.txt: %v", err)
			}
			if err := os.WriteFile(filepath.Join(r.dir, ".gitattributes"), []byte("x.txt filter="+r.driver+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile .gitattributes: %v", err)
			}
		}
		Git(t, r.dir, "add", ".")
		Git(t, r.dir, "commit", "-q", "--allow-empty", "-m", "control")
	}
	fire := "touch '" + c.Path + "'; cat"
	later := time.Now().Add(time.Hour)
	for _, r := range repos[:2] {
		Git(t, r.dir, "config", "filter."+r.driver+".clean", fire)
		if err := os.Chtimes(filepath.Join(r.dir, "x.txt"), later, later); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	return c
}

// NewOperatorFilterCanary builds a FilterCanary and gives the process an
// operator configuration (GIT_CONFIG_GLOBAL, restored by t.Setenv) defining
// two clean filters that create the file at the returned path: "op", which
// only the operator defines, on o.txt; and "both", on b.txt, which the
// repository's own config defines too, firing the repository's canary. Both
// files are stat-dirty. Unix only.
func NewOperatorFilterCanary(t testing.TB) (FilterCanary, string) {
	t.Helper()
	c := NewFilterCanary(t, [2]string{"o.txt", "op"}, [2]string{"b.txt", "both"})
	// NewFilterCanary defined "op" in the repository too; the operator's
	// global config is to be its only definition.
	Git(t, c.Dir, "config", "--unset", "filter.op.clean")
	operator := filepath.Join(filepath.Dir(c.Dir), "operator-canary")
	global := filepath.Join(filepath.Dir(c.Dir), "gitconfig")
	fire := "touch '" + operator + "'; cat"
	// Quoted: an unquoted ';' would start a comment in a config file.
	conf := "[filter \"op\"]\n\tclean = \"" + fire + "\"\n[filter \"both\"]\n\tclean = \"" + fire + "\"\n"
	if err := os.WriteFile(global, []byte(conf), 0o600); err != nil {
		t.Fatalf("WriteFile global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	return c, operator
}
