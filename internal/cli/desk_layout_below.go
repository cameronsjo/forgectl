// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
)

// Geometry of `desk layout --below`. A split ratio is the share the ORIGINAL
// pane keeps, so the caller's row keeps deskBelowRatio of the tab's height and
// the desk gets the rest; with --progress the desk keeps deskBelowProgressRatio
// of the row's width.
const (
	deskBelowRatio         = 0.72
	deskBelowProgressRatio = 0.60
)

// belowPlan is the tab as `desk layout --below` found it: the caller's pane,
// and every other pane in the same tab, in `pane list` order.
type belowPlan struct {
	caller herdr.Pane
	others []herdr.Pane
}

func planBelow(ctx context.Context, client *herdr.Client, callerPaneID string) (belowPlan, error) {
	caller, err := client.PaneGet(ctx, callerPaneID)
	if err != nil {
		return belowPlan{}, fmt.Errorf("read this pane: %w", err)
	}
	panes, err := client.Panes(ctx)
	if err != nil {
		return belowPlan{}, fmt.Errorf("list the tab's panes: %w", err)
	}
	plan := belowPlan{caller: caller}
	for _, p := range panes {
		if p.TabID == caller.TabID && p.TerminalID != caller.TerminalID {
			plan.others = append(plan.others, p)
		}
	}
	return plan, nil
}

// evenKeep is the share the target pane keeps at step i (1-based) of building
// a row of n equal panes, where each step splits the pane the last step
// placed: the first leaves 1/n on the target, and so on down to 1/2.
func evenKeep(i, n int) float64 {
	return float64(int(100/float64(n-i+1)+0.5)) / 100
}

