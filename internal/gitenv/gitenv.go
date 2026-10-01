// Package gitenv is the one way forgectl runs git (cameronsjo/forgectl#944).
//
// git runs inside repositories whose .git/config forgectl does not control and
// cannot drop: repository extensions such as objectFormat and partialClone
// live there. Some of that config can make git run a program or reach the
// network on a call that means neither. So every git invocation goes through
// a Profile, and TestProductionGitGoesThroughGitenv fails when production
// code runs git any other way. The one exception is git that gh runs for
// `gh repo clone`, which the pin allowlists by name (ghGitAllowlist); gh's
// environment loses what Transport removes (Unset).
//
// # Local
//
// Local, the zero Profile, is for every call that needs no fetch or push:
// rev-parse, remote get-url, status, for-each-ref, and the local writes
// (config, branch -D, worktree add from a complete clone). It pins off the
// ways below that such a call can be made to run a program or reach the
// network, rather than trusting the repository not to ask. It is the stash
// check's hardening from #938, extracted unchanged. It does not pin off every
// way: see "What Local leaves running".
//
//   - lazy fetch. In a partial clone (a promisor remote), reading a missing
//     object fetches it, which runs the transport, and with it
//     core.sshCommand, a remote's uploadpack, or an `ext::` URL's command.
//     Measured on git 2.43: `stash list` and `ls-tree` both ran a canary.
//
//     The load-bearing control is GIT_ALLOW_PROTOCOL set and empty (git 2.6
//     and later), an allowlist that names no transport. Not "none": that is
//     an allowlist naming a transport called none, and a git-remote-none on
//     PATH ran through it (measured on git 2.43). When it is set, git
//     ignores every protocol.allow and protocol.<name>.allow setting, the
//     repository's included, and refuses every transport before it starts.
//     Any inherited value is removed first, so it cannot widen it.
//     `-c protocol.allow=never` is NOT enough on its own: git lets a
//     repository's protocol.<name>.allow take precedence over it, so a
//     repository with protocol.ext.allow=always and an ext:: remote ran its
//     canary through it. GIT_NO_LAZY_FETCH=1 (git 2.44 and later, and some
//     backports) stops the fetch before a transport is chosen; an older git
//     ignores it. Both stay as defence in depth. With the fetch refused, the
//     read fails.
//
//   - core.fsmonitor, which names a hook git launches to query the working
//     tree.
//
//   - log.showSignature, which runs gpg.program on a signed commit a log
//     walk reaches.
//
//   - replace refs, which could substitute another object for the one a call
//     reads (--no-replace-objects).
//
// The environment is scrubbed of the variables git itself clears before it
// works in another repository (`git rev-parse --local-env-vars`): GIT_DIR,
// GIT_WORK_TREE, GIT_INDEX_FILE, GIT_COMMON_DIR, the object-directory
// variables, and the config injected by a parent git's -c, numbered
// GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n pairs included. Inside a git hook, or
// under a caller that exported GIT_DIR, they would point every call at a
// repository other than the one the caller named. GIT_CEILING_DIRECTORIES and
// GIT_DISCOVERY_ACROSS_FILESYSTEM stay: they only narrow discovery, and they
// are the operator's to set. GIT_TERMINAL_PROMPT=0 keeps a call that was
// never meant to authenticate from asking for a password.
//
// User-level config, global and system, is kept: it is the operator's own,
// and dropping it would drop safe.directory, which turns a repository the
// operator marked safe into a failed call.
//
// # What Local leaves running
//
//   - Filter drivers. A file that .gitattributes (or .git/info/attributes)
//     routes through filter.<name>.clean or .process runs that program
//     whenever git re-hashes the file, which status does for any stat-dirty
//     entry, and so does worktree remove's dirty check. No option turns
//     filters off, and the repository's own config can define them. A call
//     that needs no filter uses RunUnfiltered, which lists the drivers the
//     repository and each of its submodules define and blanks each one by
//     name (#977); the projects status probe, clean's dirty check and
//     branch prune's worktree remove do. A driver that only the operator's
//     global or system config defines stays live where git can tell scopes
//     apart (git 2.26 and later).
//
//   - The operator's own filters on checkout. worktree add runs smudge
//     filters, git-lfs's among them, which may fetch objects themselves.
//     Measured on git 2.43 with git-lfs 3.4.1: a worktree add under Local
//     from a bare clone without the LFS objects fetched them from a file://
//     remote, through git-lfs's own transfer, and checked the real content
//     out. Not measured: an https remote whose credentials only an
//     interactive prompt supplies, which GIT_TERMINAL_PROMPT=0 would refuse.
//
//   - Hooks. The Local calls that can run one (branch -D's
//     reference-transaction, worktree add's post-checkout) run it in the
//     operator's own repository.
//
// # Transport
//
// Transport is for a call that reaches a remote (clone, fetch, pull, push,
// remote show) or may need a lazy fetch in the operator's own partial clone
// (worktree add from the operator's repository). It keeps the operator's
// transports, credential helpers, prompts and injected config, since those
// are how the operator's own fetch authenticates, and adds only what cannot
// change that: core.fsmonitor=false, and the repository-locating variables
// removed, so an exported GIT_DIR cannot redirect a push or a fetch into
// another repository. Every Transport use in production is allowlisted with
// its reason in the pin.
package gitenv

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// Profile selects how git is hardened for one call. The zero value is Local.
type Profile int

