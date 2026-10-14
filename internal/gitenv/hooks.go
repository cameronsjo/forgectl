package gitenv

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// A git call that must run no hook needs two mechanisms (#1079, #1049).
//
//   - Hooks found as files: core.hooksPath=/dev/null. git looks a hook up
//     by name in the hooks directory (find_hook), and /dev/null holds none.
//     A command-line value outranks every configuration file.
//   - Hooks defined in configuration (git 2.54 and later):
//     hook.<name>.command, run for each event hook.<name>.event names. git
//     lists these apart from the hooks directory, so core.hooksPath does not
//     reach them. Each name any configuration scope defines is switched off
//     with hook.<name>.enabled=false, which git reads last from the command
//     line. That per-name switch is the control this package relies on.
//
// Every documented event is also passed hook.<event>.enabled=false. Newer
// git documents it as an event-wide switch; git 2.54 reads it only as the
// per-name switch of a hook named after the event, so it stops nothing
// there, and nothing here depends on it. It is harmless where unknown.
//
// RunNoHooks also reads every file an include or a conditional include
// names (includeIf onbranch:, gitdir: and the rest), whatever its
// condition: a call such as worktree add runs children under another
// GIT_DIR and branch, for which a condition can hold that did not hold for
// the listing.
//
// A git older than 2.54 runs no configured hook, and ignores every hook.*
// option.

// hookKeys matches every configuration key that defines a configured hook:
// hook.<name>.command and hook.<name>.event. The name is a config
// subsection, so it may hold dots; it is parsed from the right.
const hookKeys = `^hook\..*\.(command|event)$`

// listedKeys is the one listing RunUnfiltered runs per repository: the
// filter drivers it blanks and the configured hooks it switches off.
const listedKeys = `^(filter\..*\.(clean|smudge|process)|hook\..*\.(command|event))$`

// hookVars are the hook.<name> variables hookKeys matches.
var hookVars = []string{"command", "event"}

// hookEvents are the hook events githooks(5) documents (git 2.55), each
// switched off by name. A friendly name that collides with one is refused
// by git itself.
var hookEvents = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit",
	"pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push", "pre-receive",
	"update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "receive-report", "push-to-checkout",
	"pre-auto-gc", "post-rewrite", "sendemail-validate", "fsmonitor-watchman",
	"p4-changelist", "p4-prepare-changelist", "p4-post-changelist",
	"p4-pre-submit", "post-index-change",
}

// errUnoverridableHook is returned for a configured hook whose name -c
// cannot switch off.
var errUnoverridableHook = errors.New("a configured hook's name holds '=' or a newline, which git -c cannot switch off; refusing to run with it live")

// NoHookArgs returns the options that switch off every hook: the hooks
// directory, every event, and each configured hook in names. RunUnfiltered
// and RunNoHooks place them right after the profile's own options.
func NoHookArgs(names []string) []string {
	out := make([]string, 0, 2+2*len(hookEvents)+2*len(names))
	out = append(out, "-c", "core.hooksPath=/dev/null")
	for _, ev := range hookEvents {
		out = append(out, "-c", "hook."+ev+".enabled=false")
	}
	for _, name := range names {
		out = append(out, "-c", "hook."+name+".enabled=false")
	}
	return out
}

