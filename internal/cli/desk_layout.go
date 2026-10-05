// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
)

// Layout geometry. herdr's split ratio is the share the ORIGINAL pane keeps
// (measured: 0.3 left 53 of 175 columns), so the desk's ratio is computed
// from the tab's width and clamped, and the progress pane takes the lower
// 60% of the desk's column.
const (
	deskLayoutWidth    = 66
	deskLayoutMinRatio = 0.45
	deskLayoutMaxRatio = 0.8
	deskProgressRatio  = 0.40
	deskLayoutMinWidth = 20
	deskLayoutMaxWidth = 500
)

// Seams for the layout: where herdr is, whether this process runs in a herdr
// pane, its cwd, and the binary the desk pane should run.
var (
	deskLookupEnv  = os.LookupEnv
	deskHerdrPath  = func() (string, error) { return lookAbs(herdr.Binary) }
	deskGetwd      = os.Getwd
	deskSelfBinary = selfBinary
)

func lookAbs(name string) (string, error) {
	p, err := osexec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// selfBinary is how the desk pane names forgectl: "forgectl" when that is
// what PATH finds (so an upgrade is picked up on the next run), else this
// binary's absolute path.
func selfBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "forgectl"
	}
	if onPath, err := osexec.LookPath("forgectl"); err == nil {
		a, errA := filepath.EvalSymlinks(onPath)
		b, errB := filepath.EvalSymlinks(exe)
		if errA == nil && errB == nil && a == b {
			return "forgectl"
		}
	}
	return exe
}

func newDeskLayoutCmd(deps module.Deps, dir *string) *cobra.Command {
	var progress string
	var width int
	cmd := &cobra.Command{
		Use:   "layout",
		Short: "Split the herdr tab: this pane left, the desk right, CMD below it",
		Long: `layout splits the current herdr tab around this pane (where Claude runs): the
desk dashboard on the right, about --width columns wide, and, with --progress,
CMD in a pane below the desk. The new panes are named "desk" and "progress";
this pane keeps the focus.

CMD is typed into the progress pane's shell as it is, so quote it for a shell.
The desk pane runs forgectl desk --dir <the resolved desk directory>.

It must run inside a herdr pane (HERDR_ENV=1, with HERDR_PANE_ID set).

Exit codes: 0 laid out; 1 a herdr call failed (the panes made so far stay);
2 usage, or not in a herdr pane.`,
		Example: `  forgectl desk layout --progress 'claude-desk progress' --width 70`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if width < deskLayoutMinWidth || width > deskLayoutMaxWidth {
				return deskUsage("desk layout: --width must be between %d and %d", deskLayoutMinWidth, deskLayoutMaxWidth)
			}
			return runDeskLayout(cmd, deps, *dir, progress, width)
		},
	}
	cmd.Flags().StringVar(&progress, "progress", "", "a shell command to run in a pane below the desk")
	cmd.Flags().IntVar(&width, "width", deskLayoutWidth, "about how many columns the desk's column gets")
	return cmd
}

// deskSplitRatio is the share the current pane keeps so the new right pane is
// about want columns of total, clamped so neither side gets squeezed.
func deskSplitRatio(total, want int) float64 {
	r := 1 - float64(want)/float64(total)
	r = math.Min(math.Max(r, deskLayoutMinRatio), deskLayoutMaxRatio)
	return math.Round(r*100) / 100
}