const (
	// Local is for a call that needs no fetch or push. See the package doc.
	Local Profile = iota
	// Transport is for a call that reaches a remote. See the package doc.
	Transport
)

// Runner is the part of internal/exec's Runner the helper needs.
// *exec.FakeRunner and exec.OSRunner both satisfy it.
type Runner interface {
	RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error)
}

// Bin is the git executable Run names: resolved through PATH, as every other
// child forgectl runs (internal/exec's trust model).
const Bin = "git"

// localArgs precede the caller's arguments on every Local call.
var localArgs = []string{
	"-c", "protocol.allow=never",
	"-c", "core.fsmonitor=false",
	"-c", "log.showSignature=false",
	"--no-replace-objects",
}

// transportArgs precede the caller's arguments on every Transport call.
var transportArgs = []string{
	"-c", "core.fsmonitor=false",
}

// repositoryVars are the variables in `git rev-parse --local-env-vars` (git
// 2.43) that point git at a repository, an index or an object store.
// Transport removes these and no others.
var repositoryVars = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_OBJECT_DIRECTORY",
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_GRAFT_FILE",
	"GIT_INDEX_FILE",
	"GIT_PREFIX",
	"GIT_SHALLOW_FILE",
	"GIT_COMMON_DIR",
}

// localOnlyVars are the rest of `git rev-parse --local-env-vars` (git 2.43):
// injected config and replace-ref selection. Local removes them too, and
// Local's own --no-replace-objects stands in for the replace-ref pair.
var localOnlyVars = []string{
	"GIT_CONFIG",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_REPLACE_REF_BASE",
}

// localPins are set on every Local call after the scrub. GIT_ALLOW_PROTOCOL
// is last: in an environment slice, exec keeps the last value of a duplicate
// key, and the scrub has already removed any inherited one.
var localPins = [][2]string{
	{"GIT_NO_LAZY_FETCH", "1"},
	{"GIT_TERMINAL_PROMPT", "0"},
	{"GIT_ALLOW_PROTOCOL", ""},
}

// Args returns the global options that precede a call's own arguments under
// p. The slice is fresh on every call.
func Args(p Profile) []string {
	if p == Transport {
		return append([]string(nil), transportArgs...)
	}
	return append([]string(nil), localArgs...)
}

// caseInsensitiveEnv is whether environment variable names compare without
// regard to case, as on Windows, where git_dir and GIT_DIR are one variable.
// It is a variable only so a test on another platform can model Windows.
var caseInsensitiveEnv = runtime.GOOS == "windows"

