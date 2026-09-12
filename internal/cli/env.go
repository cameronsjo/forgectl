package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	envpkg "github.com/cameronsjo/forgectl/internal/env"
	"github.com/cameronsjo/forgectl/internal/module"
	sopspkg "github.com/cameronsjo/forgectl/internal/sops"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// confirmAnyFile is the --any-file confirmation seam — a package-level var
// (mirrors isTerminal/readPassword above) so a test can stub the huh prompt
// without a real tty, rather than exercising confirm() itself.
var confirmAnyFile = confirm

// isTerminal and readPassword are package-level seams over the REAL
// process stdin (not cmd.InOrStdin(), which tests point at a fake reader
// for the piped-stdin branch) — a test can't hand `set`'s interactive
// no-echo branch a genuine tty, so both are overridable in tests instead.
var (
	isTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	readPassword = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
)

// resolveEnvTarget resolves --file ONCE and returns the cleared target every
// env subcommand then operates on. It is the single place the env-file-name
// allowlist and the --any-file escape hatch are applied.
//
// # Why one resolution, and why the path travels
//
// This function used to return a bool, and each caller then re-resolved the
// raw --file string to decide what to touch. Two resolutions of the same
// mutable input, separated by the operator's think-time at a confirmation
// prompt, is a time-of-check/time-of-use gap — and it was exploitable, not
// theoretical. With `link -> notes.txt` in a repo, `env set sshCommand
// --file link --any-file` prompted with "notes.txt"; repointing `link` at
// `.git/config` before answering moved the write there, and `core.fsmonitor`
// is executed by the next `git status`. The confirmed path and the written
// path were simply two different files, and no test could have caught it
// because the seam carried nothing to compare.
//
// So a resolved env.Target is the return value. A bool cannot carry a path,
// which is what makes that gap unrepresentable rather than merely closed.
//
// Returning the path alone would not have been enough, and the first attempt
// at this fix proved it: with the path travelling but every consumer still
// opening it BY STRING, swapping an intermediate directory during the prompt
// reproduced the identical exploit — a prompt reading "sub/config" wrote
// core.fsmonitor into .git/config and exited zero. A Target therefore also
// carries an open descriptor on its containing directory, pinned at
// resolution, and every read, write, and rename happens relative to that
// descriptor. See env.dirPin for the mechanism and for the narrow race it
// does not claim to close.
//
// A Target owns that descriptor, so every caller here closes it.
//
// # What the TTY gate actually bounds
//
// The threat is an injected agent driving forgectl into writing an arbitrary
// repo file (.git/config, .envrc, a Makefile — any KEY=value-shaped sink),
// and an agent can pass --any-file exactly as easily as it can pass --file.
// The gate stops an agent with no pty, which is the common case: a harness
// tool call, a CI step, a pipeline. It does NOT stop an agent running inside
// a terminal multiplexer pane, where stdin is a real pty and a prompt is
// answerable — this repo's own estate runs agents that way. Read the gate as
// "raises the cost and covers the ptyless case", not as a boundary.
func resolveEnvTarget(anyFile bool, file, cwd string, th theme.Theme) (envpkg.Target, error) {
	target, err := envpkg.ResolveTarget(file, cwd)
	if err != nil {
		return envpkg.Target{}, err
	}
	clearErr := target.Clear()
	if clearErr == nil {
		return target, nil
	}
	if !anyFile {
		return envpkg.Target{}, clearErr
	}
	if !isTerminal() {
		// Phrased to lead with a word, not the flag: fang title-cases the
		// first token when it renders an error, so "--any-file requires …"
		// reaches the user as "--Any-File requires …" — a flag spelling
		// that does not exist and that someone will reasonably try to type.
		return envpkg.Target{}, errors.New("an interactive terminal is required for --any-file")
	}
	// Prompts with the repo-relative resolved path: resolved so a human
	// cannot approve a file they never saw (a `.env` symlinked to
	// `.git/config` must read ".git/config"), and relative because the
	// absolute form carries a machine-specific prefix into a transcript
	// (forgectl#481).
	ok, err := confirmAnyFile(th, fmt.Sprintf("%q is not a recognized env file (.env, .env.*, or *.env) — operate on it anyway?", target.Rel()))
	if err != nil {
		return envpkg.Target{}, err
	}
	if !ok {
		return envpkg.Target{}, fmt.Errorf("refusing %s: --any-file confirmation declined", target.Rel())
	}
	return target, nil
}

