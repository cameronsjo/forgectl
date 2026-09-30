// stash.go extends the leftover scan into the repository's stashes
// (cameronsjo/forgectl#751).
//
// # Why the scan reads the stashes
//
// Every scratch directory carries a `*` .gitignore (scratch.go), which keeps
// it out of `git add`. It does not keep it out of `git stash --all` (or `-a`),
// which stashes ignored files too: it copies the directory, plaintext
// included, into the stash's untracked-files commit (the stash commit's third
// parent, `stash@{N}^3`) and removes it from the working tree. The listing in
// scanLeftovers then finds nothing, and the next write goes ahead while the
// secret sits in the object store, one `git stash pop` from coming back.
// Measured on git 2.43.
//
// So the scan also lists, in every stash entry's untracked tree, the target's
// own directory, and refuses on the same scoped names it refuses on in the
// working tree. Every entry, not only the newest: a later `git stash` pushes
// the capture down to stash@{1}, and it is no less there.
//
// # When it is skipped, and when it refuses
//
// A target whose directory is not in a git work tree has no stashes, and one
// where git cannot run at all cannot have been stashed by it, so the check is
// skipped: `git rev-parse --show-prefix` failing is the signal for both. Once
// a work tree is confirmed, a stash list or tree that cannot be read is a
// refusal, as a directory that cannot be listed is. It removes nothing, and it
// never drops a stash: the stash may also hold the operator's other work.
//
// # How git is run
//
// The stash check runs git inside a repository, whose .git/config the check
// does not control and cannot drop: repository extensions such as
// objectFormat and partialClone live there. So the invocation pins off each
// way a read of refs and trees can be made to run a program or reach the
// network, rather than trusting the repository not to ask (stashGitArgs,
// stashGitEnv):
//
//   - lazy fetch. In a partial clone (a promisor remote), reading a missing
//     object fetches it, which runs the transport, and with it
//     core.sshCommand, a remote's uploadpack, or an `ext::` URL's command. A
//     refs/stash naming a missing commit, or a stash whose untracked tree is
//     missing, triggers it. Measured on git 2.43: `stash list` and `ls-tree`
//     both ran a canary.
//
//     The load-bearing control is GIT_ALLOW_PROTOCOL set and empty (git 2.6
//     and later), an allowlist that names no transport. Not "none": that is
//     an allowlist naming a transport called none, and a git-remote-none on
//     PATH ran through it (measured on git 2.43). When it is set, git
//     ignores every protocol.allow and protocol.<name>.allow setting, the
//     repository's included, and refuses every transport before it starts.
//     It goes last in the environment, so an inherited value cannot widen it.
//     `-c protocol.allow=never` is NOT enough on its own: git lets a
//     repository's protocol.<name>.allow take precedence over it, so a
//     repository with protocol.ext.allow=always and an ext:: remote ran its
//     canary through it. GIT_NO_LAZY_FETCH=1 (git 2.44 and later, and some
//     backports) stops the fetch before a transport is chosen; an older git
//     ignores it. Both stay as defence in depth. With the fetch refused, the
//     read fails and the scan refuses.
//
//   - core.fsmonitor, which names a hook git launches to query the working
//     tree.
//
//   - log.showSignature, which runs gpg.program on a signed commit that
//     `stash list` walks.
//
//   - replace refs, which could substitute another object for a stash commit
//     or its tree (--no-replace-objects).
//
// None of the three commands runs a hook, reads the index, or opens a pager
// (stdout is a pipe). User-level config, global and system, is kept: it is the
// operator's own, and dropping it would drop safe.directory, which turns a
// repository the operator marked safe into a skipped check.
//
// The environment is scrubbed of the variables git itself clears before it
// works in another repository (`git rev-parse --local-env-vars`): GIT_DIR,
// GIT_WORK_TREE, GIT_INDEX_FILE, GIT_COMMON_DIR, the object-directory
// variables, and the config injected by a parent git's -c. Inside a git hook,
// or under a caller that exported GIT_DIR, they would point every call at a
// repository other than the target's, and the check would read the wrong
// stashes. GIT_CEILING_DIRECTORIES and GIT_DISCOVERY_ACROSS_FILESYSTEM stay:
// they only narrow discovery, and they are the operator's to set.
//
// Pathspecs are literal, so a directory whose name holds a glob character is
// matched as itself.
package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// stashGitTimeout bounds each git call the stash check makes. It is a
// variable only so a test can make a hung git time out quickly.
var stashGitTimeout = 10 * time.Second

// stashGitWaitDelay bounds how long a timed-out git call may keep its output
// pipes open after it is killed: a grandchild that inherited them (a
// transport, a hook) would otherwise hold Output past the deadline.
const stashGitWaitDelay = 2 * time.Second

// stashGitArgs precede every git call the stash check makes. See "How git is
// run" above.
var stashGitArgs = []string{
	"-c", "protocol.allow=never",
	"-c", "core.fsmonitor=false",
	"-c", "log.showSignature=false",
	"--no-replace-objects",
	"--literal-pathspecs",
}