// sameVar reports whether the environment variable names a and b name one
// variable on this platform.
func sameVar(a, b string) bool {
	if caseInsensitiveEnv {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// hasVarPrefix reports whether the environment variable name key begins with
// prefix, compared as sameVar compares names.
func hasVarPrefix(key, prefix string) bool {
	return len(key) >= len(prefix) && sameVar(key[:len(prefix)], prefix)
}

// removed reports whether p removes key from the inherited environment.
func removed(p Profile, key string) bool {
	for _, k := range repositoryVars {
		if sameVar(key, k) {
			return true
		}
	}
	if p == Transport {
		return false
	}
	for _, k := range localOnlyVars {
		if sameVar(key, k) {
			return true
		}
	}
	for _, pin := range localPins {
		if sameVar(key, pin[0]) {
			return true
		}
	}
	return hasVarPrefix(key, "GIT_CONFIG_KEY_") || hasVarPrefix(key, "GIT_CONFIG_VALUE_")
}

// Unset returns the variables in environ that p removes, as a Runner's
// removals. A caller that starts a program which itself runs git, rather
// than git directly, passes Unset(Transport, os.Environ()) so an exported
// GIT_WORK_TREE or GIT_INDEX_FILE cannot redirect that program's git.
func Unset(p Profile, environ []string) []string {
	_, unset := filter(p, environ)
	return unset
}

// Env returns environ as p runs git with it: the scrubbed variables removed,
// then, for Local, the pins appended in order.
func Env(p Profile, environ []string) []string {
	out := make([]string, 0, len(environ)+len(localPins))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if removed(p, key) {
			continue
		}
		out = append(out, kv)
	}
	if p == Transport {
		return out
	}
	for _, pin := range localPins {
		out = append(out, pin[0]+"="+pin[1])
	}
	return out
}

// Run runs git with args through r under p and returns its trimmed stdout, as
// r's Run would.
func Run(ctx context.Context, r Runner, p Profile, args ...string) (string, error) {
	return RunBin(ctx, r, Bin, p, args...)
}

// RunRefusing is Run with each transport in refuse refused whatever the
// operator's configuration or environment allows: the call carries
// `-c protocol.<name>.allow=never` for each, and under Transport an
// inherited GIT_ALLOW_PROTOCOL loses them from its list. git honours that
// variable ahead of every protocol.allow setting, so without the second step
// an operator's GIT_ALLOW_PROTOCOL naming ext would admit an ext:: URL past
// the first (measured on git 2.43). The rest of the operator's list stays,
// so the result never allows a transport it did not. Local needs neither
// step: its GIT_ALLOW_PROTOCOL is pinned empty, which refuses them all.
func RunRefusing(ctx context.Context, r Runner, p Profile, refuse []string, args ...string) (string, error) {
	overrides, unset := filter(p, os.Environ())
	if v, ok := os.LookupEnv(allowProtocolVar); ok && p == Transport {
		if overrides == nil {
			overrides = map[string]string{}
		}
		overrides[allowProtocolVar] = withoutProtocols(v, refuse)
	}
	pre := Args(p)
	for _, name := range refuse {
		pre = append(pre, "-c", "protocol."+name+".allow=never")
	}
	return r.RunWithEnvFiltered(ctx, overrides, unset, Bin, append(pre, args...)...)
}

// allowProtocolVar is git's transport allowlist variable.
const allowProtocolVar = "GIT_ALLOW_PROTOCOL"

// withoutProtocols is the colon-separated GIT_ALLOW_PROTOCOL list allowed
// without the names in refuse. git matches a name exactly.
func withoutProtocols(allowed string, refuse []string) string {
	var kept []string
	for _, name := range strings.Split(allowed, ":") {
		if !slices.Contains(refuse, name) {
			kept = append(kept, name)
		}
	}
	return strings.Join(kept, ":")
}

// RunBin is Run with the git executable named: internal/projects runs the
// absolute path it resolved once at construction.
func RunBin(ctx context.Context, r Runner, bin string, p Profile, args ...string) (string, error) {
	overrides, unset := filter(p, os.Environ())
	return r.RunWithEnvFiltered(ctx, overrides, unset, bin, append(Args(p), args...)...)
}

// filter expresses Env as a Runner's overrides and removals against environ.
func filter(p Profile, environ []string) (map[string]string, []string) {
	var unset []string
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if removed(p, key) {
			unset = append(unset, key)
		}
	}
	if p == Transport {
		return nil, unset
	}
	overrides := make(map[string]string, len(localPins))
	for _, pin := range localPins {
		overrides[pin[0]] = pin[1]
	}
	return overrides, unset
}