// envKeyPattern documents ValidKey's regex for CLI-side error messages —
// duplicated from internal/env's own (unexported) copy rather than
// exported across the package boundary; both derive from the same
// `^[A-Za-z_][A-Za-z0-9_]*$` source in document.go.
const envKeyPattern = "[A-Za-z_][A-Za-z0-9_]*"

// envModule declares the .env-management extension (ADR-0005): stateless
// (no config section — every knob is a per-invocation flag).
var envModule = module.Manifest{
	Name:      "env",
	Tier:      module.TierExtension,
	ConfigKey: "",
	New:       newEnvCmd,
}

// newEnvCmd builds `forgectl env` over the registry Deps.
func newEnvCmd(deps module.Deps) *cobra.Command {
	// clippkg.WithSensitive() suppresses the clipboard client's byte-length
	// log field — env's whole reason to exist is that a value never
	// prints, and a length is itself signal about a secret (the plan
	// declines a partial-redact reveal for the exact same reason).
	clip := clippkg.New(deps.Runner, clippkg.WithSensitive())
	client := envpkg.NewClient(clip)
	// The sops driver runs over the SENSITIVE seam, not deps.Runner: sops'
	// stderr quotes the line it failed to parse, and that line holds the
	// value. deps.SensitiveRunner is nil when a caller did not wire one, and
	// nil is correct to pass through — the sops route refuses rather than
	// silently falling back to the argv-logging path.
	return newEnvCmdForClient(client, sopspkg.NewClient(deps.SensitiveRunner), clip, deps.Theme)
}

// newEnvCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *env.Client (mirrors
// newYCmdForClient/newDockerCmdForClient) without going through newEnvCmd.
func newEnvCmdForClient(client *envpkg.Client, sopsClient *sopspkg.Client, clip *clippkg.Client, th theme.Theme) *cobra.Command {
	var file string
	var anyFile bool

	cmd := &cobra.Command{
		Use: "env",
		// SilenceUsage/SilenceErrors mirror root.go's own setting: `get`
		// without --clipboard is spec'd to print NOTHING to stdout, and
		// cobra's default auto-usage-on-error would otherwise do exactly
		// that when this command tree is exercised directly (as the tests
		// do) rather than through fang's root, which already silences both.
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Safely manage .env files — key names visible, values never",
		Long: `env manages .env files without ever putting a secret value in argv, terminal
output, or a session transcript: key names are always visible, values never
print. --file defaults to .env (relative to the current directory).

  forgectl env keys                    list KEY names only — never values
  forgectl env set KEY                 value from piped stdin, no-echo
                                        prompt, or --clipboard — never argv
  forgectl env get KEY --clipboard     value to clipboard only; no print
                                        path exists
  forgectl env check                   report missing/extra keys vs
                                        --example (default .env.example)
  forgectl env redact                  print the file with values masked

set's blessed value sources, non-inline producers first:

  op read op://vault/item/field | forgectl env set API_KEY   # 1Password by composition
  forgectl env set API_KEY < value.txt                       # from a file
  forgectl env set API_KEY --clipboard                       # from the clipboard
  forgectl env set API_KEY                                   # interactive, no echo

Never inline the secret in the producing command itself
(printf 'secret' | forgectl env set KEY) — that puts it in THAT command's own
argv and transcript; forgectl can't close a channel it doesn't own.`,
	}
	cmd.PersistentFlags().StringVar(&file, "file", ".env", "path to the .env file")
	cmd.PersistentFlags().BoolVar(&anyFile, "any-file", false, "allow a --file that isn't .env/.env.*/*.env, after an interactive confirmation (requires a tty)")

	cmd.AddCommand(
		newEnvKeysCmd(&file, &anyFile, th),
		newEnvSetCmd(client, sopsClient, clip, &file, &anyFile, th),
		newEnvGetCmd(client, &file, &anyFile, th),
		newEnvCheckCmd(&file, &anyFile, th),
		newEnvRedactCmd(&file, &anyFile, th),
	)
	return cmd
}

