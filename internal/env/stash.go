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
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
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
	cmd := gitenv.Command(ctx, gitenv.Local, full...)
	cmd.Dir = dir
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
// to prefix, in two git processes, and returns each commit's entry paths in
// the prefix directory, joined to the prefix as `ls-tree --full-tree` prints
// them. The first, `cat-file --batch-check`, names each object's type and
// size only, so a level that is a file in some stash is never read into
// memory; the second, `cat-file --batch`, reads the trees. One object name per
// level, because cat-file reports a missing object and an absent path alike
// as "missing": only the parent's entries say which. A root tree or a listed
// directory that cannot be read is an error, as it is for ls-tree; a prefix
// directory the commit lacks is an empty listing. Anything malformed is an
// error.
func stashListingsBatch(dir, prefix string, commits []string) ([][]string, error) {
	var parts []string
	if prefix != "" {
		parts = strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	}
	var specs []string
	for _, commit := range commits {
		specs = append(specs, commit+":")
		for i := range parts {
			specs = append(specs, commit+":"+strings.Join(parts[:i+1], "/"))
		}
	}
	in := []byte(strings.Join(specs, "\n") + "\n")
	checked, err := stashGitStdin(dir, in, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	kinds, err := stashBatchCheck(checked, specs)
	if err != nil {
		return nil, err
	}
	var treeSpecs []string
	for i, kind := range kinds {
		if kind == "tree" {
			treeSpecs = append(treeSpecs, specs[i])
		}
	}
	var contents [][]byte
	if len(treeSpecs) > 0 {
		out, err := stashGitStdin(dir, []byte(strings.Join(treeSpecs, "\n")+"\n"), "cat-file", "--batch")
		if err != nil {
			return nil, err
		}
		if contents, err = stashBatchTrees(out, len(treeSpecs)); err != nil {
			return nil, err
		}
	}
	listings := make([][]string, 0, len(commits))
	next := 0 // the next spec, and the next tree content
	treeNo := 0
	for _, commit := range commits {
		hashLen := stashHashLen(commit)
		var entries []stashTreeEntry
		alive := true // every level so far was a tree
		for level := 0; level <= len(parts); level++ {
			kind := kinds[next]
			next++
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
			if kind == "tree" {
				content := contents[treeNo]
				treeNo++
				if alive {
					if entries, err = parseStashTree(content, hashLen); err != nil {
						return nil, err
					}
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

// stashBatchCheck reads `cat-file --batch-check` output for specs, one line
// each, and returns each object's type, or "missing". It matches the missing
// line as exactly `<spec> missing`: cat-file echoes the name it was given,
// which may hold spaces. A found line is `<oid> <type> <size>` and never
// echoes the input.
func stashBatchCheck(out []byte, specs []string) ([]string, error) {
	kinds := make([]string, 0, len(specs))
	for _, spec := range specs {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			return nil, errStashUnreadable
		}
		line := string(out[:nl])
		out = out[nl+1:]
		if line == spec+" missing" {
			kinds = append(kinds, "missing")
			continue
		}
		header := strings.Split(line, " ")
		if len(header) != 3 || header[1] == "missing" {
			return nil, errStashUnreadable
		}
		if _, err := strconv.Atoi(header[2]); err != nil {
			return nil, errStashUnreadable
		}
		kinds = append(kinds, header[1])
	}
	if len(out) != 0 {
		return nil, errStashUnreadable
	}
	return kinds, nil
}

// stashBatchTrees splits `cat-file --batch` output for count objects already
// known to be trees into their contents. A record is `<oid> tree <size>`,
// the content, and a newline; anything else, or output left over, is an error.
func stashBatchTrees(out []byte, count int) ([][]byte, error) {
	contents := make([][]byte, 0, count)
	for range count {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			return nil, errStashUnreadable
		}
		header := strings.Split(string(out[:nl]), " ")
		out = out[nl+1:]
		if len(header) != 3 || header[1] != "tree" {
			return nil, errStashUnreadable
		}
		size, err := strconv.Atoi(header[2])
		if err != nil || size < 0 || size+1 > len(out) || out[size] != '\n' {
			return nil, errStashUnreadable
		}
		contents = append(contents, out[:size])
		out = out[size+1:]
	}
	if len(out) != 0 {
		return nil, errStashUnreadable
	}
	return contents, nil
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