// filterDriverKeys matches every configuration key that names a filter
// driver's program (gitattributes(5)): filter.<driver>.clean, .smudge and
// .process. The driver name is a config subsection, so it may itself hold
// dots; it is parsed from the right.
const filterDriverKeys = `^filter\..*\.(clean|smudge|process)$`

// filterDriverVars are the variables filterDriverKeys matches, in the order
// RunUnfiltered blanks them.
var filterDriverVars = []string{"clean", "smudge", "process"}

// maxSubmoduleDepth is how deeply nested a submodule RunUnfiltered follows
// before it refuses the call. git sets no bound of its own; a deeper tree, or
// a submodule path that loops back through a symlink, fails closed rather
// than walking without end.
const maxSubmoduleDepth = 32

// RunUnfiltered runs git with args in dir under Local, as RunBin would, with
// every filter driver that repository configuration defines switched off
// (cameronsjo/forgectl#977). It is for a call that can run a filter on the
// working tree and needs none: status, whose racy or stat-dirty entries are
// re-hashed through the clean filter (or the process filter) that
// .gitattributes names. Local cannot stop that by itself: no option turns
// filters off, and the repository's own .gitattributes and .git/config
// select and define them.
//
// So it lists the drivers first, under Local, with `git config
// --get-regexp`, and the call then carries
// `-c filter.<name>.clean= -c filter.<name>.smudge= -c filter.<name>.process=`
// for each name found, which outranks every configuration file. A driver
// with filter.<name>.required set then fails the call instead of running,
// which the callers read as an unknown state, never a clean one.
//
// The listing covers dir's repository and every populated submodule under
// it, nested ones included, found through `git ls-files --stage`: status runs
// a child git in each submodule, which reads that submodule's own config,
// and the -c options reach those children, because git passes its command
// line config on to a submodule's git (measured on git 2.43). A repository
// with no submodule costs two git processes ahead of the call; each
// populated submodule costs two more.
//
// Only drivers that local, worktree or command scope defines are blanked
// (git config --show-scope, git 2.26 and later). A driver that global or
// system configuration alone defines is the operator's own, git-lfs's among
// them, and stays live, as the rest of the operator's configuration does
// under Local; a repository can still route its files through it, which runs
// the operator's program, not the repository's. A name the repository also
// defines is blanked in every scope. On a git without --show-scope, or a
// scoped listing it cannot parse, every listed driver is blanked.
//
// It fails closed: when a listing fails, names a driver that -c cannot
// override (a name holding '=', where git splits a -c argument, or a
// newline), or lists anything else it does not expect, the call is not run
// and the error says why. dir "" lists and runs in the current directory.
func RunUnfiltered(ctx context.Context, r Runner, bin, dir string, args ...string) (string, error) {
	return RunUnfilteredAlso(ctx, r, bin, dir, nil, args...)
}