// readDocument opens and parses a cleared target — the shared read used by
// every subcommand that doesn't go through the domain Client (keys, check,
// redact touch no clipboard, so they read directly via env.OpenTarget/
// env.Parse rather than a Client method).
func readDocument(target envpkg.Target) (*envpkg.Document, error) {
	f, err := envpkg.OpenTarget(target)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	doc, err := envpkg.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", target.Rel(), err)
	}
	return doc, nil
}

// newEnvKeysCmd builds `env keys`.
func newEnvKeysCmd(file *string, anyFile *bool, th theme.Theme) *cobra.Command {
	return &cobra.Command{
		Use:   "keys",
		Short: "List KEY names — never values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			target, err := resolveEnvTarget(*anyFile, *file, cwd, th)
			if err != nil {
				return err
			}
			defer target.Close()
			if !target.Exists {
				return fmt.Errorf("env file %s not found", target.Rel())
			}
			doc, err := readDocument(target)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			for _, k := range doc.Keys() {
				fmt.Fprintln(out, k)
			}

			malformed := 0
			for _, l := range doc.Lines {
				if l.Kind == envpkg.KindMalformed {
					malformed++
				}
			}
			if malformed > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d malformed line(s)\n", malformed)
			}
			return nil
		},
	}
}

// newEnvSetCmd builds `env set`.
func newEnvSetCmd(client *envpkg.Client, sopsClient *sopspkg.Client, clip *clippkg.Client, file *string, anyFile *bool, th theme.Theme) *cobra.Command {
	var clipboard bool
	var useSops bool

	cmd := &cobra.Command{
		Use:   "set KEY",
		Short: "Set KEY's value — piped stdin, a no-echo prompt, or --clipboard; never argv",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			// The key gate BRANCHES, and it stays in this position.
			//
			// Checking here — before stdin is read or the clipboard is
			// touched — is what makes a hostile key shape (`env set
			// KEY=VALUE`) refuse without consuming input. But the two routes
			// have different grammars: ValidKey forbids dots and hyphens,
			// so under --sops it would reject every dotted path the feature
			// exists to accept. Branching keeps the ordering and admits the
			// right shape on each route.
			if useSops {
				if _, err := sopspkg.ParsePath(key); err != nil {
					return err
				}
			} else if !envpkg.ValidKey(key) {
				return fmt.Errorf("key must match %s; values are piped or --clipboard, never argv", envKeyPattern)
			}

			// --any-file is inert on the sops route (it never reaches the
			// env-file allowlist), so accepting it silently would imply a
			// bypass that does not exist. Refusing says so.
			if useSops && *anyFile {
				return errors.New("--any-file does not apply with --sops; a SOPS target must match " + sopspkg.NameShapes())
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			if useSops {
				return runEnvSetSops(cmd, sopsClient, clip, *file, cwd, key, clipboard)
			}

			target, err := resolveEnvTarget(*anyFile, *file, cwd, th)
			if err != nil {
				return err
			}
			defer target.Close()

			var tightened bool
			if clipboard {
				tightened, err = client.SetFromClipboard(cmd.Context(), target, key)
			} else {
				var value string
				value, err = resolveSetValue(cmd)
				if err != nil {
					return err
				}
				tightened, err = client.SetValue(target, key, value)
			}
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "set %s in %s\n", key, target.Rel())
			if tightened {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "tightened %s to 0600\n", target.Rel())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clipboard, "clipboard", false, "read the value from the clipboard (wins over piped stdin)")
	cmd.Flags().BoolVar(&useSops, "sops", false, "write one dotted KEY path into a SOPS-encrypted YAML file instead of a .env file")
	return cmd
}

