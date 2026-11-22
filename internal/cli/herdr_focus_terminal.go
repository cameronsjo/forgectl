package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/notify"
)

// focusTerminalTimeout bounds the whole click: the ls and both selects.
const focusTerminalTimeout = 5 * time.Second

// newHerdrFocusTerminalCmd is the command a macOS banner's click runs
// (internal/notify builds it): show the herdr-app space and tab that hold one
// terminal. Hidden, because nobody types it: it runs from terminal-notifier,
// with launchd's bare PATH, so herdr-app arrives as an absolute path.
//
// The tab is found at click time, not when the banner was posted: a tab
// moved or renamed since still resolves, and posting stays one exec. herdr
// has no focus by terminal or pane, so the tab is the finest grain.
func newHerdrFocusTerminalCmd(deps module.Deps) *cobra.Command {
	var terminal, herdrApp string
	cmd := &cobra.Command{
		Use:    "_focus-terminal --terminal <id> --herdr-app <path>",
		Short:  "Show the herdr-app tab that holds a terminal (a notification click runs this)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFocusTerminalArgs(terminal, herdrApp); err != nil {
				return WithExitCode(err, exitUsage)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), focusTerminalTimeout)
			defer cancel()
			return focusTerminal(ctx, herdr.NewAppAt(deps.Runner, herdrApp), terminal)
		},
	}
	cmd.Flags().StringVar(&terminal, "terminal", "", "the herdr-app terminal (HC_TERMINAL_ID) whose tab to show")
	cmd.Flags().StringVar(&herdrApp, "herdr-app", "", "absolute path of the herdr-app executable")
	return cmd
}

// checkFocusTerminalArgs refuses a terminal id the notifier would not have
// built and any herdr-app path but an absolute one to a file named
// herdr-app, so the command runs nothing but herdr-app.
func checkFocusTerminalArgs(terminal, herdrApp string) error {
	if !notify.ValidID(terminal) {
		return fmt.Errorf("--terminal %q is not a terminal id (letters, digits, - and _, at most 64)", terminal)
	}
	if !filepath.IsAbs(herdrApp) || filepath.Base(herdrApp) != herdr.AppBinary {
		return fmt.Errorf("--herdr-app %q is not an absolute path to %s", herdrApp, herdr.AppBinary)
	}
	return nil
}

// terminalFocuser is the part of herdr.AppClient a click drives.
type terminalFocuser interface {
	LocateTerminal(ctx context.Context, terminal string) (spaceID, tabID string, err error)
	FocusWorkspace(ctx context.Context, workspaceID string) error
	FocusTab(ctx context.Context, tabID string) error
}

// focusTerminal selects the terminal's space, then its tab: herdr-app's
// tab.select is not documented to switch spaces (AppClient.FocusNeedsSpace).
func focusTerminal(ctx context.Context, app terminalFocuser, terminal string) error {
	space, tab, err := app.LocateTerminal(ctx, terminal)
	if err != nil {
		slog.Warn("Notification click found no tab for its terminal.", "terminal", terminal, "error", err)
		return err
	}
	err = errors.Join(app.FocusWorkspace(ctx, space), app.FocusTab(ctx, tab))
	if err != nil {
		slog.Warn("Notification click could not show the terminal's tab.", "terminal", terminal, "space", space, "tab", tab, "error", err)
		return err
	}
	slog.Info("Notification click showed the terminal's tab.", "terminal", terminal, "space", space, "tab", tab)
	return nil
}