// RunUnfilteredAlso is RunUnfiltered for a call that reaches working trees
// besides dir's. worktree remove is one: its dirty check runs status in the
// working tree it removes, under that worktree's own config.worktree, which
// a listing in dir does not read (measured on git 2.43). The drivers of each
// repository in also, and of its submodules, are blanked as well. A path in
// also that does not exist is skipped: git runs no filter in a working tree
// that is not there, and worktree remove still prunes its record.
func RunUnfilteredAlso(ctx context.Context, r Runner, bin, dir string, also []string, args ...string) (string, error) {
	type repo struct {
		dir   string
		depth int
	}
	queue := []repo{{dir: dir}}
	for _, d := range also {
		if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		queue = append(queue, repo{dir: d})
	}
	var names []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		found, err := listFilterDrivers(ctx, r, bin, cur.dir)
		if err != nil {
			return "", err
		}
		for _, name := range found {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		subs, err := populatedSubmodules(ctx, r, bin, cur.dir)
		if err != nil {
			return "", err
		}
		if len(subs) > 0 && cur.depth >= maxSubmoduleDepth {
			return "", errSubmoduleDepth
		}
		for _, sub := range subs {
			queue = append(queue, repo{dir: sub, depth: cur.depth + 1})
		}
	}
	slices.Sort(names)
	at := atDir(dir)
	off := make([]string, 0, 2*len(filterDriverVars)*len(names)+len(at)+len(args))
	for _, name := range names {
		for _, v := range filterDriverVars {
			off = append(off, "-c", "filter."+name+"."+v+"=")
		}
	}
	return RunBin(ctx, r, bin, Local, append(append(off, at...), args...)...)
}

// atDir is the -C option that points git at dir, or none for "".
func atDir(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{"-C", dir}
}

// noMatch reports whether err is git config's answer to a listing that
// matched nothing: exit 1, printing nothing.
func noMatch(err error) bool {
	var ce *fexec.CommandError
	return errors.As(err, &ce) && ce.ExitCode == 1 && ce.Output == ""
}

// listFilterDrivers returns the drivers in dir's repository configuration
// that RunUnfiltered blanks: those a local, worktree or command scope
// defines, or, when git cannot say which scope a key came from, every one.
func listFilterDrivers(ctx context.Context, r Runner, bin, dir string) ([]string, error) {
	out, err := RunBin(ctx, r, bin, Local, append(atDir(dir), "config", "-z", "--show-scope", "--name-only", "--get-regexp", filterDriverKeys)...)
	switch {
	case err == nil:
	case noMatch(err):
		return nil, nil
	default:
		// A git before 2.26 refuses --show-scope (exit 129). Any other
		// failure, the unscoped listing meets as well, and fails closed.
		return listAllFilterDrivers(ctx, r, bin, dir)
	}
	names, err := scopedFilterDriverNames(out)
	if errors.Is(err, errScopeUnparsed) {
		return listAllFilterDrivers(ctx, r, bin, dir)
	}
	return names, err
}

// listAllFilterDrivers returns every driver dir's configuration defines, in
// any scope.
func listAllFilterDrivers(ctx context.Context, r Runner, bin, dir string) ([]string, error) {
	out, err := RunBin(ctx, r, bin, Local, append(atDir(dir), "config", "-z", "--name-only", "--get-regexp", filterDriverKeys)...)
	if err != nil {
		if !noMatch(err) {
			return nil, fmt.Errorf("list the filter drivers git could run: %w", err)
		}
		out = ""
	}
	return filterDriverNames(out)
}

// populatedSubmodules returns the working tree of every submodule in dir's
// index that status would enter: a gitlink entry whose path holds a .git,
// as a directory or a gitfile.
func populatedSubmodules(ctx context.Context, r Runner, bin, dir string) ([]string, error) {
	// ":/" covers the whole tree when dir is below its top, as status does.
	out, err := RunBin(ctx, r, bin, Local, append(atDir(dir), "ls-files", "-z", "--stage", "--", ":/")...)
	if err != nil {
		return nil, fmt.Errorf("list the submodules git status would enter: %w", err)
	}
	var subs []string
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, p, ok := strings.Cut(entry, "\t")
		if !ok || p == "" {
			return nil, errUnexpectedIndexEntry
		}
		if !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		sub := filepath.Join(dir, filepath.FromSlash(p))
		populated, err := holdsGit(sub)
		if err != nil {
			return nil, err
		}
		if populated {
			subs = append(subs, sub)
		}
	}
	return subs, nil
}