// sopsDefaultFile is the target `--sops` uses when --file was not given. It
// resolves against the REPOSITORY ROOT rather than the current directory,
// which is where these files live; without that the .env default would
// silently aim at the wrong file and the failure would read as a sops problem
// rather than a path one.
const sopsDefaultFile = "secrets.sops.yaml"

// runEnvSetSops is the --sops route: resolve and gate the target, source the
// value, hand both to the driver.
//
// # The gate, and why there is no escape hatch
//
// A SOPS file is named secrets.sops.yaml, which the .env allowlist refuses —
// so this route needs an allowlist of its own rather than a bypass of that
// one. Three conditions, all required: the path resolves inside the
// repository (env.ResolveTarget), its basename matches a SOPS shape, and its
// CONTENT carries a top-level sops: mapping. The name check alone would admit
// a plain YAML file someone called secrets.sops.yaml; the content check alone
// would admit any encrypted document anywhere in the tree.
//
// A SOPS file under some other name is unreachable, deliberately. The
// alternative is an interactive confirmation, and that is the path that
// carried the time-of-check/time-of-use defect fixed in the commit this
// branch sits on. Refusing costs a rename and takes a whole class of bug off
// the table.
func runEnvSetSops(cmd *cobra.Command, sopsClient *sopspkg.Client, clip *clippkg.Client, file, cwd, key string, clipboard bool) error {
	// The .env default would aim at the wrong file, so substitute this
	// route's own default when --file was not given. Changed() reads the
	// parent's persistent flag correctly — pflag shares the *Flag pointer.
	if !cmd.Flags().Changed("file") {
		root, err := envpkg.RepoRoot(cwd)
		if err != nil {
			return err
		}
		file = filepath.Join(root, sopsDefaultFile)
	}

	target, err := envpkg.ResolveTarget(file, cwd)
	if err != nil {
		return err
	}
	defer target.Close()

	// The NAME check precedes the existence check, and the order matters twice
	// over. A refused name should refuse on the name whatever the filesystem
	// says — "not found" is the wrong reason and sends the operator looking
	// for a missing file rather than at their --file argument. And answering
	// existence first turns a refused path into an existence oracle, which is
	// a disclosure a refusal has no business making.
	if !sopspkg.IsSOPSFileName(filepath.Base(target.Abs())) {
		return fmt.Errorf("refusing %s: --sops requires a target named one of %s", target.Rel(), sopspkg.NameShapes())
	}
	if !target.Exists {
		// Creating a file is out of scope, and a rule-named refusal beats a
		// raw os.Open error that reads as an internal fault.
		return fmt.Errorf("%s not found; --sops edits an existing SOPS file and does not create one", target.Rel())
	}

	// Sourced after the target is gated, so a refusable target never consumes
	// input — the same ordering the key gate follows.
	var value string
	if clipboard {
		// client.SetFromClipboard runs the whole .env pipeline and would
		// append a plaintext KEY=value line to an encrypted file, so the
		// clipboard is read directly here and handed to the sops driver.
		value, err = clip.Paste(cmd.Context())
	} else {
		value, err = resolveSetValue(cmd)
	}
	if err != nil {
		return err
	}

	outcome, err := sopsClient.SetValue(cmd.Context(), target, key, value)
	if err != nil {
		return err
	}

	// The outcomes are distinguished because replacing a key is a rotation
	// and adding one is new configuration — an operator reading a transcript
	// wants to know which happened.
	switch outcome {
	case sopspkg.OutcomeAdded:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added %s to %s\n", key, target.Rel())
	default:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "replaced %s in %s\n", key, target.Rel())
	}
	return nil
}

