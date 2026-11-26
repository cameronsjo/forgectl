// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package tmuxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"

	"charm.land/huh/v2"
	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

var tmuxAliases = map[string][]string{
	"ls":      {"l", "list", "sessions"},
	"pick":    {"p", "go", "n", "new"},
	"kill":    {"k", "rm", "delete", "x"},
	"rename":  {"mv", "rn"},
	"windows": {"w"},
	"tree":    {"t"},
	"last":    {"-"},
	"cheat":   {"keys"},
}

var (
	errTmuxMenuNeedsTerminal = errors.New("the tmux menu needs a terminal on stdin and stdout; use forgectl tmux ls or forgectl tmux tree for plain output")
	errTmuxMenuDumbTerminal  = tui.DumbTerminalError("the tmux menu", "forgectl tmux ls or forgectl tmux tree for plain output")
)

const exitCancelled = 130

var userCancelled atomic.Bool

func noteCancelled(w io.Writer) error {
	_, _ = fmt.Fprintln(w, "cancelled")
	userCancelled.Store(true)
	return nil
}

func confirm(th theme.Theme, prompt string) (bool, error) {
	ok := false
	err := keymap.Suspendable(huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(prompt).
			Description("esc to cancel").
			Affirmative("Yes").
			Negative("No").
			Value(&ok),
	))).WithKeyMap(keymap.Cancel()).WithShowHelp(true).WithTheme(th.Huh()).Run()
	return ok, err
}

func isInteractiveTTY() bool {
	in, err := os.Stdin.Stat()
	if err != nil || in.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	out, err := os.Stdout.Stat()
	if err != nil || out.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return true
}

func termIsDumb() bool {
	return os.Getenv("TERM") == "dumb"
}

// Execute is the entrypoint for the standalone forgectl-tmux plugin binary.
func Execute(ctx context.Context) error {
	userCancelled.Store(false)
	env, err := config.CaptureEnvSnapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		return WithExitCode(err, 2)
	}
	legacyBoundary, err := config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
	if err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		return WithExitCode(err, 1)
	}
	defer legacyBoundary.Close() //nolint:errcheck

	var cfg config.Config
	if !errors.Is(legacyBoundary.Refusal, config.ErrLegacyPathControl) {
		cfg = config.LoadPath(legacyBoundary.ConfigPath)
	}
	closer := config.SetupLogger(cfg)
	defer closer.Close() //nolint:errcheck

	th := theme.Default()
	if opts, terr := theme.FromConfig(cfg.Theme); terr == nil {
		th = theme.New(opts, true)
	}
	deps := module.Deps{
		Cfg:            cfg,
		Runner:         exec.OSRunner{},
		Theme:          th,
		LegacyBoundary: legacyBoundary,
	}

	root := newTmuxCmd(deps, tmux.New(deps.Runner))
	root.Version = meta.Version
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.PersistentFlags().Bool("no-icons", cfg.NoIcons, "use ASCII markers instead of Nerd Font glyphs")

	args := normalizeTmuxArgs(os.Args[1:])
	root.SetArgs(args)

	execErr := fang.Execute(
		ctx,
		root,
		fang.WithVersion(meta.Version),
		fang.WithErrorHandler(tmuxErrorHandler),
	)
	if execErr == nil && userCancelled.Load() {
		return &silentCodedError{code: exitCancelled}
	}
	return execErr
}

func newTmuxCmd(deps module.Deps, client *tmux.Client) *cobra.Command {
	th := deps.Theme
	cmd := &cobra.Command{
		Use:     "tmux",
		Aliases: []string{"tm"},
		Short:   "Wrangle tmux sessions, windows, and panes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isInteractiveTTY() {
				return errTmuxMenuNeedsTerminal
			}
			if termIsDumb() {
				return errTmuxMenuDumbTerminal
			}
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			act, err := tui.Run(cmd.Context(), client, tui.RunOptions{
				StartInTmux: true,
				NoIcons:     noIcons,
				Theme:       th,
			})
			if err != nil {
				return err
			}
			return dispatchAction(cmd.Context(), client, act)
		},
	}
	cmd.AddCommand(
		newTmuxLsCmd(client),
		newTmuxPickCmd(client),
		newTmuxKillCmd(client, th),
		newTmuxRenameCmd(client),
		newTmuxWindowsCmd(client),
		newTmuxTreeCmd(client),
		newTmuxLastCmd(client),
		newTmuxCheatCmd(th),
	)
	applyAliases(cmd, tmuxAliases)
	return cmd
}

func dispatchAction(ctx context.Context, client *tmux.Client, act tui.Action) error {
	switch act.Kind {
	case tui.ActionAttachSession:
		return client.AttachSession(ctx, act.Session)
	case tui.ActionAttachWindow:
		return client.AttachWindow(ctx, act.Window)
	case tui.ActionPick:
		return seshPick(ctx, client, act.Pick)
	case tui.ActionLast:
		return client.LastSession(ctx)
	}
	return nil
}

func applyAliases(parent *cobra.Command, aliases map[string][]string) {
	for _, sub := range parent.Commands() {
		list, ok := aliases[sub.Name()]
		if !ok {
			continue
		}
		var usable []string
		for _, a := range list {
			if a == "-" {
				continue
			}
			usable = append(usable, a)
		}
		sub.Aliases = usable
	}
}

func normalizeTmuxArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	res := forgive.NewResolver(tmuxAliases)
	for i, tok := range out {
		if tok == "--" {
			break
		}
		if tok != "-" && len(tok) > 0 && tok[0] == '-' {
			continue
		}
		if canon, known := res.Canonical(tok); known {
			out[i] = canon
		}
		break
	}
	return out
}

func tmuxErrorHandler(w io.Writer, styles fang.Styles, err error) {
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return
	}
	fang.DefaultErrorHandler(w, styles, termsafe.Error(err))
}

const (
	labelMaxRunes = 64
	titleMaxRunes = 256
	textMaxRunes  = 1280
)

func safeTitle(s string) string {
	return termsafe.SafeLineMax(s, titleMaxRunes)
}

func safeText(s string) string {
	return termsafe.SafeLineMax(s, textMaxRunes)
}

func safeColumnPath(s string) string {
	return termsafe.SafePathMax(s, termsafe.PathEchoMaxRunes)
}

type codedError struct {
	err  error
	code int
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

// WithExitCode wraps err so ExitCode(err) reports code instead of 1.
func WithExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return &codedError{err: err, code: code}
}

type silentCodedError struct {
	code int
}

func (e *silentCodedError) Error() string { return "" }
func (e *silentCodedError) ExitCode() int { return e.code }

// ExitCode walks err's chain for a codedError or silentCodedError and returns
// its code, or 1 when err carries none.
func ExitCode(err error) int {
	var coded *codedError
	if chainAs(err, &coded) {
		return coded.ExitCode()
	}
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return silent.ExitCode()
	}
	return 1
}

func chainAs[T any](err error, target *T) (found bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Error chain walk panicked; treating the target as absent.",
				"error_type", fmt.Sprintf("%T", err), "target_type", fmt.Sprintf("%T", target), "panic_type", fmt.Sprintf("%T", r))
			found = false
		}
	}()
	return errors.As(err, target)
}
