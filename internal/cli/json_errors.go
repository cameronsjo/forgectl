// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The --json stderr contract (forgectl#862; docs/json-contract.md). Once a
// verb runs with --json, a non-zero exit never renders fang's human error
// frame. Either:
//
//   - (a) the verb already wrote its JSON verdict on stdout, so it exits with
//     its code and writes nothing more (jsonVerdict); or
//   - (b) it failed before emitting, so stderr carries exactly one
//     jsonFailureObject (jsonFailure) and stdout stays empty.
//
// installJSONErrorContract applies (b) to every command in the tree that
// declares --json; each verb that emits a verdict and then exits non-zero
// applies (a) itself. The exit code is never changed by --json.
//
// Two verb families keep the shapes they shipped with, because callers parse
// them: env check uses its own codes (check_failed, file_not_found) through
// this same helper, and the docs verbs keep docsErrorJSON's integer code and
// root field (docs_errors.go).

// The code strings a generic --json failure object carries.
const (
	// jsonCodeUsage: cobra rejected a flag or a positional argument before
	// the verb ran.
	jsonCodeUsage = "usage_error"
	// jsonCodeFailed: the verb ran and failed before emitting its verdict.
	jsonCodeFailed = "failed"
)

// jsonFailureObject is the --json failure wire shape on stderr: always all
// three keys, path "" when no single resolved file is to blame.
type jsonFailureObject struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Path  string `json:"path"`
}

// jsonFailurePath is implemented by an error that knows the one resolved,
// repo-relative file it is about (envTargetError); jsonFailure reports it as
// the object's path.
type jsonFailurePath interface {
	jsonFailurePath() string
}

// jsonFailure renders err under the --json contract. Without asJSON, or for a
// nil err, err passes through untouched to the human renderer. An error the
// verb already rendered itself (a silentCodedError anywhere in the chain)
// comes back as that silentCodedError, unwrapped, because
// termsafeErrorHandler matches the concrete type and would render a wrapper's
// message. Anything else is written to stderr as one
// jsonFailureObject with the given code and handed back as a silentCodedError
// carrying the exit code err already had.
func jsonFailure(cmd *cobra.Command, err error, asJSON bool, code string) error {
	if err == nil || !asJSON {
		return err
	}
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return silent
	}
	path := ""
	var withPath jsonFailurePath
	if chainAs(err, &withPath) {
		path = withPath.jsonFailurePath()
	}
	enc := termsafe.JSONEncoder(cmd.ErrOrStderr())
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(jsonFailureObject{Error: err.Error(), Code: code, Path: path}); encErr != nil {
		return err
	}
	return newSilentCodedError(ExitCode(err))
}

// jsonVerdict is (a): the verb has already written its JSON verdict on stdout,
// so under asJSON a non-zero exit keeps its code and renders nothing. Without
// asJSON, err passes through to the human renderer.
func jsonVerdict(err error, asJSON bool) error {
	if err == nil || !asJSON {
		return err
	}
	return newSilentCodedError(ExitCode(err))
}

// jsonFlagParsed reports whether cmd's --json flag parsed true. It is the
// right question everywhere flags have already been parsed: Args validators
// and RunE. A verb that does not declare --json never wants it.
func jsonFlagParsed(cmd *cobra.Command) bool {
	if cmd.Flags().Lookup("json") == nil {
		return false
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

// jsonOSArgs is the process argument list argvWantsJSON scans. Tests replace
// the seam.
var jsonOSArgs = func() []string { return os.Args[1:] }

// argvWantsJSON reports whether cmd was asked for --json, for the one caller
// that cannot trust the parsed flag: a flag-error handler, where a parse
// failure stopped pflag before it reached --json. It scans the raw arguments
// (up to a "--" terminator), mirroring pflag: a token that is the value of a
// known value-taking flag is skipped, a bare --json is true,
// --json=<v> is strconv.ParseBool(v) (an unparseable value is ignored, as
// pflag would have rejected it), and the LAST occurrence wins. With no
// occurrence in the arguments the parsed flag value decides. A verb that does
// not declare --json never wants it.
func argvWantsJSON(cmd *cobra.Command) bool {
	if cmd.Flags().Lookup("json") == nil {
		return false
	}
	seen, want := false, false
	args := jsonOSArgs()
scan:
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			break scan
		case a == "--json":
			seen, want = true, true
		case strings.HasPrefix(a, "--json="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json=")); err == nil {
				seen, want = true, v
			}
		case takesSeparateValue(cmd, a):
			// pflag hands the next token to this flag as its value, so a
			// --json there (`--host --json`) is data, not the flag.
			i++
		}
	}
	if seen {
		return want
	}
	return jsonFlagParsed(cmd)
}

// takesSeparateValue reports whether token a is a flag cmd knows that takes
// its value from the following token: `--name` or `-n` with no inline value,
// on a flag that has no NoOptDefVal (a bool's is "true"). An unknown flag is
// assumed not to take one, since pflag stops at it anyway.
func takesSeparateValue(cmd *cobra.Command, a string) bool {
	var f *pflag.Flag
	switch {
	case strings.HasPrefix(a, "--") && len(a) > 2 && !strings.Contains(a, "="):
		name := a[2:]
		if f = cmd.Flags().Lookup(name); f == nil {
			f = cmd.InheritedFlags().Lookup(name)
		}
	case len(a) == 2 && a[0] == '-' && a[1] != '-':
		short := a[1:]
		if f = cmd.Flags().ShorthandLookup(short); f == nil {
			f = cmd.InheritedFlags().ShorthandLookup(short)
		}
	}
	return f != nil && f.NoOptDefVal == ""
}

// installJSONErrorContract walks root's tree and, on every command that
// declares a boolean --json flag, routes the three places an error can leave
// cobra through jsonFailure: the flag-error handler (code usage_error, --json
// read from raw argv), the positional-argument validator (usage_error, parsed
// flag), and RunE (failed, parsed flag). Each wrapper composes over what the
// command already installed, so a verb with its own contract (env check, the
// docs verbs) renders first and its silentCodedError passes through.
//
// A nil Args is left alone: cobra's default for a leaf accepts anything, and
// for a parent it runs the legacy unknown-subcommand check in Find, which
// setting Args would disable. Cobra's required-flag and flag-group checks
// also bypass all three sites; TestJSONStderr_NoUnwrappedErrorSites keeps
// every --json verb free of them.
func installJSONErrorContract(root *cobra.Command) {
	for _, c := range root.Commands() {
		installJSONErrorContract(c)
	}
	f := root.Flags().Lookup("json")
	if f == nil || f.Value.Type() != "bool" {
		return
	}
	cmd := root

	prevFlagErr := cmd.FlagErrorFunc()
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return jsonFailure(c, prevFlagErr(c, err), argvWantsJSON(c), jsonCodeUsage)
	})

	if prevArgs := cmd.Args; prevArgs != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			return jsonFailure(c, prevArgs(c, args), jsonFlagParsed(c), jsonCodeUsage)
		}
	}

	if prevRunE := cmd.RunE; prevRunE != nil {
		cmd.RunE = func(c *cobra.Command, args []string) error {
			return jsonFailure(c, prevRunE(c, args), jsonFlagParsed(c), jsonCodeFailed)
		}
	}
}
