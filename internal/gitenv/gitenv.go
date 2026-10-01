// Package gitenv is the one way forgectl runs git (cameronsjo/forgectl#944).
//
// git runs inside repositories whose .git/config forgectl does not control and
// cannot drop: repository extensions such as objectFormat and partialClone
// live there. Some of that config can make git run a program or reach the
// network on a call that means neither. So every git invocation goes through
// a Profile, and TestProductionGitGoesThroughGitenv fails when production
// code runs git any other way.
//
// # Local
//
// Local, the zero Profile, is for every call that needs no fetch or push:
// rev-parse, remote get-url, status, for-each-ref, and the local writes
// (config, branch -D, worktree add from a complete clone). It pins off each
// way such a call can be made to run a program or reach the network, rather
// than trusting the repository not to ask. It is the stash check's hardening
// from #938, extracted unchanged:
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
// operator marked safe into a failed call. Hooks are not switched off: the
// Local calls that can run one (branch -D's reference-transaction, worktree
// add's post-checkout) run it in the operator's own repository.
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
	"os"
	"os/exec"
	"strings"
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

// removed reports whether p removes key from the inherited environment.
func removed(p Profile, key string) bool {
	for _, k := range repositoryVars {
		if key == k {
			return true
		}
	}
	if p == Transport {
		return false
	}
	for _, k := range localOnlyVars {
		if key == k {
			return true
		}
	}
	for _, pin := range localPins {
		if key == pin[0] {
			return true
		}
	}
	return strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_")
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

// Command builds an *exec.Cmd running git with args under p, for a caller
// that needs the command itself (its own timeout and wait delay) rather than
// a Runner. The caller sets Dir, WaitDelay and the output streams.
func Command(ctx context.Context, p Profile, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, Bin, append(Args(p), args...)...) //nolint:gosec // G204: git, with the profile's fixed options ahead of the caller's arguments
	cmd.Env = Env(p, os.Environ())
	return cmd
}