// stashGitScrubbed is the output of `git rev-parse --local-env-vars` on git
// 2.43: what git clears before it works in another repository.
var stashGitScrubbed = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_PREFIX":                       true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_COMMON_DIR":                   true,
}

// stashGitEnv is environ without the scrubbed variables, the numbered
// GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n pairs GIT_CONFIG_COUNT indexes, and any
// inherited GIT_ALLOW_PROTOCOL, followed by stashGitEnvPins.
func stashGitEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if stashGitScrubbed[key] || key == "GIT_ALLOW_PROTOCOL" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, stashGitEnvPins...)
}

// stashGitEnvPins are appended to the environment, in this order.
// GIT_ALLOW_PROTOCOL must stay last: exec keeps the last value of a duplicate
// key, so an inherited GIT_ALLOW_PROTOCOL cannot widen it. It is a variable
// only so a test can drop GIT_NO_LAZY_FETCH and model a git older than 2.44,
// which ignores it.
var stashGitEnvPins = []string{"GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL="}

// stashGit runs git with args in dir and returns its stdout. It is a variable
// only so a test can make one call fail the way a corrupt repository would.
var stashGit = func(dir string, args ...string) ([]byte, error) {
	return stashGitRun(dir, nil, args...)
}

// stashGitStdin is stashGit for the one call that reads its stdin. It is a
// variable only so a test can count or fail the call.
var stashGitStdin = func(dir string, stdin []byte, args ...string) ([]byte, error) {
	return stashGitRun(dir, stdin, args...)
}

// stashGitRun is the one place git is spawned, with every pin applied.
func stashGitRun(dir string, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stashGitTimeout)
	defer cancel()
	full := append(append([]string{}, stashGitArgs...), args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // G204: git with read-only arguments this package builds; the only variable parts are a commit id git printed and a path prefix after "--"
	cmd.Dir = dir
	cmd.Env = stashGitEnv(os.Environ())
	cmd.WaitDelay = stashGitWaitDelay
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.Output()
}

// stashedLeftover is one scoped scratch name found in a stash entry.
type stashedLeftover struct {
	index int    // N in stash@{N}
	name  string // the entry's name in the target's directory
}

// errStashUnreadable is the refusal's cause when a confirmed repository's
// stashes cannot be read. It carries no git output: that can quote paths from
// the repository, and the refusal names the target already.
var errStashUnreadable = errors.New("its repository's stashes could not be read to check them for scratch that `git stash --all` captured")

// stashedLeftovers lists the entries of the target's directory, in every
// stash entry's untracked tree, that match scoped. It returns nothing when the
// directory is not in a git work tree or git cannot run there.
func stashedLeftovers(t Target, scoped func(name string) bool) ([]stashedLeftover, error) {
	dir := filepath.Dir(t.Abs())
	out, err := stashGit(dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, nil
	}
	prefix := strings.TrimSuffix(string(out), "\n")

	out, err = stashGit(dir, "stash", "list", "--format=%H %P")
	if err != nil {
		return nil, errStashUnreadable
	}
	// The third parent of each entry that has one. Without it the entry
	// stashed no untracked or ignored files, so it cannot hold scratch.
	var indexes []int
	var commits []string
	for index, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		indexes = append(indexes, index)
		commits = append(commits, fields[3])
	}
	if len(commits) == 0 {
		return nil, nil
	}

	// One git process reads every entry's listing. A prefix with a newline
	// cannot be named on cat-file's line protocol, so it takes the per-entry
	// ls-tree path.
	var listings [][]string
	if strings.Contains(prefix, "\n") {
		listings, err = stashListingsPerEntry(dir, prefix, commits)
	} else {
		listings, err = stashListingsBatch(dir, prefix, commits)
	}
	if err != nil {
		return nil, errStashUnreadable
	}
	var found []stashedLeftover
	for i, paths := range listings {
		for _, path := range paths {
			name, ok := strings.CutPrefix(path, prefix)
			if !ok || name == "" || strings.Contains(name, "/") {
				continue
			}
			if scoped(name) {
				found = append(found, stashedLeftover{index: indexes[i], name: name})
			}
		}
	}
	return found, nil
}

// stashListingsPerEntry lists the prefix directory of each commit's tree with
// one `git ls-tree` each.
func stashListingsPerEntry(dir, prefix string, commits []string) ([][]string, error) {
	out := make([][]string, 0, len(commits))
	for _, commit := range commits {
		args := []string{"ls-tree", "--full-tree", "--name-only", "-z", "--end-of-options", commit}
		if prefix != "" {
			args = append(args, "--", prefix)
		}
		tree, err := stashGit(dir, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, strings.Split(string(tree), "\x00"))
	}
	return out, nil
}

