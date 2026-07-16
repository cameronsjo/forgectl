package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	envpkg "github.com/cameronsjo/forgectl/internal/env"
	"github.com/cameronsjo/forgectl/internal/module"
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

// resolveAllowAnyFile decides the allowAnyFile bool env.Locate consults,
// enforcing the --any-file escape hatch's TTY gate. A flag an agent can
// type is not a bound on an agent — the threat this whole command defends
// against is an injected agent driving forgectl into writing an arbitrary
// repo file (.git/config, .envrc, a Makefile — any KEY=value-shaped sink),
// and an agent can pass --any-file exactly as easily as it can pass --file.
// What it cannot do is answer an interactive confirmation a human grants in
// seconds — so the TTY gate, not the flag, is the actual bound. When
// anyFile is false this is a no-op (the ordinary env-file-name check in
// Locate applies); when anyFile is true it either refuses outright (no
// tty) or gates the skip on an explicit "yes".
func resolveAllowAnyFile(anyFile bool, file string) (bool, error) {
	if !anyFile {
		return false, nil
	}
	if !isTerminal() {
		return false, errors.New("--any-file requires an interactive terminal")
	}
	ok, err := confirmAnyFile(fmt.Sprintf("%s is not a recognized env file (.env, .env.*, or *.env) — operate on it anyway?", file))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("refusing %s: --any-file confirmation declined", file)
	}
	return true, nil
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
	client := envpkg.NewClient(clippkg.New(deps.Runner, clippkg.WithSensitive()))
	return newEnvCmdForClient(client)
}

// newEnvCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *env.Client (mirrors
// newYCmdForClient/newDockerCmdForClient) without going through newEnvCmd.
func newEnvCmdForClient(client *envpkg.Client) *cobra.Command {
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
		newEnvKeysCmd(&file, &anyFile),
		newEnvSetCmd(client, &file, &anyFile),
		newEnvGetCmd(client, &file, &anyFile),
		newEnvCheckCmd(&file, &anyFile),
		newEnvRedactCmd(&file, &anyFile),
	)
	return cmd
}

// readDocument opens and parses realPath — the shared read used by every
// subcommand that doesn't go through the domain Client (keys, check,
// redact touch no clipboard, so they read directly via the exported
// env.Locate/env.Parse rather than a Client method).
func readDocument(realPath string) (*envpkg.Document, error) {
	f, err := os.Open(realPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", realPath, err)
	}
	defer f.Close()
	doc, err := envpkg.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", realPath, err)
	}
	return doc, nil
}

// newEnvKeysCmd builds `env keys`.
func newEnvKeysCmd(file *string, anyFile *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "keys",
		Short: "List KEY names — never values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			allow, err := resolveAllowAnyFile(*anyFile, *file)
			if err != nil {
				return err
			}
			realPath, exists, err := envpkg.Locate(*file, cwd, allow)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%s not found", realPath)
			}
			doc, err := readDocument(realPath)
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
func newEnvSetCmd(client *envpkg.Client, file *string, anyFile *bool) *cobra.Command {
	var clipboard bool

	cmd := &cobra.Command{
		Use:   "set KEY",
		Short: "Set KEY's value — piped stdin, a no-echo prompt, or --clipboard; never argv",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			// Checked here, BEFORE reading stdin or touching the clipboard —
			// not just inside the domain pipeline — so a hostile key shape
			// (env set KEY=VALUE) refuses without ever consuming input.
			// "ValidKey first, refuse before touching the file or reading
			// input" applies to the CLI's own input-sourcing step, too.
			if !envpkg.ValidKey(key) {
				return fmt.Errorf("key must match %s; values are piped or --clipboard, never argv", envKeyPattern)
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			allow, err := resolveAllowAnyFile(*anyFile, *file)
			if err != nil {
				return err
			}

			var tightened bool
			if clipboard {
				tightened, err = client.SetFromClipboard(cmd.Context(), cwd, *file, key, allow)
			} else {
				var value string
				value, err = resolveSetValue(cmd, key)
				if err != nil {
					return err
				}
				tightened, err = client.SetValue(cwd, *file, key, value, allow)
			}
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "set %s in %s\n", key, *file)
			if tightened {
				fmt.Fprintf(cmd.ErrOrStderr(), "tightened %s to 0600\n", *file)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clipboard, "clipboard", false, "read the value from the clipboard (wins over piped stdin)")
	return cmd
}

// resolveSetValue reads the value `set` will use when --clipboard wasn't
// given: piped stdin when the real stdin isn't a terminal, else an
// interactive no-echo prompt. The trailing-newline strip and empty-value
// refusal happen downstream, in the domain's shared set pipeline — this
// only sources the raw string.
func resolveSetValue(cmd *cobra.Command, key string) (string, error) {
	if !isTerminal() {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "Value for %s: ", key)
	value, err := readPassword()
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read value: %w", err)
	}
	return value, nil
}

// newEnvGetCmd builds `env get`.
func newEnvGetCmd(client *envpkg.Client, file *string, anyFile *bool) *cobra.Command {
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
			allow, err := resolveAllowAnyFile(*anyFile, *file)
			if err != nil {
				return err
			}
			if err := client.CopyValue(cmd.Context(), cwd, *file, key, allow); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "copied %s to clipboard\n", key)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clipboard, "clipboard", false, "copy the value to the clipboard (required)")
	return cmd
}

// newEnvCheckCmd builds `env check`.
func newEnvCheckCmd(file *string, anyFile *bool) *cobra.Command {
	var example string

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Report keys missing from/extra vs --example (default .env.example) — names only",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			allowFile, err := resolveAllowAnyFile(*anyFile, *file)
			if err != nil {
				return err
			}
			fileReal, fileExists, err := envpkg.Locate(*file, cwd, allowFile)
			if err != nil {
				return err
			}
			if !fileExists {
				return fmt.Errorf("%s not found", fileReal)
			}
			fileDoc, err := readDocument(fileReal)
			if err != nil {
				return err
			}

			allowExample, err := resolveAllowAnyFile(*anyFile, example)
			if err != nil {
				return err
			}
			exampleReal, exampleExists, err := envpkg.Locate(example, cwd, allowExample)
			if err != nil {
				return err
			}
			if !exampleExists {
				return fmt.Errorf("example file %s not found", exampleReal)
			}
			exampleDoc, err := readDocument(exampleReal)
			if err != nil {
				return err
			}

			missing, extra := envpkg.Diff(fileDoc, exampleDoc)
			out := cmd.OutOrStdout()
			// Only print a section that has names under it — a bare
			// "extra:" header with nothing beneath reads as a truncated
			// list rather than as "there are none".
			printSection(out, "missing:", missing)
			printSection(out, "extra:", extra)

			if len(missing) > 0 {
				return fmt.Errorf("%d missing key(s) in %s", len(missing), *file)
			}
			if len(extra) == 0 {
				// Clean: stdout stays empty so a caller can treat any
				// output as drift, and the reassurance goes to stderr.
				fmt.Fprintf(cmd.ErrOrStderr(), "%s matches %s\n", *file, example)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&example, "example", ".env.example", "path to the example file to check against")
	return cmd
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

// newEnvRedactCmd builds `env redact`.
func newEnvRedactCmd(file *string, anyFile *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "redact",
		Short: "Print --file with every value masked (****)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			allow, err := resolveAllowAnyFile(*anyFile, *file)
			if err != nil {
				return err
			}
			realPath, exists, err := envpkg.Locate(*file, cwd, allow)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%s not found", realPath)
			}
			doc, err := readDocument(realPath)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc.Redacted())
			return err
		},
	}
}
