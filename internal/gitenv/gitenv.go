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
//     branch prune's worktree remove do. The operator's own drivers are
//     blanked too: git-lfs's global filter runs the program a repository's
//     lfs.extension config names.
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
// its reason in the pin, and goes through RunRefusing or RunBinRefusing
// with ext and fd refused (#987): a served URL, or a repository's own config
// that names an ext:: remote and allows it, would otherwise run a command.
package gitenv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

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
	return RunBinRefusing(ctx, r, Bin, p, refuse, args...)
}

// RunBinRefusing is RunRefusing with the git executable named, as RunBin is
// Run's: internal/projects pulls through the absolute path it resolved.
func RunBinRefusing(ctx context.Context, r Runner, bin string, p Profile, refuse []string, args ...string) (string, error) {
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
	return r.RunWithEnvFiltered(ctx, overrides, unset, bin, append(pre, args...)...)
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
// before it refuses the call. git sets no bound of its own.
const maxSubmoduleDepth = 32

// maxUnfilteredRepos is how many repositories, the superproject, each
// submodule and each extra working tree together, RunUnfiltered lists
// before it refuses the call. Each costs two git processes.
const maxUnfilteredRepos = 256

// repoDeadline bounds one RunUnfiltered call, its listings included, and
// each call made under Bounded (#1005). git itself blocks for good on a
// repository whose HEAD, or a loose ref it reads, is a FIFO: plain `git
// status` hangs there (measured on git 2.43), and so do the listings. 30
// seconds is an order of magnitude above a status of a large working tree on
// a cold cache, which takes seconds, so a real repository never meets it,
// while a planted one costs one bounded wait per repository rather than a
// hung command.
//
// branch prune's `worktree remove` runs under it too, the deletion as well
// as its dirty check. Meeting the deadline there is safe: the remove is
// killed part way, prune keeps the branch (it deletes the branch only after
// a remove that succeeded), and a re-run finishes the remove. It is a
// variable only so a test can shorten it.
var repoDeadline = 30 * time.Second

// Bounded returns ctx bounded by repoDeadline, under which each git a
// Runner starts runs in a process group of its own that the deadline kills
// whole. It is for a non-interactive Local call in a repository forgectl
// did not make, where a FIFO HEAD or ref would block git for good: the
// projects inventory's `remote get-url`, for one. The caller defers cancel.
//
// A git in its own group no longer gets the terminal's Ctrl-C, which goes
// to the foreground group only, so Bounded passes it on (interruptible): a
// terminating signal that reaches forgectl while the context is live
// cancels it, which kills the git's group, and cancel then re-raises the
// signal, so forgectl stops as it did when git shared its group rather
// than carrying on to the next repository.
func Bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithTimeout(ctx, repoDeadline)
	ictx, stop := interruptible(dctx)
	return fexec.WithProcessGroup(ictx), func() {
		stop()
		cancel()
	}
}

// RunUnfiltered runs git with args in dir under Local, as RunBin would, with
// every filter driver git's configuration defines switched off
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
// Every driver is blanked, whatever scope defines it. The operator's own,
// git-lfs's among them, are switched off too: a driver defined globally
// still reads the repository's config, and git-lfs runs the program a
// repository's lfs.extension.<name>.clean names through the operator's
// global filter.lfs.process. A stat-dirty LFS file then reads unknown.
//
// The listing covers dir's repository and every populated submodule under
// it, nested ones included, found through `git ls-files --stage`: status runs
// a child git in each submodule, which reads that submodule's own config,
// and the -c options reach those children, because git passes its command
// line config on to a submodule's git (measured on git 2.43). A repository
// with no submodule costs two git processes ahead of the call; each
// populated submodule costs two more.
//
// It fails closed, refusing the call with an error that says why, when:
//   - a listing fails, or lists anything it does not expect;
//   - a driver's name holds '=' (git splits a -c argument there) or a
//     newline, which -c cannot override;
//   - a gitlink's path is a symbolic link, or its .git is a symbolic link
//     or not a repository (a directory with no HEAD, or a gitfile that names
//     none). git itself would not enter such a submodule, but proving that
//     costs more than refusing;
//   - any repository's .git, dir's own included, is neither a directory nor
//     a regular file: a FIFO or a device, whose read could block or never
//     end;
//   - any repository's HEAD is neither a regular file nor a symbolic link:
//     a FIFO there blocks git itself;
//   - two paths reach one repository, submodules nest past
//     maxSubmoduleDepth, or the walk passes maxUnfilteredRepos.
//
// The whole call, listings included, runs under Bounded, and it fails with
// errUnfilteredDeadline when repoDeadline ends it: a FIFO that git
// reads, which the checks above do not see (a loose ref's), blocks it for
// good.
//
// dir "" lists and runs in the current directory.
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
	// Status and worktree remove are non-interactive, so each git may leave
	// the terminal's process group; at the deadline its whole group dies.
	dctx, cancel := Bounded(ctx)
	defer cancel()
	out, err := runUnfilteredAlso(dctx, r, bin, dir, also, args...)
	if err != nil && ctx.Err() == nil && errors.Is(dctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%w within %s", errUnfilteredDeadline, repoDeadline)
	}
	return out, err
}