func runDeskLayoutBelow(cmd *cobra.Command, client *herdr.Client, run exec.SensitiveRunner, herdrPath, cwd, deskCommand, progress string, dryRun bool) error {
	ctx := cmd.Context()
	callerID, _ := deskLookupEnv("HERDR_PANE_ID")
	plan, err := planBelow(ctx, client, callerID)
	if err != nil {
		return fmt.Errorf("desk layout: %w", err)
	}
	if dryRun {
		return printBelowPlan(cmd, plan, deskCommand, progress)
	}

	// Park every other pane in one temporary tab, so this pane is alone and a
	// down split of it spans the whole tab.
	var parked []herdr.Pane
	var parkTab string
	restore := func() error {
		return restoreParked(ctx, client, run, herdrPath, plan.caller, plan.caller.TabID, parked)
	}
	for i, o := range plan.others {
		mv := herdr.PaneMove{NewTab: true}
		target := ""
		if i > 0 {
			mv = herdr.PaneMove{Tab: parkTab, Direction: herdr.SplitRight, Ratio: evenKeep(i, len(plan.others))}
			target = plan.others[i-1].TerminalID
		}
		moved, err := movePane(ctx, client, run, herdrPath, o.TerminalID, target, mv)
		if err != nil {
			return errors.Join(fmt.Errorf("desk layout: move a pane out of the tab: %w", err), restore())
		}
		if i == 0 {
			parkTab = moved.NewTab
		}
		parked = append(parked, o)
	}

	deskPane, err := herdr.PaneSplit(ctx, run, herdrPath, herdr.Split{Direction: herdr.SplitDown, Ratio: deskBelowRatio, CWD: cwd})
	if err != nil {
		return errors.Join(fmt.Errorf("desk layout: split off the desk pane: %w", err), restore())
	}
	if err := restore(); err != nil {
		return fmt.Errorf("desk layout: %w", err)
	}

	panes := []struct{ terminal, label, command string }{{deskPane.TerminalID, "desk", deskCommand}}
	if progress != "" {
		at, err := client.PaneByTerminal(ctx, deskPane.TerminalID)
		if err != nil {
			return fmt.Errorf("desk layout: %w", err)
		}
		prog, err := herdr.PaneSplit(ctx, run, herdrPath, herdr.Split{Pane: at.PaneID, Direction: herdr.SplitRight, Ratio: deskBelowProgressRatio, CWD: cwd})
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
	w.printf("moved=%d\n", len(plan.others))
	return w.err
}

// restoreParked moves the parked panes back into tab, in a row to the right
// of caller. It keeps going after a failure so one stuck pane does not strand
// the rest, and returns every failure.
func restoreParked(ctx context.Context, client *herdr.Client, run exec.SensitiveRunner, herdrPath string, caller herdr.Pane, tab string, parked []herdr.Pane) error {
	var errs []error
	target := caller.TerminalID
	for i, o := range parked {
		_, err := movePane(ctx, client, run, herdrPath, o.TerminalID, target,
			herdr.PaneMove{Tab: tab, Direction: herdr.SplitRight, Ratio: evenKeep(i+1, len(parked)+1)})
		if err != nil {
			errs = append(errs, fmt.Errorf("move a pane back into the tab (it is in its temporary tab): %w", err))
			continue
		}
		target = o.TerminalID
	}
	return errors.Join(errs...)
}

// movePane runs one `pane move`. herdr names panes only by id and ids
// renumber after a move, so the mover and the target pane are found again by
// terminal in one pane list, and each id is confirmed with a read, right
// before the call. A mismatch stops the move.
func movePane(ctx context.Context, client *herdr.Client, run exec.SensitiveRunner, herdrPath, mover, target string, mv herdr.PaneMove) (herdr.MovedPane, error) {
	panes, err := client.Panes(ctx)
	if err != nil {
		return herdr.MovedPane{}, err
	}
	find := func(terminal string) (string, error) {
		for _, p := range panes {
			if p.TerminalID != terminal {
				continue
			}
			got, err := client.PaneGet(ctx, p.PaneID)
			if err != nil {
				return "", fmt.Errorf("re-read pane %s: %w", p.PaneID, err)
			}
			if got.TerminalID != terminal {
				return "", fmt.Errorf("pane %s holds terminal %s, not %s; stopping", p.PaneID, safeLabel(got.TerminalID), safeLabel(terminal))
			}
			return p.PaneID, nil
		}
		return "", fmt.Errorf("no pane holds terminal %s any more", safeLabel(terminal))
	}
	if mv.Pane, err = find(mover); err != nil {
		return herdr.MovedPane{}, err
	}
	if target != "" {
		if mv.Target, err = find(target); err != nil {
			return herdr.MovedPane{}, err
		}
	}
	return herdr.PanePlace(ctx, run, herdrPath, mv)
}

// printBelowPlan is --dry-run for --below: one key=value line per herdr call,
// and nothing changed.
func printBelowPlan(cmd *cobra.Command, plan belowPlan, deskCommand, progress string) error {
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("dry-run: no pane is moved, split, renamed or started\n")
	n := len(plan.others)
	for i, o := range plan.others {
		if i == 0 {
			w.printf("move.out=%s to=new-tab\n", safeText(o.PaneID))
			continue
		}
		w.printf("move.out=%s to=parked-tab direction=right ratio=%.2f\n", safeText(o.PaneID), evenKeep(i, n))
	}
	w.printf("split=desk from=current direction=down ratio=%.2f\n", deskBelowRatio)
	for i, o := range plan.others {
		w.printf("move.back=%s to=this-tab direction=right ratio=%.2f\n", safeText(o.PaneID), evenKeep(i+1, n+1))
	}
	if progress != "" {
		w.printf("split=progress from=desk direction=right ratio=%.2f\n", deskBelowProgressRatio)
	}
	w.printf("rename=desk\n")
	if progress != "" {
		w.printf("rename=progress\n")
	}
	w.printf("run.desk=%s\n", safeText(deskCommand))
	if progress != "" {
		w.printf("run.progress=%s\n", safeText(progress))
	}
	w.printf("moved=%d\n", n)
	return w.err
}