func runDeskLayout(cmd *cobra.Command, deps module.Deps, dirFlag, progress string, width int) error {
	if !deskSupported {
		return errDeskUnsupported()
	}
	if err := herdr.CheckSession(deskLookupEnv); err != nil {
		return deskUsage("desk layout must run inside a herdr pane: %w", err)
	}
	if v, ok := deskLookupEnv("HERDR_PANE_ID"); !ok || v == "" {
		return deskUsage("desk layout must run inside a herdr pane: HERDR_PANE_ID is not set")
	}
	if progress != "" {
		if err := herdr.CheckRunCommand(progress); err != nil {
			return deskUsage("desk layout: --progress: %w", err)
		}
	}
	dir, err := resolveDeskDir(dirFlag)
	if err != nil {
		return err
	}
	qdir, err := shellQuote(dir)
	if err != nil {
		return deskUsage("desk layout: the desk directory %w", err)
	}
	qbin, err := shellQuote(deskSelfBinary())
	if err != nil {
		return fmt.Errorf("desk layout: the forgectl path %w", err)
	}
	deskCommand := qbin + " desk --dir " + qdir
	herdrPath, err := deskHerdrPath()
	if err != nil {
		return fmt.Errorf("desk layout: find herdr: %w", err)
	}
	cwd, err := deskGetwd()
	if err != nil {
		return fmt.Errorf("desk layout: %w", err)
	}

	ctx := cmd.Context()
	client := herdr.New(deps.Runner)
	run := deps.SensitiveRunner
	layout, err := client.CurrentLayout(ctx)
	if err != nil {
		return fmt.Errorf("desk layout: read the tab's width: %w", err)
	}
	ratio := deskSplitRatio(layout.Area.Width, width)
	deskPane, err := herdr.PaneSplit(ctx, run, herdrPath, herdr.Split{Direction: herdr.SplitRight, Ratio: ratio, CWD: cwd})
	if err != nil {
		return fmt.Errorf("desk layout: split off the desk pane: %w", err)
	}
	panes := []struct{ terminal, label, command string }{{deskPane.TerminalID, "desk", deskCommand}}
	if progress != "" {
		at, err := client.PaneByTerminal(ctx, deskPane.TerminalID)
		if err != nil {
			return fmt.Errorf("desk layout: %w", err)
		}
		prog, err := herdr.PaneSplit(ctx, run, herdrPath, herdr.Split{Pane: at.PaneID, Direction: herdr.SplitDown, Ratio: deskProgressRatio, CWD: cwd})
		if err != nil {
			return fmt.Errorf("desk layout: split off the progress pane: %w", err)
		}
		panes = append(panes, struct{ terminal, label, command string }{prog.TerminalID, "progress", progress})
	}
	// Ids renumber, so each pane is found again by terminal right before
	// each call that names it.
	act := func(terminal string, fn func(ctx context.Context, paneID string) error) (string, error) {
		p, err := client.PaneByTerminal(ctx, terminal)
		if err != nil {
			return "", err
		}
		return p.PaneID, fn(ctx, p.PaneID)
	}
	for _, p := range panes {
		if _, err := act(p.terminal, func(ctx context.Context, id string) error {
			return herdr.PaneRename(ctx, run, herdrPath, id, p.label)
		}); err != nil {
			return fmt.Errorf("desk layout: name the %s pane: %w", p.label, err)
		}
	}
	w := &stickyWriter{w: cmd.OutOrStdout()}
	for _, p := range panes {
		id, err := act(p.terminal, func(ctx context.Context, id string) error {
			return herdr.PaneRun(ctx, run, herdrPath, id, p.command)
		})
		if err != nil {
			return fmt.Errorf("desk layout: start the %s pane: %w", p.label, err)
		}
		w.printf("%s=%s\n", p.label, id)
	}
	w.printf("columns=%d of %d\n", int(math.Round(float64(layout.Area.Width)*(1-ratio))), layout.Area.Width)
	return w.err
}

// shellWordRe is a word a POSIX shell reads as itself, unquoted.
var shellWordRe = regexp.MustCompile(`^[A-Za-z0-9_./][A-Za-z0-9_./:=+-]*$`)

// shellQuote renders s as one POSIX shell word for a command line typed into
// a pane: as it is when it needs no quoting, else single-quoted. Text that
// would break that line (a newline, a control character, invalid UTF-8) is
// refused rather than quoted.
func shellQuote(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("is not valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return "", errors.New("contains a control character")
		}
	}
	if shellWordRe.MatchString(s) {
		return s, nil
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}