// runUnfilteredAlso is RunUnfilteredAlso without its deadline.
func runUnfilteredAlso(ctx context.Context, r Runner, bin, dir string, also []string, args ...string) (string, error) {
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
	// The starting repositories are keyed when their .git can be read, so a
	// submodule that leads back to one of them is refused as a revisit. One
	// that cannot be is left to git: dir may be a subdirectory of its
	// working tree, or a worktree whose layout the listing need not model.
	// A .git that is neither a directory nor a regular file is refused, and
	// so is a HEAD that is neither a regular file nor a symbolic link.
	visited := map[string]bool{}
	for _, q := range queue {
		key, err := gitDirKey(q.dir)
		if errors.Is(err, errGitfileNotRegular) || errors.Is(err, errHeadNotRegular) {
			return "", err
		}
		if err == nil && key != "" {
			visited[key] = true
		}
	}
	var names []string
	for listed := 0; len(queue) > 0; listed++ {
		if listed == maxUnfilteredRepos {
			return "", errTooManyRepos
		}
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
			if visited[sub.key] {
				return "", errSubmoduleRevisit
			}
			visited[sub.key] = true
			queue = append(queue, repo{dir: sub.dir, depth: cur.depth + 1})
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

// listFilterDrivers returns every filter driver dir's configuration
// defines, in any scope.
func listFilterDrivers(ctx context.Context, r Runner, bin, dir string) ([]string, error) {
	out, err := RunBin(ctx, r, bin, Local, append(atDir(dir), "config", "-z", "--name-only", "--get-regexp", filterDriverKeys)...)
	if err != nil {
		// git config exits 1, printing nothing, when no key matches.
		var ce *fexec.CommandError
		if !errors.As(err, &ce) || ce.ExitCode != 1 || ce.Output != "" {
			return nil, fmt.Errorf("list the filter drivers git could run: %w", err)
		}
		out = ""
	}
	return filterDriverNames(out)
}

// submodule is a populated submodule's working tree and the key of the
// repository it holds.
type submodule struct {
	dir, key string
}

// populatedSubmodules returns every submodule in dir's index that status
// would enter: a gitlink entry whose path holds a .git.
func populatedSubmodules(ctx context.Context, r Runner, bin, dir string) ([]submodule, error) {
	// ":/" covers the whole tree when dir is below its top, as status does.
	out, err := RunBin(ctx, r, bin, Local, append(atDir(dir), "ls-files", "-z", "--stage", "--", ":/")...)
	if err != nil {
		return nil, fmt.Errorf("list the submodules git status would enter: %w", err)
	}
	var subs []submodule
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
		fi, err := os.Lstat(sub)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("inspect a submodule's working tree: %w", err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, errSubmoduleSymlink
		case !fi.IsDir():
			continue
		}
		key, err := gitDirKey(sub)
		if err != nil {
			return nil, err
		}
		if key != "" {
			subs = append(subs, submodule{dir: sub, key: key})
		}
	}
	return subs, nil
}

// Blocks reports whether git would block for good, or read without end, on
// dir's repository as gitDirKey sees it: a .git that is neither a directory
// nor a regular file, or a HEAD that is neither a regular file nor a
// symbolic link (#1005). A caller about to run a Bounded git there skips it
// instead of waiting out repoDeadline. It reads no more than RunUnfiltered's
// own check does, and is false when dir holds no .git.
func Blocks(dir string) bool {
	_, err := gitDirKey(dir)
	return errors.Is(err, errGitfileNotRegular) || errors.Is(err, errHeadNotRegular)
}

// maxGitfileBytes bounds a gitfile read: "gitdir: " and a path.
const maxGitfileBytes = 64 << 10

// gitDirKey returns the resolved repository directory that the working
// tree sub's .git names: the directory itself, or the one a gitfile points
// at. It returns "" when sub holds no .git. It returns errSubmoduleSymlink
// when .git is a symbolic link, errGitfileNotRegular when .git is neither a
// directory nor a regular file (a FIFO or a device, whose read could block
// or never end; git's own read_gitfile refuses them too),
// errInvalidSubmoduleGit when .git names no repository (no HEAD in it), and
// errHeadNotRegular when that HEAD is neither a regular file nor a symbolic
// link (a FIFO there blocks git itself). It never reads more than
// maxGitfileBytes.
func gitDirKey(sub string) (string, error) {
	dotGit := filepath.Join(sub, ".git")
	fi, err := os.Lstat(dotGit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("inspect a submodule's .git: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return "", errSubmoduleSymlink
	case !fi.IsDir() && !fi.Mode().IsRegular():
		return "", errGitfileNotRegular
	}
	gitDir := dotGit
	if !fi.IsDir() {
		target, err := readGitfile(dotGit)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(sub, target)
		}
		gitDir = target
	}
	resolved, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", errInvalidSubmoduleGit
	}
	// The gitfile names any path it likes; only its HEAD is checked.
	head, err := os.Lstat(filepath.Join(resolved, "HEAD")) //nolint:gosec // G703: a gitfile may point anywhere, as git allows; this only checks for a HEAD there
	if err != nil {
		return "", errInvalidSubmoduleGit
	}
	if !head.Mode().IsRegular() && head.Mode()&fs.ModeSymlink == 0 {
		return "", errHeadNotRegular
	}
	return resolved, nil
}