// hookNames parses hook.<name>.command and hook.<name>.event keys, as
// `git config -z --name-only --get-regexp` prints them, into the sorted,
// distinct names. A key it does not expect, or a name -c cannot override,
// fails the parse.
func hookNames(keys []string) ([]string, error) {
	var names []string
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, "hook.")
		dot := strings.LastIndexByte(rest, '.')
		if !ok || dot < 0 || !slices.Contains(hookVars, rest[dot+1:]) {
			return nil, errUnexpectedFilterKey
		}
		name := rest[:dot]
		if strings.ContainsAny(name, "=\n") {
			return nil, errUnoverridableHook
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// listedNames splits a listedKeys listing into the filter driver names and
// the configured hook names it holds.
func listedNames(out string) (filters, hooks []string, err error) {
	var filterKeys, hookKeyList []string
	for _, key := range strings.Split(out, "\x00") {
		switch {
		case key == "":
		case strings.HasPrefix(key, "hook."):
			hookKeyList = append(hookKeyList, key)
		default:
			filterKeys = append(filterKeys, key)
		}
	}
	if filters, err = filterDriverNames(strings.Join(filterKeys, "\x00")); err != nil {
		return nil, nil, err
	}
	if hooks, err = hookNames(hookKeyList); err != nil {
		return nil, nil, err
	}
	return filters, hooks, nil
}

// listConfig runs `git config -z --name-only --get-regexp keys` in dir under
// Local, reading every scope. No match is an empty listing.
func listConfig(ctx context.Context, r Runner, bin, dir, keys, what string) (string, error) {
	out, err := RunBin(ctx, r, bin, Local, append(atDir(dir), "config", "-z", "--name-only", "--get-regexp", keys)...)
	if err != nil {
		// git config exits 1, printing nothing, when no key matches.
		var ce *fexec.CommandError
		if !errors.As(err, &ce) || ce.ExitCode != 1 || ce.Output != "" {
			return "", fmt.Errorf("list the %s git could run: %w", what, err)
		}
		out = ""
	}
	return out, nil
}

// RunNoHooks runs git with args in dir under Local, as Run would, with every
// hook switched off: the hooks directory, and every configured hook the
// configuration git reads in dir defines, whatever its scope. It is for a
// call that would fire a hook a repository or a fetched branch could
// select, such as `worktree add`, whose checkout fires post-checkout.
//
// worktree add runs its children (the checkout's reset, symbolic-ref) with
// GIT_DIR set to the new worktree's admin directory, on the new branch. A
// conditional include whose condition holds for them (includeIf
// "onbranch:<new branch>", includeIf "gitdir:**/worktrees/<id>") but not for
// the listing in dir would define a hook the listing never saw. So the
// hooks of every file any include names are switched off too, read without
// evaluating its condition (includedHookNames).
//
// It fails closed, running nothing, when a listing fails, names a hook -c
// cannot switch off, or names an include it cannot resolve or read. The
// whole call, listings included, runs under Bounded. dir "" lists and runs
// in the current directory.
func RunNoHooks(ctx context.Context, r Runner, dir string, args ...string) (string, error) {
	dctx, cancel := Bounded(ctx)
	defer cancel()
	out, err := listConfig(dctx, r, Bin, dir, hookKeys, "configured hooks")
	if err != nil {
		return "", err
	}
	var keys []string
	for _, key := range strings.Split(out, "\x00") {
		if key != "" {
			keys = append(keys, key)
		}
	}
	names, err := hookNames(keys)
	if err != nil {
		return "", err
	}
	included, err := includedHookNames(dctx, r, Bin, dir)
	if err != nil {
		return "", err
	}
	for _, name := range included {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return Run(dctx, r, Local, append(append(NoHookArgs(names), atDir(dir)...), args...)...)
}

// includeKeys matches the keys that name an include: include.path and
// includeIf.<condition>.path.
const includeKeys = `^(include\.path|includeif\..*\.path)$`

// includedFileKeys matches what includedHookNames reads from each included
// file: its hooks and its own includes.
const includedFileKeys = `^(hook\..*\.(command|event)|include\.path|includeif\..*\.path)$`

// maxIncludedFiles bounds how many included files includedHookNames reads.
const maxIncludedFiles = 64

var (
	// errIncludeUnresolvable is returned for an include whose path cannot be
	// resolved the way git would.
	errIncludeUnresolvable = errors.New("a configuration include names a path that cannot be resolved; refusing to run with its hooks unread")
	// errIncludeNotRegular is returned for an include that names something
	// other than a regular file.
	errIncludeNotRegular = errors.New("a configuration include names something other than a regular file; refusing to run with its hooks unread")
	// errTooManyIncludes is returned past maxIncludedFiles.
	errTooManyIncludes = errors.New("configuration includes name too many files; refusing to run with their hooks unread")
)

// includedHookNames returns the configured hook names every file an include
// names defines, read whatever the include's condition, and the hooks of
// the includes those files name in turn. It starts from the includes the
// configuration git reads in dir lists, with the file each came from.
func includedHookNames(ctx context.Context, r Runner, bin, dir string) ([]string, error) {
	out, err := runConfigList(ctx, r, bin, append(atDir(dir), "config", "-z", "--show-origin", "--get-regexp", includeKeys)...)
	if err != nil {
		return nil, fmt.Errorf("list the configuration includes: %w", err)
	}
	type include struct{ from, path string }
	var queue []include
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		_, value, ok := strings.Cut(entry, "\n")
		if !ok {
			return nil, errIncludeUnresolvable
		}
		from := ""
		if f, isFile := strings.CutPrefix(origin, "file:"); isFile {
			if !filepath.IsAbs(f) {
				// git prints a path relative to where it ran: dir.
				f = filepath.Join(dir, f)
			}
			from = f
		}
		queue = append(queue, include{from: from, path: value})
	}
	if len(fields)%2 == 0 && out != "" {
		// -z output ends in a NUL, so a whole listing splits into an odd
		// count of fields, the last one empty.
		return nil, errIncludeUnresolvable
	}

	var names []string
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		file, err := resolveInclude(cur.from, cur.path)
		if err != nil {
			return nil, err
		}
		if seen[file] {
			continue
		}
		seen[file] = true
		if len(seen) > maxIncludedFiles {
			return nil, errTooManyIncludes
		}
		fi, err := os.Stat(file) //nolint:gosec // G703: an include may name any path, as git allows; this only checks what is there
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// git skips an include that names no file.
			continue
		case err != nil:
			return nil, fmt.Errorf("%w: %w", errIncludeUnresolvable, err)
		case !fi.Mode().IsRegular():
			return nil, errIncludeNotRegular
		}
		body, err := runConfigList(ctx, r, bin, "config", "--file", file, "--no-includes", "-z", "--get-regexp", includedFileKeys)
		if err != nil {
			return nil, fmt.Errorf("read a configuration include: %w", err)
		}
		var hookKeyList []string
		for _, entry := range strings.Split(body, "\x00") {
			if entry == "" {
				continue
			}
			key, value, _ := strings.Cut(entry, "\n")
			if strings.HasPrefix(key, "hook.") {
				hookKeyList = append(hookKeyList, key)
				continue
			}
			queue = append(queue, include{from: file, path: value})
		}
		found, err := hookNames(hookKeyList)
		if err != nil {
			return nil, err
		}
		for _, name := range found {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return names, nil
}

// resolveInclude resolves an include's path as git does: a leading "~/"
// names the home directory, and a relative path is relative to the
// directory of the file the include is in. A form this cannot resolve the
// same way ("~user/", "%(prefix)/", a relative path from a source that is
// not a file) is refused.
func resolveInclude(from, path string) (string, error) {
	switch {
	case path == "":
		return "", errIncludeUnresolvable
	case path == "~" || strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errIncludeUnresolvable
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	case strings.HasPrefix(path, "~"), strings.HasPrefix(path, "%("):
		return "", errIncludeUnresolvable
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if from == "" {
		return "", errIncludeUnresolvable
	}
	return filepath.Join(filepath.Dir(from), path), nil
}

// runConfigList runs a `git config --get-regexp` listing under Local. git
// exits 1, printing nothing, when no key matches; that is an empty listing.
func runConfigList(ctx context.Context, r Runner, bin string, args ...string) (string, error) {
	out, err := RunBin(ctx, r, bin, Local, args...)
	if err != nil {
		var ce *fexec.CommandError
		if !errors.As(err, &ce) || ce.ExitCode != 1 || ce.Output != "" {
			return "", err
		}
		out = ""
	}
	return out, nil
}