// resolveSetValue reads the value `set` will use when --clipboard wasn't
// given: piped stdin when the real stdin isn't a terminal, else an
// interactive no-echo prompt. The trailing-newline strip and empty-value
// refusal happen downstream, in the domain's shared set pipeline — this
// only sources the raw string.
//
// The prompt deliberately does NOT name the key. This package's asymmetry
// rule is that a token is safe to echo only once it is provably a key name,
// and `set`'s success message earns that by having written it; a prompt
// earns nothing, because it precedes any write and is abandoned on Ctrl-C.
// `forgectl env set sk_live_51H8xY2eZvKYlo2C` is an ordinary typo, and
// naming the argument here would put that secret in the transcript with
// nothing written to show for it. The operator just typed the key, so
// repeating it adds no information anyway.
func resolveSetValue(cmd *cobra.Command) (string, error) {
	if !isTerminal() {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}

	_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Value: ")
	value, err := readPassword()
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read value: %w", err)
	}
	return value, nil
}

// newEnvGetCmd builds `env get`.
func newEnvGetCmd(client *envpkg.Client, file *string, anyFile *bool, th theme.Theme) *cobra.Command {
	var clipboard bool

	cmd := &cobra.Command{
		Use:   "get KEY",
		Short: "Copy KEY's value to the clipboard — requires --clipboard; no print path exists",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !clipboard {
				return errors.New("get requires --clipboard; there is no path to print a value")
			}
			key := args[0]
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			target, err := resolveEnvTarget(*anyFile, *file, cwd, th)
			if err != nil {
				return err
			}
			defer target.Close()
			if err := client.CopyValue(cmd.Context(), target, key); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "copied %s to clipboard\n", key)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clipboard, "clipboard", false, "copy the value to the clipboard (required)")
	return cmd
}