// readGitfile returns the path the gitfile at path names. It opens the file
// without following a link or blocking (openGitfile), refuses a handle that
// is not a regular file, and reads at most maxGitfileBytes+1 bytes.
func readGitfile(path string) (string, error) {
	f, err := openGitfile(path)
	if err != nil {
		return "", fmt.Errorf("open a submodule's gitfile: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect a submodule's gitfile: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", errGitfileNotRegular
	}
	body, err := io.ReadAll(io.LimitReader(f, maxGitfileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read a submodule's gitfile: %w", err)
	}
	if len(body) > maxGitfileBytes {
		return "", errInvalidSubmoduleGit
	}
	target, ok := strings.CutPrefix(strings.TrimRight(string(body), "\r\n"), "gitdir: ")
	if !ok || target == "" {
		return "", errInvalidSubmoduleGit
	}
	return target, nil
}

// errGitfileNotRegular is returned for a .git that is neither a directory
// nor a regular file.
var errGitfileNotRegular = errors.New("a repository's .git is neither a directory nor a regular file; refusing to run unfiltered")

// errHeadNotRegular is returned for a repository whose HEAD is neither a
// regular file nor a symbolic link.
var errHeadNotRegular = errors.New("a repository's HEAD is neither a regular file nor a symbolic link; refusing to run git there")

// errUnfilteredDeadline is returned when repoDeadline ends a RunUnfiltered
// call.
var errUnfilteredDeadline = errors.New("git did not finish")

// errUnexpectedFilterKey is returned for listing output that is not a
// NUL-separated list of filter driver keys.
var errUnexpectedFilterKey = errors.New("git config listed a key that names no filter driver; refusing to run unfiltered")

// errUnoverridableFilter is returned for a driver name -c cannot override.
var errUnoverridableFilter = errors.New("a filter driver's name holds '=' or a newline, which git -c cannot override; refusing to run with it live")

// errUnexpectedIndexEntry is returned for ls-files output that is not
// NUL-separated "mode object stage<TAB>path" entries.
var errUnexpectedIndexEntry = errors.New("git ls-files listed an entry it should not; refusing to run unfiltered")

// errSubmoduleSymlink is returned for a submodule path, or its .git, that
// is a symbolic link.
var errSubmoduleSymlink = errors.New("a submodule's path or .git is a symbolic link; refusing to run unfiltered")

// errInvalidSubmoduleGit is returned for a submodule .git that names no
// repository.
var errInvalidSubmoduleGit = errors.New("a submodule's .git names no repository; refusing to run unfiltered")

// errSubmoduleRevisit is returned when two submodule paths reach one
// repository.
var errSubmoduleRevisit = errors.New("two submodule paths reach one repository; refusing to run unfiltered")

// errSubmoduleDepth is returned for submodules nested past
// maxSubmoduleDepth.
var errSubmoduleDepth = errors.New("submodules nest too deeply to list their filter drivers; refusing to run unfiltered")

// errTooManyRepos is returned when the walk passes maxUnfilteredRepos.
var errTooManyRepos = errors.New("too many submodules to list their filter drivers; refusing to run unfiltered")

// filterDriverNames parses `git config -z --name-only --get-regexp
// filterDriverKeys` output into the sorted, distinct driver names.
func filterDriverNames(out string) ([]string, error) {
	var names []string
	for _, key := range strings.Split(out, "\x00") {
		if key == "" {
			continue
		}
		rest, ok := strings.CutPrefix(key, "filter.")
		dot := strings.LastIndexByte(rest, '.')
		if !ok || dot < 0 || !slices.Contains(filterDriverVars, rest[dot+1:]) {
			return nil, errUnexpectedFilterKey
		}
		name := rest[:dot]
		if strings.ContainsAny(name, "=\n") {
			return nil, errUnoverridableFilter
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Command builds an *exec.Cmd running git with args under p, for a caller
// that needs the command itself (its own timeout and wait delay) rather than
// a Runner. The caller sets Dir, WaitDelay and the output streams.
func Command(ctx context.Context, p Profile, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, Bin, append(Args(p), args...)...) //nolint:gosec // G204: git, with the profile's fixed options ahead of the caller's arguments
	cmd.Env = Env(p, os.Environ())
	return cmd
}
