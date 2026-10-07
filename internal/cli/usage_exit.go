// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// classifyUsageErrors gives every cobra-raised usage error (a bad flag, a
// wrong argument count, an unknown verb) the usage class, so the same mistake
// exits the same way whichever layer caught it (ADR-0015, forgectl#1085).
//
// It wraps the two places cobra reports a bad call: each command's
// flag-error handler and its Args validator. A command that already returns a
// coded error (the docs verbs, a verb's own contract) keeps that code. The
// exceptions in usageClassFor keep exit 1 where 2 already means something
// else.
//
// It runs before installJSONErrorContract, so the --json wrapper sees a coded
// error and reports the exit status the class chose.
func classifyUsageErrors(root *cobra.Command) {
	for _, c := range root.Commands() {
		classifyUsageErrors(c)
	}

	prevFlagErr := root.FlagErrorFunc()
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return asUsageError(c, prevFlagErr(c, err))
	})

	if prev := root.Args; prev != nil {
		root.Args = func(c *cobra.Command, args []string) error {
			return asUsageError(c, prev(c, args))
		}
	}
}

// asUsageError tags err with cmd's usage exit code unless it already carries
// one. Help requests and nil pass through.
func asUsageError(cmd *cobra.Command, err error) error {
	if err == nil || errors.Is(err, pflag.ErrHelp) {
		return err
	}
	var coded *codedError
	var silent *silentCodedError
	if chainAs(err, &coded) || chainAs(err, &silent) {
		return err
	}
	return WithExitCode(err, usageExitFor(cmd))
}

// usageExitFor is the exit code for a usage error on cmd: classExit(classUsage)
// except where 2 already means something else in that verb, or where the verb
// must never exit 2.
//
//   - tasks: 2 is "instance unreachable", an external probe depends on it.
//   - env check: 2 is "file absent", part of its documented contract.
//   - resume snapshot: wired as a Claude Code Stop hook, and a Stop hook that
//     exits 2 is a blocking error.
//   - k8s: pass-through, kubectl's codes are not ours.
func usageExitFor(cmd *cobra.Command) int {
	if usageKeepsOne(cmd) {
		return classExit(classFailed)
	}
	return classExit(classUsage)
}

func usageKeepsOne(cmd *cobra.Command) bool {
	var path []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		path = append([]string{c.Name()}, path...)
	}
	switch {
	case len(path) == 0:
		return false
	case path[0] == "tasks", path[0] == "k8s":
		return true
	case len(path) >= 2 && path[0] == "env" && path[1] == "check":
		return true
	case len(path) >= 2 && path[0] == "resume" && path[1] == "snapshot":
		return true
	}
	return false
}

// jsonCodedError carries the --json `code` a failure reports, for an error
// raised inside RunE that is a usage error but would otherwise be reported as
// `failed` (RunE's default). It never changes the exit status.
type jsonCodedError struct {
	err  error
	code string
}

func (e *jsonCodedError) Error() string { return e.err.Error() }
func (e *jsonCodedError) Unwrap() error { return e.err }

// withJSONUsageCode marks err so jsonFailure reports it as usage_error.
func withJSONUsageCode(err error) error {
	if err == nil {
		return nil
	}
	return &jsonCodedError{err: err, code: jsonCodeUsage}
}

// preFangUsageExit is the exit code for a setup failure raised before any
// command tree exists (an unresolvable $HOME, a relative $XDG_CONFIG_HOME). It
// reads the exceptions from argv by literal verb name, as invokesHookVerb
// does, so `tasks` and `env check` keep 1 where their 2 means something else.
func preFangUsageExit(args []string) int {
	first, idx := firstNonFlag(args)
	switch first {
	case "tasks", "k8s":
		return classExit(classFailed)
	case "env":
		if idx >= 0 {
			if next, _ := firstNonFlag(args[idx+1:]); next == "check" {
				return classExit(classFailed)
			}
		}
	}
	return classExit(classUsage)
}
