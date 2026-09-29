package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

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
// a root rg could not fully search, `docs check` with findings) exits 1 because
// the verb did run, and `docs read` hands mdroll's own exit status through once
// the child has started.

// docsErrorJSON is the --json wire shape of a docs "could not run" failure.
type docsErrorJSON struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Root  string `json:"root"`
}

// docsOSArgs is the process argument list docsWantsJSON falls back to when a
// flag-parse failure leaves the --json flag unset. Tests replace the seam.
var docsOSArgs = func() []string { return os.Args[1:] }

// docsFail renders a docs failure under the shared contract and returns the
// error the command hands back. Without --json it is WithExitCode(err, code),
// so the normal human error path renders it. With --json it writes one
// docsErrorJSON object to stderr and returns a silent coded error, so nothing
// is written on top of it. root names the root a failure stopped on, or "".
func docsFail(cmd *cobra.Command, verb, root string, err error, code int, asJSON bool) error {
	if !asJSON {
		return WithExitCode(err, code)
	}
	obj := docsErrorJSON{Error: err.Error(), Code: code, Root: root}
	if encErr := termsafe.JSONEncoder(cmd.ErrOrStderr()).Encode(obj); encErr != nil {
		return WithExitCode(fmt.Errorf("%s: encode error: %w", verb, encErr), code)
	}
	return newSilentCodedError(code)
}

// docsWantsJSON reports whether cmd was asked for --json. It scans the raw
// arguments (up to a "--" terminator) because a flag-parse failure stops pflag
// before it reached --json, leaving the parsed value false. The scan mirrors
// pflag: a bare --json is true, --json=<v> is strconv.ParseBool(v) (an
// unparseable value is ignored, as pflag would have rejected it), and the LAST
// occurrence wins. With no occurrence in the arguments the parsed flag value
// decides. A verb that does not declare --json never wants it.
func docsWantsJSON(cmd *cobra.Command) bool {
	if cmd.Flags().Lookup("json") == nil {
		return false
	}
	seen, want := false, false
scan:
	for _, a := range docsOSArgs() {
		switch {
		case a == "--":
			break scan
		case a == "--json":
			seen, want = true, true
		case strings.HasPrefix(a, "--json="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json=")); err == nil {
				seen, want = true, v
			}
		}
	}
	if seen {
		return want
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

// docsFlagError is the SetFlagErrorFunc every docs leaf installs: a bad flag
// is a "could not run" failure like any other.
func docsFlagError(verb string) func(*cobra.Command, error) error {
	return func(cmd *cobra.Command, err error) error {
		return docsFail(cmd, verb, "", err, 2, docsWantsJSON(cmd))
	}
}

// docsArgs wraps a cobra positional-argument validator so a count error takes
// the same contract as every other failure (cobra's own validators return a
// plain error, which exits 1 and never produces JSON).
func docsArgs(verb string, inner cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return docsFail(cmd, verb, "", err, 2, docsWantsJSON(cmd))
		}
		return nil
	}
}
