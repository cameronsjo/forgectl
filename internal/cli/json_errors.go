// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cameronsjo/forgectl/internal/meta"
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
	var jsonCoded *jsonCodedError
	if chainAs(err, &jsonCoded) {
		code = jsonCoded.code
	}
	var withPath jsonFailurePath
	if chainAs(err, &withPath) {
		path = withPath.jsonFailurePath()
	}
	enc := termsafe.JSONEncoder(cmd.ErrOrStderr())
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(jsonFailureObject{Error: jsonErrorText(err), Code: code, Path: path}); encErr != nil {
		return err
	}
	return newSilentCodedError(ExitCode(err))
}

// jsonErrTextUnavailable stands in for the text of an error whose Error
// method panicked. It matches termsafe's wording for the same case.
const jsonErrTextUnavailable = "error text unavailable: its Error method panicked"

// jsonErrorText is err.Error(), or jsonErrTextUnavailable when that call
// panics. A bare typed-nil error (a nil *os.PathError returned as error) is
// non-nil, and its Error method dereferences the nil receiver; fmt recovers
// such a panic but a direct call does not. Like termsafe's errorText, the
// recovery logs the Go types only, never the panic value.
func jsonErrorText(err error) (text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Error method panicked; its text is withheld.",
				"error_type", fmt.Sprintf("%T", err), "panic_type", fmt.Sprintf("%T", r))
			text = jsonErrTextUnavailable
		}
	}()
	return err.Error()
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
	return argvWantsJSONIn(cmd, jsonOSArgs())
}

// argvWantsJSONIn is argvWantsJSON over an explicit argument list, for the
// callers that hold the argv themselves (Execute, before fang starts).
func argvWantsJSONIn(cmd *cobra.Command, args []string) bool {
	if cmd.Flags().Lookup("json") == nil {
		return false
	}
	seen, want := false, false
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
// its value from the following token: `--name` with no inline value, on a
// flag that has no NoOptDefVal (a bool's is "true"), or a shorthand group
// whose value flag is its last letter. An unknown flag is assumed not to take
// one, since pflag stops at it anyway.
func takesSeparateValue(cmd *cobra.Command, a string) bool {
	switch {
	case strings.HasPrefix(a, "--"):
		if len(a) == 2 || strings.Contains(a, "=") {
			return false
		}
		f := lookupFlag(cmd, a[2:])
		return f != nil && f.NoOptDefVal == ""
	case len(a) > 1 && a[0] == '-':
		return shorthandGroupTakesNext(cmd, a[1:])
	}
	return false
}

// shorthandGroupTakesNext mirrors pflag's parseShortArg over a group of
// shorthands (the token without its dash): each letter that names a flag with
// a NoOptDefVal (a bool) is consumed on its own, and the first letter that
// names a value flag takes the rest of the group as its value, or the next
// token when it is the last letter. So in `-vH` with a bool v and a value
// flag H, H takes the next token, and in `-Hv`, H takes "v". An unknown letter
// stops pflag with an error, so it takes nothing. `-x=v` needs no case of its
// own: a value flag x is not the last letter, and a bool x moves on to "=",
// which names no shorthand.
func shorthandGroupTakesNext(cmd *cobra.Command, group string) bool {
	for i := 0; i < len(group); i++ {
		f := lookupShorthand(cmd, group[i:i+1])
		switch {
		case f == nil:
			return false
		case f.NoOptDefVal != "":
			continue
		default:
			return i == len(group)-1
		}
	}
	return false
}

// lookupFlag finds a long flag cmd declares or inherits.
func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	return cmd.InheritedFlags().Lookup(name)
}

// lookupShorthand finds a one-letter shorthand cmd declares or inherits.
func lookupShorthand(cmd *cobra.Command, short string) *pflag.Flag {
	if f := cmd.Flags().ShorthandLookup(short); f != nil {
		return f
	}
	return cmd.InheritedFlags().ShorthandLookup(short)
}

// preFangFailure reports err, raised in Execute before fang starts (the
// env-snapshot failure, the config-parse gate, a launch-intercept error). A
// verb run with --json gets the one stderr object its family always writes
// (jsonFamilyFailure), handed back as a silentCodedError with err's exit code;
// anything else gets the plain `forgectl: <message>` line and err itself.
//
// root builds or returns the command tree. It is called only when args
// mention --json at all, because the env-snapshot failure happens before
// Execute builds the tree and must not build it for nothing.
func preFangFailure(root func() *cobra.Command, args []string, err error) error {
	if cmd := preFangJSONTarget(root, args); cmd != nil {
		return jsonFamilyFailure(cmd, err)
	}
	_, _ = fmt.Fprintln(os.Stderr, meta.AppName+": "+safeText(jsonErrorText(err)))
	return err
}

// preFangJSONTarget returns the command args would run when it declares
// --json and args ask for it, or nil. Flags have not parsed yet, so args are
// scanned the way argvWantsJSON scans the raw argv: `--json` after a "--", or
// as another flag's value, does not count.
//
// The first loop only decides whether building the tree is worth it, so it
// must never miss a --json that argvWantsJSONIn would count. A "--" right
// after a token that could be a flag waiting for its value (`--host --
// --json`) may be that value rather than the terminator, and without the tree
// there is no telling which, so the scan keeps going and argvWantsJSONIn's
// value-skip settles it.
func preFangJSONTarget(root func() *cobra.Command, args []string) *cobra.Command {
	mentioned := false
	for i, a := range args {
		if a == "--" && (i == 0 || !mayTakeNextValue(args[i-1])) {
			break
		}
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return nil
	}
	cmd, _, err := root().Find(args)
	if err != nil || cmd == nil || !argvWantsJSONIn(cmd, args) {
		return nil
	}
	return cmd
}

// mayTakeNextValue reports whether a, read with no command tree, could be a
// flag that takes the following token as its value: a dash-led token other
// than "--" with no inline "=value". It over-reports (a bool flag qualifies),
// which only costs building the tree.
func mayTakeNextValue(a string) bool {
	return len(a) > 1 && a[0] == '-' && a != "--" && !strings.Contains(a, "=")
}

// jsonFamilyFailure writes err as the one --json failure object cmd's family
// writes, and returns the silentCodedError carrying err's exit code. The docs
// verbs keep docsErrorJSON's integer code, env check keeps check_failed, and
// every other verb gets code "failed".
func jsonFamilyFailure(cmd *cobra.Command, err error) error {
	switch {
	case isDocsVerb(cmd):
		return docsFail(cmd, cmd.CommandPath(), "", err, ExitCode(err), true)
	case cmd.Name() == "check" && cmd.HasParent() && cmd.Parent().Name() == "env":
		return jsonFailure(cmd, err, true, "check_failed")
	default:
		return jsonFailure(cmd, err, true, jsonCodeFailed)
	}
}

// isDocsVerb reports whether cmd sits under the top-level docs command.
func isDocsVerb(cmd *cobra.Command) bool {
	for c := cmd; c.HasParent(); c = c.Parent() {
		if !c.Parent().HasParent() {
			return c.Name() == "docs"
		}
	}
	return false
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
