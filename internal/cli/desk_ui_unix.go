//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tui"
)

var errDeskUIDumbTerminal = tui.DumbTerminalError("desk ui", "desk ui --frame for one frame, or desk asks")

func newDeskUICmd(dir *string, deps module.Deps) *cobra.Command {
	var frame bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Answer the asks agents left you, from the keyboard",
		Long: `ui shows everything on the desk that needs you: open asks grouped Now / When
you have 10 minutes / Optional / Coming, snoozed asks, the desk's waiting
scripts (read-only, with their sha256), and the last few answered asks. It
re-reads the asks log every second.

Keys: d done (press d or y again to confirm), r reply, l link (http or https),
s snooze (1 one hour, 2 four hours, 3 until 9:00 tomorrow, 4 one week),
enter shows or hides the details, ? help, q quit. An ask offers only the
responses its agent allowed. Each response is appended to the asks log and,
when the ask names a terminal, sent there with hc.

Scripts are not run here; run or skip them from the dashboard (forgectl desk).`,
		Example: `  forgectl desk ui
  COLUMNS=100 LINES=30 forgectl desk ui --frame`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeskUI(cmd, deps, *dir, frame)
		},
	}
	cmd.Flags().BoolVar(&frame, "frame", false, "print one frame to stdout and exit; size from $COLUMNS and $LINES (default 80x40)")
	return cmd
}

func runDeskUI(cmd *cobra.Command, deps module.Deps, dirFlag string, frame bool) error {
	if !frame && !deskHasTerminal() {
		return WithExitCode(tui.ErrDeskNeedsTerminal, deskExitUsage)
	}
	if !frame && termIsDumb() {
		return WithExitCode(errDeskUIDumbTerminal, deskExitUsage)
	}
	home, _ := deskUserHome() // display only
	if frame {
		// --frame only reads: it creates no asks directory.
		dir, err := resolveDeskDir(dirFlag)
		if err != nil {
			return err
		}
		s, err := ask.OpenReadOnly(dir, ask.WithClock(deskNow))
		if err != nil {
			return err
		}
		opts := deskUIOptions(s, dir, home, deps)
		out, err := tui.RenderDeskAsksFrame(cmd.Context(), opts, envSize("COLUMNS", deskFrameWidth), envSize("LINES", deskFrameHeight))
		if err != nil {
			return err
		}
		_, err = io.WriteString(deps.Theme.Writer(cmd.OutOrStdout(), os.Environ()), out+"\n")
		return err
	}
	s, dir, err := openAskStore(dirFlag)
	if err != nil {
		return err
	}
	return tui.RunDeskAsks(cmd.Context(), deskUIOptions(s, dir, home, deps))
}

func deskUIOptions(s *ask.Store, dir, home string, deps module.Deps) tui.DeskAsksOptions {
	return tui.DeskAsksOptions{
		Store:   s,
		Scripts: func() ([]tui.DeskAskScript, error) { return deskScripts(dir) },
		Theme:   deps.Theme,
		Now:     deskNow,
		Dir:     tui.TildePath(dir, home),
	}
}

// deskScripts lists the desk's waiting, held and running scripts through a
// read-only open, so the view never fixes a hash or moves an item. A
// directory that is not a desk yet has no scripts.
func deskScripts(dir string) ([]tui.DeskAskScript, error) {
	d, err := desk.OpenReadOnly(dir)
	if errors.Is(err, desk.ErrNoDesk) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the desk: %w", err)
	}
	defer d.Close() //nolint:errcheck // read-only
	snap, err := d.Scan()
	if err != nil {
		return nil, fmt.Errorf("scan the desk: %w", err)
	}
	var out []tui.DeskAskScript
	for _, group := range [][]desk.Item{snap.Pending, snap.Running} {
		for _, it := range group {
			sha := it.Meta.SHA256
			if !desk.ValidSHA256(sha) {
				sha = ""
			}
			out = append(out, tui.DeskAskScript{Name: it.Name, State: string(it.State), What: it.What, SHA256: sha})
		}
	}
	return out, nil
}
