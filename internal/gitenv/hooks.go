package gitenv

import (
	"context"
	"errors"
	"fmt"
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
//     line; every event is switched off as well with
//     hook.<event>.enabled=false (git 2.55 and later; an older git ignores
//     the key).
//
// A git older than 2.54 runs no configured hook, and ignores both kinds of
// hook.* option.

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
// hook switched off: the hooks directory and every configured hook the
// configuration git reads in dir defines, whatever its scope. It is for a
// call that would fire a hook a repository or a fetched branch could select,
// such as `worktree add`, whose checkout fires post-checkout.
//
// It lists the configured hooks first, and fails closed, running nothing,
// when the listing fails or names a hook -c cannot switch off. dir "" lists
// and runs in the current directory.
func RunNoHooks(ctx context.Context, r Runner, dir string, args ...string) (string, error) {
	out, err := listConfig(ctx, r, Bin, dir, hookKeys, "configured hooks")
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
	return Run(ctx, r, Local, append(append(NoHookArgs(names), atDir(dir)...), args...)...)
}
