// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

	// SIGINT or SIGTERM cancels this context, and the restore below runs on a
	// context a cancel cannot reach. The handler is released at the first
	// signal, so a second Ctrl-C during a stuck restore ends the process
	// instead of being swallowed. SIGHUP is left alone: it arrives when the
	// pane closes, and the restore targets this pane's terminal, which is gone
	// by then.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	// Park every other pane in one temporary tab, so this pane is alone and a
	// down split of it spans the whole tab. A pane is noted before its move,
	// not after: herdr can carry out a move and still answer with something
	// unreadable, so a pane whose move reported an error may be parked anyway.
	// restoreParked looks at where each one is before moving it back.
	var attempted []herdr.Pane
	var parkTab string
	restore := func() error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
		defer cancel()
		return restoreParked(rctx, client, run, herdrPath, plan.caller, attempted)
	}
	for i, o := range plan.others {
		if err := ctx.Err(); err != nil {
			return errors.Join(fmt.Errorf("desk layout: stopped: %w", err), restore())
		}
		mv := herdr.PaneMove{NewTab: true}
		target := ""
		if i > 0 {
			mv = herdr.PaneMove{Tab: parkTab, Direction: herdr.SplitRight, Ratio: evenKeep(i, len(plan.others))}
			target = plan.others[i-1].TerminalID
		}
		attempted = append(attempted, o)
		moved, err := movePane(ctx, client, run, herdrPath, o.TerminalID, target, mv)
		if err != nil {
			return errors.Join(fmt.Errorf("desk layout: move a pane out of the tab: %w", err), restore())
		}
		if i == 0 {
			parkTab = moved.NewTab
		}
	}

	if err := ctx.Err(); err != nil {
		return errors.Join(fmt.Errorf("desk layout: stopped: %w", err), restore())
	}
	// Not --current: herdr reads HERDR_PANE_ID, fixed when this process
	// started, and pane ids renumber after the park moves. Find this pane
	// again by its terminal, as every other call does.
	callerNow, err := paneByTerminalChecked(ctx, client, plan.caller.TerminalID)
	if err != nil {
		return errors.Join(fmt.Errorf("desk layout: find this pane again: %w", err), restore())
	}
	deskPane, err := herdr.PaneSplit(ctx, run, herdrPath, herdr.Split{Pane: callerNow, Direction: herdr.SplitDown, Ratio: deskBelowRatio, CWD: cwd})
	if err != nil {
		return errors.Join(fmt.Errorf("desk layout: split off the desk pane (herdr may have made it before the error; look for an unnamed pane): %w", err), restore())
	}
	if err := restore(); err != nil {
		return fmt.Errorf("desk layout: the desk pane (terminal %s) was made and stays: %w", safeLabel(deskPane.TerminalID), err)
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

// restoreTimeout bounds the restore, which runs on a context that cannot be
// cancelled.
const restoreTimeout = 30 * time.Second

// restoreParked moves back, to the right of caller in the caller's tab, every
// pane of attempted that is not in that tab now, in equal widths. It reads
// where the panes are instead of trusting what the moves reported. It keeps
// going after a failure so one stuck pane does not strand the rest, and
// returns every failure, naming each pane that stays in its temporary tab.
func restoreParked(ctx context.Context, client *herdr.Client, run exec.SensitiveRunner, herdrPath string, caller herdr.Pane, attempted []herdr.Pane) error {
	if len(attempted) == 0 {
		return nil
	}
	now, err := client.Panes(ctx)
	if err != nil {
		return fmt.Errorf("find the moved panes to put back: %w (panes of this tab may be in a temporary tab: %s)", err, terminalList(attempted))
	}
	var strays []herdr.Pane
	for _, a := range attempted {
		for _, p := range now {
			if p.TerminalID == a.TerminalID && p.TabID != caller.TabID {
				strays = append(strays, p)
			}
		}
	}
	var errs []error
	target := caller.TerminalID
	placed, failed := 0, 0
	for _, o := range strays {
		_, err := movePane(ctx, client, run, herdrPath, o.TerminalID, target,
			herdr.PaneMove{Tab: caller.TabID, Direction: herdr.SplitRight, Ratio: evenKeep(placed+1, len(strays)-failed+1)})
		if err != nil {
			errs = append(errs, fmt.Errorf("put the pane with terminal %s back into the tab (it stays in its temporary tab): %w", safeLabel(o.TerminalID), err))
			failed++
			continue
		}
		target = o.TerminalID
		placed++
	}
	return errors.Join(errs...)
}

func terminalList(ps []herdr.Pane) string {
	var ids []string
	for _, p := range ps {
		ids = append(ids, safeLabel(p.TerminalID))
	}
	return strings.Join(ids, ", ")
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
			if p.TerminalID == terminal {
				return confirmPane(ctx, client, p.PaneID, terminal)
			}
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

// confirmPane reads paneID back and returns it only if it still holds terminal.
func confirmPane(ctx context.Context, client *herdr.Client, paneID, terminal string) (string, error) {
	got, err := client.PaneGet(ctx, paneID)
	if err != nil {
		return "", fmt.Errorf("re-read pane %s: %w", paneID, err)
	}
	if got.TerminalID != terminal {
		return "", fmt.Errorf("pane %s holds terminal %s, not %s; stopping", paneID, safeLabel(got.TerminalID), safeLabel(terminal))
	}
	return paneID, nil
}

// paneByTerminalChecked finds the pane that holds terminal now and confirms
// the id with a read.
func paneByTerminalChecked(ctx context.Context, client *herdr.Client, terminal string) (string, error) {
	p, err := client.PaneByTerminal(ctx, terminal)
	if err != nil {
		return "", err
	}
	return confirmPane(ctx, client, p.PaneID, terminal)
}

// printBelowPlan is --dry-run for --below: one key=value line per herdr call,
// and nothing changed.
func printBelowPlan(cmd *cobra.Command, plan belowPlan, deskCommand, progress string) error {
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("dry-run: no pane is moved, split, renamed or started (pane ids renumber after each move; the real run finds each pane again by its terminal)\n")
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
