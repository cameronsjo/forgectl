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
// does not control and cannot drop. It runs every call under gitenv's Local
// profile, which pins off each way a read of refs and trees can be made to
// run a program or reach the network (lazy fetch, core.fsmonitor,
// log.showSignature, replace refs) and scrubs the variables that would point
// git at another repository; internal/gitenv's package doc is the whole
// account, measurements included. With a lazy fetch refused, the read fails
// and the scan refuses. None of the three commands runs a hook, reads the
// index, or opens a pager (stdout is a pipe).
//
// Pathspecs are literal, so a directory whose name holds a glob character is
// matched as itself.
package env

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// stashGitTimeout bounds each git call the stash check makes. It is a
// variable only so a test can make a hung git time out quickly.
var stashGitTimeout = 10 * time.Second

// stashGitWaitDelay bounds how long a timed-out git call may keep its output
// pipes open after it is killed: a grandchild that inherited them (a
// transport, a hook) would otherwise hold Output past the deadline.
const stashGitWaitDelay = 2 * time.Second

// stashGitArgs precede every git call the stash check makes, after
// gitenv's Local options.
var stashGitArgs = []string{"--literal-pathspecs"}

// stashGit runs git with args in dir and returns its stdout. It is a variable
// only so a test can make one call fail the way a corrupt repository would.
var stashGit = func(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stashGitTimeout)
	defer cancel()
	full := append(append([]string{}, stashGitArgs...), args...)
	cmd := gitenv.Command(ctx, gitenv.Local, full...)
	cmd.Dir = dir
	cmd.WaitDelay = stashGitWaitDelay
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
	var found []stashedLeftover
	for index, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.Fields(line)
		// The commit and its three parents. Without a third parent the entry
		// stashed no untracked or ignored files, so it cannot hold scratch.
		if len(fields) < 4 {
			continue
		}
		args := []string{"ls-tree", "--full-tree", "--name-only", "-z", "--end-of-options", fields[3]}
		if prefix != "" {
			args = append(args, "--", prefix)
		}
		tree, err := stashGit(dir, args...)
		if err != nil {
			return nil, errStashUnreadable
		}
		for _, path := range strings.Split(string(tree), "\x00") {
			name, ok := strings.CutPrefix(path, prefix)
			if !ok || name == "" || strings.Contains(name, "/") {
				continue
			}
			if scoped(name) {
				found = append(found, stashedLeftover{index: index, name: name})
			}
		}
	}
	return found, nil
}

// stashedLeftoverLine is the refusal line for one stashed scratch name.
func stashedLeftoverLine(t Target, s stashedLeftover) string {
	ref := fmt.Sprintf("stash@{%d}", s.index)
	path := termsafe.QuotePath(filepath.Join(filepath.Dir(t.Rel()), s.name))
	return fmt.Sprintf(
		"%s in %s: scratch from a forgectl write to %s, captured by `git stash --all`, which stashes ignored files; it may hold a secret in plaintext, and the stash keeps it in the repository's object store. `git stash show --include-untracked %s` lists what the stash holds. Pop it to put the scratch back where this check names it, or drop it if nothing else in it is needed; a dropped stash's objects stay until `git gc` prunes them",
		path, ref, termsafe.QuotePath(t.Rel()), ref)
}
