package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The docs "could not run" contract (forgectl#604). Every docs verb that fails
// BEFORE it starts its real work exits 2. Under --json, on a verb that declares
// the flag, that failure writes exactly one docsErrorJSON object to stderr and
// nothing to stdout; the object always carries "root" (empty when no single root
// is to blame), so the key `docs list` documented first is never omitted
// (ADR-0008 rule 2: additive only).
//
// Two things are deliberately outside it: a partial result (`docs search` with
// a root rg could not fully search, `docs check` with error-severity findings) exits 1 because
// the verb did run, and `docs read` hands mdroll's own exit status through once
// the child has started.

// docsErrorJSON is the --json wire shape of a docs "could not run" failure.
type docsErrorJSON struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Root  string `json:"root"`
}

// docsFail renders a docs failure under the shared contract and returns the
// error the command hands back. Without --json it is WithExitCode(err, code),
// so the normal human error path renders it. With --json it writes one
// docsErrorJSON object to stderr and returns a silent coded error, so nothing
// is written on top of it. root names the root a failure stopped on, or "".
func docsFail(cmd *cobra.Command, verb, root string, err error, code int, asJSON bool) error {
	if !asJSON {
		return WithExitCode(err, code)
	}
	obj := docsErrorJSON{Error: jsonErrorText(err), Code: code, Root: root}
	if encErr := termsafe.JSONEncoder(cmd.ErrOrStderr()).Encode(obj); encErr != nil {
		return WithExitCode(fmt.Errorf("%s: encode error: %w", verb, encErr), code)
	}
	return newSilentCodedError(code)
}

// docsFlagError is the SetFlagErrorFunc every docs leaf installs: a bad flag
// is a "could not run" failure like any other.
func docsFlagError(verb string) func(*cobra.Command, error) error {
	return func(cmd *cobra.Command, err error) error {
		return docsFail(cmd, verb, "", err, 2, argvWantsJSON(cmd))
	}
}

// docsArgs wraps a cobra positional-argument validator so a count error takes
// the same contract as every other failure (cobra's own validators return a
// plain error, which exits 1 and never produces JSON). Flags are parsed by the
// time Args runs, so --json is read from the parsed flag (forgectl#862).
func docsArgs(verb string, inner cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return docsFail(cmd, verb, "", usageArgError(cmd, args, err), 2, jsonFlagParsed(cmd))
		}
		return nil
	}
}
