// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// nameUsageArgs makes a wrong argument count name the argument and the usage
// line (forgectl#1087). Cobra's own validators say only "Accepts 1 arg(s),
// received 0.", which names neither the missing argument nor the command's
// shape, so a caller has nothing to correct the call with.
//
// It walks the finished tree and wraps every leaf's validator. A validator that
// returns anything but cobra's count error (a verb's own message, a coded
// error carrying an exit status) passes through unchanged, and no exit code
// moves: the rewrite changes the text of the same error.
func nameUsageArgs(root *cobra.Command) {
	for _, c := range root.Commands() {
		nameUsageArgs(c)
	}
	if root.Args == nil || root.HasSubCommands() || root.Parent() == nil {
		return
	}
	prev := root.Args
	root.Args = func(cmd *cobra.Command, args []string) error {
		return usageArgError(cmd, args, prev(cmd, args))
	}
}

// usageArgError rewrites cobra's argument-count error into one line that names
// the missing or unexpected argument and ends with the usage line. Any other
// error, and nil, is returned as it came.
func usageArgError(cmd *cobra.Command, args []string, err error) error {
	if err == nil || !isCountError(err) {
		return err
	}
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	required := usagePlaceholders(cmd.Use)
	usage := "usage: " + cmd.UseLine()
	switch {
	case len(args) < len(required):
		return fmt.Errorf("%s: missing %s; %s", path, strings.Join(required[len(args):], " "), usage)
	case len(required) == 0 && len(args) > 0:
		return fmt.Errorf("%s: takes no arguments, got %s; %s", path, termsafe.QuoteArgMax(args[0], termsafe.ArgEchoMaxRunes), usage)
	case len(args) > len(required):
		return fmt.Errorf("%s: unexpected argument %s after %s; %s", path, termsafe.QuoteArgMax(args[len(required)], termsafe.ArgEchoMaxRunes), strings.Join(required, " "), usage)
	}
	// Optional and variadic shapes cobra refused: the count is wrong but the
	// placeholders cannot say by how much.
	return fmt.Errorf("%s: wrong number of arguments (%d); %s", path, len(args), usage)
}

// isCountError reports whether err is one of cobra's argument-count errors
// (ExactArgs, RangeArgs, MinimumNArgs, MaximumNArgs, NoArgs). A coded or silent
// error is never one: it already carries a verb's own contract.
func isCountError(err error) bool {
	var coded *codedError
	var silent *silentCodedError
	if errors.As(err, &coded) || errors.As(err, &silent) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "arg(s)") || strings.HasPrefix(msg, "unknown command ")
}

// usagePlaceholders returns the required positionals of a Use string, in
// order: a `<name>` or an ALL-CAPS word (`FILE`) outside any `[...]` group. It
// stops at an alternative (" | "), and skips the value of a flag
// (`--reason <text>`), which is not a positional.
func usagePlaceholders(use string) []string {
	fields := strings.Fields(use)
	if len(fields) == 0 {
		return nil
	}
	var out []string
	prev := ""
	depth := 0
	for _, f := range fields[1:] {
		if f == "|" {
			break
		}
		opens := strings.Count(f, "[")
		if depth == 0 && opens == 0 && !strings.HasPrefix(prev, "-") && isPlaceholder(f) {
			out = append(out, f)
		}
		depth += opens - strings.Count(f, "]")
		prev = f
	}
	return out
}

// isPlaceholder reports whether a Use token names a positional: `<name>`, or a
// word of capitals and underscores such as FILE.
func isPlaceholder(tok string) bool {
	if strings.HasPrefix(tok, "<") {
		return true
	}
	if len(tok) < 2 {
		return false
	}
	for _, r := range tok {
		if (r < 'A' || r > 'Z') && r != '_' {
			return false
		}
	}
	return true
}