// stashListingsBatch reads every commit's tree, and each directory on the way
// to prefix, through one `git cat-file --batch`, and returns each commit's
// entry paths in the prefix directory, joined to the prefix as
// `ls-tree --full-tree` prints them. One object name per level, because
// cat-file reports a missing object and an absent path alike as "missing":
// only the parent's entries say which. A root tree or a listed directory
// that cannot be read is an error, as it is for ls-tree; a prefix directory
// the commit lacks is an empty listing. Anything malformed is an error.
func stashListingsBatch(dir, prefix string, commits []string) ([][]string, error) {
	var parts []string
	if prefix != "" {
		parts = strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	}
	var in strings.Builder
	for _, commit := range commits {
		in.WriteString(commit + ":\n")
		for i := range parts {
			in.WriteString(commit + ":" + strings.Join(parts[:i+1], "/") + "\n")
		}
	}
	out, err := stashGitStdin(dir, []byte(in.String()), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	listings := make([][]string, 0, len(commits))
	for _, commit := range commits {
		hashLen := stashHashLen(commit)
		var entries []stashTreeEntry
		alive := true // every level so far was a tree
		for level := 0; level <= len(parts); level++ {
			var kind string
			var content []byte
			kind, content, out, err = stashBatchRecord(out)
			if err != nil {
				return nil, err
			}
			if level == 0 {
				if kind != "tree" {
					return nil, errStashUnreadable
				}
			} else if alive {
				_, isDir := stashEntryNamed(entries, parts[level-1])
				switch {
				case kind == "tree":
				case kind == "missing" && isDir:
					// Listed by its parent, yet not there: unreadable.
					return nil, errStashUnreadable
				default:
					alive = false // absent, or not a directory
				}
			}
			if alive && kind == "tree" {
				if entries, err = parseStashTree(content, hashLen); err != nil {
					return nil, err
				}
			}
		}
		var paths []string
		if alive {
			for _, e := range entries {
				paths = append(paths, prefix+e.name)
			}
		}
		listings = append(listings, paths)
	}
	if len(out) != 0 {
		return nil, errStashUnreadable
	}
	return listings, nil
}

// stashHashLen is the byte width of the hashes in a raw tree: the repository's
// object format, which is the width of the commit id git printed.
func stashHashLen(commitID string) int { return len(commitID) / 2 }

// stashTreeEntry is one entry of a raw tree object.
type stashTreeEntry struct {
	mode string
	name string
}

// stashEntryNamed reports whether entries names name, and whether it is a
// directory (mode 40000).
func stashEntryNamed(entries []stashTreeEntry, name string) (found, isDir bool) {
	for _, e := range entries {
		if e.name == name {
			return true, e.mode == "40000"
		}
	}
	return false, false
}

// stashBatchRecord splits one `cat-file --batch` record off out: its kind
// ("missing", or the object type) and content.
func stashBatchRecord(out []byte) (kind string, content, rest []byte, err error) {
	nl := bytes.IndexByte(out, '\n')
	if nl < 0 {
		return "", nil, nil, errStashUnreadable
	}
	header := strings.Fields(string(out[:nl]))
	out = out[nl+1:]
	if len(header) == 2 && header[1] == "missing" {
		return "missing", nil, out, nil
	}
	if len(header) != 3 {
		return "", nil, nil, errStashUnreadable
	}
	size, err := strconv.Atoi(header[2])
	if err != nil || size < 0 || size+1 > len(out) || out[size] != '\n' {
		return "", nil, nil, errStashUnreadable
	}
	return header[1], out[:size], out[size+1:], nil
}

// parseStashTree decodes a raw tree object: `<mode> <name>\0<hash>` entries.
func parseStashTree(content []byte, hashLen int) ([]stashTreeEntry, error) {
	var entries []stashTreeEntry
	for len(content) > 0 {
		sp := bytes.IndexByte(content, ' ')
		if sp < 0 {
			return nil, errStashUnreadable
		}
		mode := string(content[:sp])
		content = content[sp+1:]
		nul := bytes.IndexByte(content, 0)
		if nul < 0 || len(content) < nul+1+hashLen {
			return nil, errStashUnreadable
		}
		entries = append(entries, stashTreeEntry{mode: mode, name: string(content[:nul])})
		content = content[nul+1+hashLen:]
	}
	return entries, nil
}

// stashedLeftoverLine is the refusal line for one stashed scratch name.
func stashedLeftoverLine(t Target, s stashedLeftover) string {
	ref := fmt.Sprintf("stash@{%d}", s.index)
	path := termsafe.QuotePath(filepath.Join(filepath.Dir(t.Rel()), s.name))
	return fmt.Sprintf(
		"%s in %s: scratch from a forgectl write to %s, captured by `git stash --all`, which stashes ignored files; it may hold a secret in plaintext, and the stash keeps it in the repository's object store. `git stash show --include-untracked %s` lists what the stash holds. Pop it to put the scratch back where this check names it, or drop it if nothing else in it is needed; a dropped stash's objects stay until `git gc` prunes them",
		path, ref, termsafe.QuotePath(t.Rel()), ref)
}
