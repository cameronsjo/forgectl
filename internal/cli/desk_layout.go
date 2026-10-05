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

// selfBinary is the absolute path of the running forgectl, which the desk
// pane runs: the pane's own shell may find another build on its PATH, and the
// desk is the approval screen, so it must be this one. An upgrade replaces
// the binary at that path.
func selfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this forgectl binary: %w", err)
	}
	return exe, nil
}

func newDeskLayoutCmd(deps module.Deps, dir *string) *cobra.Command {
	var progress string
	var width int
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "layout",
		Short: "Split the herdr tab: this pane left, the desk right, CMD below it",
		Long: `layout splits the current herdr tab around this pane (where Claude runs): the
desk dashboard on the right, about --width columns wide, and, with --progress,
CMD in a pane below the desk. The new panes are named "desk" and "progress";
this pane keeps the focus.

CMD is typed into the progress pane's shell as it is, so quote it for a shell.
The desk pane runs this forgectl by its absolute path: <path> desk --dir <the
resolved desk directory>. herdr names panes only by id, and ids renumber, so
before each rename and run the pane is found again by its terminal and a read
confirms the id still holds it; a mismatch stops the layout.

It must run inside a herdr pane (HERDR_ENV=1, with HERDR_PANE_ID set).

--dry-run reads the tab's width and prints the plan (the splits and their
ratios, the renames, and the command each pane would run) without changing
anything.

Exit codes: 0 laid out (or planned, with --dry-run); 1 a herdr call failed (the panes made so far stay);
2 usage, or not in a herdr pane.`,
		Example: `  forgectl desk layout --progress 'claude-desk progress' --width 70`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if width < deskLayoutMinWidth || width > deskLayoutMaxWidth {
				return deskUsage("desk layout: --width must be between %d and %d", deskLayoutMinWidth, deskLayoutMaxWidth)
			}
			return runDeskLayout(cmd, deps, *dir, progress, width, dryRun)
		},
	}
	cmd.Flags().StringVar(&progress, "progress", "", "a shell command to run in a pane below the desk")
	cmd.Flags().IntVar(&width, "width", deskLayoutWidth, "about how many columns the desk's column gets")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the planned splits, renames and commands, and change nothing (it still reads the tab's width)")
	return cmd
}

// deskSplitRatio is the share the current pane keeps so the new right pane is
// about want columns of total, clamped so neither side gets squeezed.
func deskSplitRatio(total, want int) float64 {
	r := 1 - float64(want)/float64(total)
	r = math.Min(math.Max(r, deskLayoutMinRatio), deskLayoutMaxRatio)
	return math.Round(r*100) / 100
}

func runDeskLayout(cmd *cobra.Command, deps module.Deps, dirFlag, progress string, width int, dryRun bool) error {
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
	self, err := deskSelfBinary()
	if err != nil {
		return fmt.Errorf("desk layout: %w", err)
	}
	qbin, err := shellQuote(self)
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
	columns := int(math.Round(float64(layout.Area.Width) * (1 - ratio)))
	if dryRun {
		return printLayoutPlan(cmd, ratio, columns, layout.Area.Width, deskCommand, progress)
	}
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
	act := func(terminal string, fn func(ctx context.Context, paneID string) error) (string, error) {
		return actOnTerminal(ctx, client, terminal, fn)
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
	w.printf("columns=%d of %d\n", columns, layout.Area.Width)
	return w.err
}

// actOnTerminal runs fn against the pane that holds terminal. herdr can only
// target a pane by id, and ids renumber when a pane in the workspace closes,
// so the id is found again by terminal right before the call and then
// confirmed with a read of that id. A mismatch aborts before fn, naming
// both terminals.
//
// A window remains: a pane closing between the confirming read and fn's own
// herdr call can still renumber the id, and herdr has no terminal-addressed
// `pane run`. The read after fn reports that case instead of passing it by
// silently, though by then the command has been typed.
func actOnTerminal(ctx context.Context, client *herdr.Client, terminal string, fn func(ctx context.Context, paneID string) error) (string, error) {
	p, err := client.PaneByTerminal(ctx, terminal)
	if err != nil {
		return "", err
	}
	check := func(when string) error {
		got, err := client.PaneGet(ctx, p.PaneID)
		if err != nil {
			return fmt.Errorf("re-read pane %s %s: %w", p.PaneID, when, err)
		}
		if got.TerminalID != terminal {
			return fmt.Errorf("pane %s holds terminal %s %s, not %s; stopping", p.PaneID, safeLabel(got.TerminalID), when, safeLabel(terminal))
		}
		return nil
	}
	if err := check("before the call"); err != nil {
		return "", err
	}
	if err := fn(ctx, p.PaneID); err != nil {
		return p.PaneID, err
	}
	return p.PaneID, check("after the call (the command may have reached that terminal)")
}

// printLayoutPlan is --dry-run: what the layout would do, one key=value line
// per herdr call, and nothing changed. The commands are operator-built text,
// so they are printed terminal-safe.
func printLayoutPlan(cmd *cobra.Command, ratio float64, columns, total int, deskCommand, progress string) error {
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("dry-run: no pane is split, renamed or started\n")
	w.printf("split=desk from=current direction=right ratio=%.2f\n", ratio)
	if progress != "" {
		w.printf("split=progress from=desk direction=down ratio=%.2f\n", deskProgressRatio)
	}
	w.printf("rename=desk\n")
	if progress != "" {
		w.printf("rename=progress\n")
	}
	w.printf("run.desk=%s\n", safeText(deskCommand))
	if progress != "" {
		w.printf("run.progress=%s\n", safeText(progress))
	}
	w.printf("columns=%d of %d\n", columns, total)
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