// newEnvCheckCmd builds `env check`. Exit codes are part of its contract
// (forgectl#104): 2 means --file or --example is absent (nothing to
// compare), 1 means the file and example were compared and differ (missing
// and/or extra keys — either counts as drift), 0 means clean. --json
// (forgectl#105) emits the same verdict as {"missing":[...],"extra":[...]}
// on stdout instead of the human sections, under the identical exit codes.
func newEnvCheckCmd(file *string, anyFile *bool, th theme.Theme) *cobra.Command {
	var example string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Report keys missing from/extra vs --example (default .env.example) — names only",
		Long: `check reports keys missing from, or extra in, --file compared against --example
(default .env.example) — names only, values never read for comparison.

Exit codes: 0 the file matches the example · 1 keys are missing or extra · 2 the file or the example was not found`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			fileTarget, err := resolveEnvTarget(*anyFile, *file, cwd, th)
			if err != nil {
				return err
			}
			defer fileTarget.Close()
			if !fileTarget.Exists {
				return notFoundCheckError(cmd, fileTarget, "env file %s not found", asJSON)
			}
			fileDoc, err := readDocument(fileTarget)
			if err != nil {
				return err
			}

			exampleTarget, err := resolveEnvTarget(*anyFile, example, cwd, th)
			if err != nil {
				return err
			}
			defer exampleTarget.Close()
			if !exampleTarget.Exists {
				return notFoundCheckError(cmd, exampleTarget, "example file %s not found", asJSON)
			}
			exampleDoc, err := readDocument(exampleTarget)
			if err != nil {
				return err
			}

			missing, extra := envpkg.Diff(fileDoc, exampleDoc)
			drift := len(missing) > 0 || len(extra) > 0

			if asJSON {
				if err := writeCheckJSON(cmd.OutOrStdout(), missing, extra); err != nil {
					return err
				}
			} else {
				out := cmd.OutOrStdout()
				// Only print a section that has names under it — a bare
				// "extra:" header with nothing beneath reads as a truncated
				// list rather than as "there are none".
				printSection(out, "missing:", missing)
				printSection(out, "extra:", extra)
				if !drift {
					// Clean: stdout stays empty so a caller can treat any
					// output as drift, and the reassurance goes to stderr.
					fmt.Fprintf(cmd.ErrOrStderr(), "%s matches %s\n", *file, example)
				}
			}

			if drift {
				return WithExitCode(
					fmt.Errorf("%d missing, %d extra key(s) between %s and %s", len(missing), len(extra), *file, example),
					1,
				)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&example, "example", ".env.example", "path to the example file to check against")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"missing":[...],"extra":[...]} to stdout instead of the human sections`)
	return cmd
}

// notFoundCheckError reports --file or --example being absent. Under
// --json it writes the agent-facing contract — exactly one
// {"error":"env file not found","code":"file_not_found","path":"…"} object
// on stderr, stdout untouched — and returns a silentCodedError so fang
// renders nothing on top of it; otherwise it returns the human wording
// (wordingFmt, one of "env file %s not found" / "example file %s not
// found") wrapped for exit 2. Both surfaces use the same repo-relative path
// so they can't drift (security ruling, forgectl#481): the resolved
// absolute path can name a directory the caller never typed, and --json
// output lands in agent transcripts verbatim.
func notFoundCheckError(cmd *cobra.Command, target envpkg.Target, wordingFmt string, asJSON bool) error {
	rel := target.Rel()
	if asJSON {
		if err := writeCheckErrorJSON(cmd.ErrOrStderr(), rel); err != nil {
			return err
		}
		return newSilentCodedError(2)
	}
	// wordingFmt is always one of the two fixed local literals passed by
	// the RunE closures above — never derived from input.
	return WithExitCode(fmt.Errorf(wordingFmt, rel), 2)
}

// checkErrorJSON is env check --json's file-not-found wire shape
// (forgectl#481) — distinct from checkJSON, which reports a completed
// comparison's missing/extra keys.
type checkErrorJSON struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Path  string `json:"path"`
}

// writeCheckErrorJSON encodes the not-found object to out (stderr).
func writeCheckErrorJSON(out io.Writer, path string) error {
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(checkErrorJSON{Error: "env file not found", Code: "file_not_found", Path: path})
}

// printSection writes a check section and its key names, and writes
// nothing at all when there are none.
func printSection(out io.Writer, header string, keys []string) {
	if len(keys) == 0 {
		return
	}
	fmt.Fprintln(out, header)
	for _, k := range keys {
		fmt.Fprintln(out, "  "+k)
	}
}

// checkJSON is the --json wire shape for `env check` (forgectl#105) — always
// both keys, arrays never null, matching the `projects list --json`
// empty-[] convention so a clean result reads as {"missing":[],"extra":[]}
// rather than a null a caller must guard against.
type checkJSON struct {
	Missing []string `json:"missing"`
	Extra   []string `json:"extra"`
}

// writeCheckJSON encodes missing/extra as checkJSON to out.
func writeCheckJSON(out io.Writer, missing, extra []string) error {
	if missing == nil {
		missing = []string{}
	}
	if extra == nil {
		extra = []string{}
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(checkJSON{Missing: missing, Extra: extra})
}

// newEnvRedactCmd builds `env redact`.
func newEnvRedactCmd(file *string, anyFile *bool, th theme.Theme) *cobra.Command {
	return &cobra.Command{
		Use:   "redact",
		Short: "Print --file with every value masked (****)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			target, err := resolveEnvTarget(*anyFile, *file, cwd, th)
			if err != nil {
				return err
			}
			defer target.Close()
			if !target.Exists {
				return fmt.Errorf("env file %s not found", target.Rel())
			}
			doc, err := readDocument(target)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc.Redacted())
			return err
		},
	}
}
