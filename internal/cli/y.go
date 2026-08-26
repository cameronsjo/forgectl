package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/history"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// yAliases is the single source of truth for y's c/p/l shorthands — migrated
// here from forgive.YAliases at conversion. A separate var (not a literal
// inside yModule) because newYCmdForClient also applies it: routing that read
// through yModule would be an initialization cycle (manifest → constructor →
// manifest).
var yAliases = map[string][]string{
	"copy":  {"c"},
	"paste": {"p"},
	"last":  {"l"},
}

// yLastOutputIsTerminal is the policy seam for history output. It inspects the
// writer Cobra will actually use, rather than assuming process stdout, because
// a parent command or an embedding caller can replace that sink. Tests replace
// this function so the interactive and redirected paths need no real terminal.
var yLastOutputIsTerminal = func(out io.Writer) bool {
	fdWriter, ok := out.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fdWriter.Fd()))
}

// yModule declares the y (clipboard) extension (ADR-0005) — the conversion
// template for SubAliases modules.
var yModule = module.Manifest{
	Name:       "y",
	Tier:       module.TierExtension,
	SubAliases: yAliases,
	New:        newYCmd,
}

// newYCmd builds `forgectl y` over the registry Deps — both halves of issue
// #26: the clipboard verbs over pbcopy/pbpaste, and read-only shell-history
// recall over $HISTFILE.
func newYCmd(deps module.Deps) *cobra.Command {
	client := clippkg.New(deps.Runner)
	return newYCmdForClient(client)
}

// newYCmdForClient builds the command over an already-constructed client —
// split out so tests can inject a fake-wired *clip.Client (mirrors
// newDockerCmdForClient) without going through newYCmd.
func newYCmdForClient(client *clippkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "y",
		Short: "Clipboard and shell-history helpers",
		Long: `y wraps pbcopy/pbpaste so a shell pipeline can move text through the
clipboard without shelling out directly, and reads back recent commands from
the zsh history file. Clipboard verbs are macOS only.

  echo hi | forgectl y copy   copy stdin to the clipboard
  forgectl y paste            print the clipboard's current contents
  forgectl y file ./a.pdf     put a file reference on the clipboard (attaches, not text)
  forgectl y img ./a.png      put decoded image data on the clipboard (pastes as a picture)
  forgectl y last 5           print the 5 most recent shell commands`,
	}
	cmd.AddCommand(
		newYCopyCmd(client),
		newYPasteCmd(client),
		newYFileCmd(client),
		newYImgCmd(client),
		newYLastCmd(),
	)
	applyAliases(cmd, yAliases)
	return cmd
}

// newYCopyCmd builds `y copy`.
func newYCopyCmd(client *clippkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "copy",
		Short: "Copy stdin to the clipboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			return client.Copy(cmd.Context(), string(data))
		},
	}
}

// newYPasteCmd builds `y paste`.
func newYPasteCmd(client *clippkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "paste",
		Short: "Print the clipboard's current contents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := client.Paste(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
}

// newYFileCmd builds `y file <path>`. Unlike copy/paste, it takes a path
// rather than stdin — that's what lets it reach a pasteboard type
// (public.file-url) a text pipe cannot express (issue #401).
func newYFileCmd(client *clippkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "file <path>",
		Short: "Put a file reference on the clipboard",
		Long: `file puts a POSIX file reference for path on the clipboard, so pasting into
Finder, Mail, or a chat window attaches the file rather than dumping its path
as text. macOS only.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveYPath(args[0])
			if err != nil {
				return err
			}
			return client.CopyFile(cmd.Context(), path)
		},
	}
}

// newYImgCmd builds `y img <path>`. Same rationale as newYFileCmd: a path
// argument reaches decoded image data (public.png/tiff/jpeg/gif), which a
// text pipe cannot carry.
func newYImgCmd(client *clippkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "img <path>",
		Short: "Put decoded image data on the clipboard",
		Long: `img decodes the image at path and puts it on the clipboard as image data, so
pasting into Finder, Mail, or a chat window pastes a picture rather than a
filename. Supports .png, .tif/.tiff, .jpg/.jpeg, .gif. macOS only.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveYPath(args[0])
			if err != nil {
				return err
			}
			return client.CopyImage(cmd.Context(), path)
		},
	}
}

// resolveYPath checks path exists and is a regular file before it ever
// reaches osascript, so a typo surfaces as a clear "no such file" instead of
// a confusing osascript failure three layers down. Returns the absolute
// path: osascript's `POSIX file` resolves relative paths against its own
// process cwd, not the caller's, so a relative path would silently break.
func resolveYPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", termsafe.SafeLine(path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s: is a directory, not a file", termsafe.SafeLine(path))
	}
	return abs, nil
}

// newYLastCmd builds `y last [n]`. Everything it prints comes from the history
// file, which is untrusted, so every command goes through termsafe.SafeLine —
// one entry always renders as exactly one inert physical line.
func newYLastCmd() *cobra.Command {
	var allowSensitiveOutput bool

	cmd := &cobra.Command{
		Use:   "last [n]",
		Short: "Print the n most recent shell commands",
		Long: `last reads $HISTFILE (default ~/.zsh_history), parses zsh's history
format, and prints the n most recent commands, oldest first. n defaults to 1.

It is read-only: forgectl installs no shell function and no hook, so it sees
only what zsh has already flushed to disk. zsh buffers history until the shell
exits unless INC_APPEND_HISTORY or SHARE_HISTORY is set in .zshrc — without one
of those, commands typed in the current shell are not on disk yet and will not
appear here.

Only zsh history is supported; bash stores history differently.

Commands are printed with control characters escaped, so the output is safe to
read but is not a faithful copy of what was typed.

Shell history routinely contains inline secrets — an exported token, a bearer
header, a password passed as a flag. last prints those values verbatim, so it
writes to an interactive terminal only. Piping or redirecting stdout requires
--allow-sensitive-output, which acknowledges the wider audience; it does not
scan, classify, or redact the output.`,
		// SilenceUsage/SilenceErrors mirror `env get` (env.go): last's stdout is
		// a stream of recovered commands, so a refusal must not dump usage text
		// into it — a caller reading the output would take the help banner for
		// history.
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if !allowSensitiveOutput && !yLastOutputIsTerminal(out) {
				return errors.New("shell history output is sensitive; refusing non-terminal stdout without --allow-sensitive-output")
			}

			count := 1
			if len(args) == 1 {
				parsed, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("count %q is not a number", termsafe.SafeLine(args[0]))
				}
				count = parsed
			}

			path, err := history.ResolvePath(os.Getenv, os.UserHomeDir)
			if err != nil {
				return err
			}
			entries, err := history.Read(path)
			if err != nil {
				return err
			}
			tail, err := history.LastN(entries, count)
			if err != nil {
				return err
			}

			// A dropped write would silently shorten the list, and a short
			// list of commands reads exactly like a short history — so a
			// write failure is surfaced rather than swallowed.
			for _, entry := range tail {
				if _, err := fmt.Fprintln(out, termsafe.SafeLine(entry.Command)); err != nil {
					return fmt.Errorf("write shell history: %w", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowSensitiveOutput, "allow-sensitive-output", false,
		"allow verbatim history on non-terminal stdout (no secret scanning or redaction)")
	return cmd
}