// holdsGit reports whether the directory sub holds a .git entry, which is
// what git checks before it runs status in a submodule. A sub that is
// missing or not a directory holds none. Any other error is returned.
func holdsGit(sub string) (bool, error) {
	fi, err := os.Stat(sub)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect a submodule's working tree: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(sub, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect a submodule's working tree: %w", err)
	}
	return true, nil
}

// errUnexpectedFilterKey is returned for listing output that is not a
// NUL-separated list of filter driver keys.
var errUnexpectedFilterKey = errors.New("git config listed a key that names no filter driver; refusing to run unfiltered")

// errUnoverridableFilter is returned for a driver name -c cannot override.
var errUnoverridableFilter = errors.New("a filter driver's name holds '=' or a newline, which git -c cannot override; refusing to run with it live")

// errScopeUnparsed is returned for a scoped listing that is not pairs of
// scope and key; RunUnfiltered then blanks every driver instead.
var errScopeUnparsed = errors.New("git config's scoped listing is not scope and key pairs")

// errUnexpectedIndexEntry is returned for ls-files output that is not
// NUL-separated "mode object stage<TAB>path" entries.
var errUnexpectedIndexEntry = errors.New("git ls-files listed an entry it should not; refusing to run unfiltered")

// errSubmoduleDepth is returned for submodules nested past
// maxSubmoduleDepth.
var errSubmoduleDepth = errors.New("submodules nest too deeply to list their filter drivers; refusing to run unfiltered")

// operatorScopes are the scopes whose drivers RunUnfiltered leaves live: the
// operator's own configuration, which no repository writes.
var operatorScopes = []string{"global", "system"}

// filterDriverNames parses `git config -z --name-only --get-regexp
// filterDriverKeys` output into the sorted, distinct driver names.
func filterDriverNames(out string) ([]string, error) {
	var names []string
	for _, key := range strings.Split(out, "\x00") {
		if key == "" {
			continue
		}
		name, err := filterDriverName(key)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// scopedFilterDriverNames parses `git config -z --show-scope --name-only
// --get-regexp filterDriverKeys` output, NUL-separated scope and key pairs,
// into the sorted, distinct names of the drivers a scope outside
// operatorScopes defines. Every key is checked, whatever its scope.
func scopedFilterDriverNames(out string) ([]string, error) {
	if out == "" {
		return nil, nil
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields)%2 != 0 {
		return nil, errScopeUnparsed
	}
	var names []string
	for i := 0; i < len(fields); i += 2 {
		scope, key := fields[i], fields[i+1]
		name, err := filterDriverName(key)
		if err != nil {
			return nil, err
		}
		if slices.Contains(operatorScopes, scope) || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// filterDriverName is the driver name in a filter driver key, parsed from
// the right, since the name may itself hold dots.
func filterDriverName(key string) (string, error) {
	rest, ok := strings.CutPrefix(key, "filter.")
	dot := strings.LastIndexByte(rest, '.')
	if !ok || dot < 0 || !slices.Contains(filterDriverVars, rest[dot+1:]) {
		return "", errUnexpectedFilterKey
	}
	name := rest[:dot]
	if strings.ContainsAny(name, "=\n") {
		return "", errUnoverridableFilter
	}
	return name, nil
}

// Command builds an *exec.Cmd running git with args under p, for a caller
// that needs the command itself (its own timeout and wait delay) rather than
// a Runner. The caller sets Dir, WaitDelay and the output streams.
func Command(ctx context.Context, p Profile, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, Bin, append(Args(p), args...)...) //nolint:gosec // G204: git, with the profile's fixed options ahead of the caller's arguments
	cmd.Env = Env(p, os.Environ())
	return cmd
}
