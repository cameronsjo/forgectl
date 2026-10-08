package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/herdr/organize"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Seams for --apply: the fork check, the lock, and where the lock file lives.
var (
	herdrCheckFork = herdr.CheckFork
	herdrWithLock  = config.WithFileLockNotify
	herdrLockPath  = organizeLockPath
)

const lockWaitNotice = "waiting for another forgectl herdr organize (Ctrl-C to cancel)"

// organizeLockPath is the lock's base path, <config dir>/herdr-organize (the
// lock file is that plus ".lock"). It is dedicated so it never contends with
// the writers that lock config.toml itself.
func organizeLockPath() (string, error) {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config directory %s: %w", termsafe.QuotePath(dir), termsafe.Error(err))
	}
	return filepath.Join(dir, "herdr-organize"), nil
}

// forkRefusal words a failed fork check. A missing `tab move` is the fork
// requirement; anything else means the probe could not run herdr at all.
func forkRefusal(err error) error {
	if errors.Is(err, herdr.ErrForkRequired) {
		return errors.New(`herdr has no "tab move", which organize --apply needs (it is in the cameronsjo/herdr fork, not upstream herdr); see docs/commands/herdr.md#requirements`)
	}
	return err
}

// applyResult is what --apply did. Applied and Refused are moves; Stopped names
// the stage that failed, when one did.
type applyResult struct {
	Applied []organize.Move
	NotRun  []organize.Move
	// AlreadyPlaced are planned moves whose tab was already in its workspace
	// when its turn came (a peer or an earlier run got there first). Nothing
	// was called for them, so they are not Applied.
	AlreadyPlaced []organize.Move
	Refused       []refusedMove
	Reordered     []organize.OrderStep
	Warnings      []string

	// Stage is what was running when the run stopped, in words for the first
	// line of the failure summary; empty when the run finished.
	Stage string
	// Skipped names later stages that never started.
	Skipped []string
	Err     error

	// FocusTitle and FocusTab name the tab the caller's focus went back to;
	// both are empty when it was not restored. FocusErr is why a restore step
	// failed.
	FocusTitle string
	FocusTab   string
	FocusErr   error
}

// refusedMove is a move herdr declined. The plan predicts the refusals it can
// (a workspace's last tab), so a decline here means the session changed after
// the snapshot. The run carries on with the moves that do not depend on it,
// and the run ends in error.
type refusedMove struct {
	Move organize.Move
	Err  error
}

// focusState is what to put back when the run ends: the terminal the caller
// was focused on, and each workspace's active tab as its first pane's terminal.
type focusState struct {
	caller string
	// focusedWS is the workspace the caller was looking at. When the caller's
	// own tab is gone at restore time, its active tab is focused last instead.
	focusedWS string
	active    []activeTab
}

type activeTab struct {
	workspaceID string
	terminalID  string
}

func snapshotFocus(snap organize.Snapshot) focusState {
	var fs focusState
	firstPaneOf := map[string]herdr.Pane{}
	for _, p := range snap.Panes {
		if _, seen := firstPaneOf[p.TabID]; !seen {
			firstPaneOf[p.TabID] = p
		}
		if p.Focused && fs.caller == "" {
			fs.caller = p.TerminalID
			fs.focusedWS = p.WorkspaceID
		}
	}
	for _, w := range snap.Workspaces {
		if fs.focusedWS == "" && w.Focused {
			fs.focusedWS = w.WorkspaceID
		}
		if p, ok := firstPaneOf[w.ActiveTabID]; ok {
			fs.active = append(fs.active, activeTab{workspaceID: w.WorkspaceID, terminalID: p.TerminalID})
		}
	}
	return fs
}

// applyPlan carries out the plan against the live session. It re-reads herdr
// after each call that can renumber ids (a tab moving between workspaces, a
// workspace reorder); a tab is found again by its terminal id, which does not
// change. Focus is restored on every exit, error paths included.
func applyPlan(ctx context.Context, c *herdr.Client, snap organize.Snapshot, plan organize.Plan) (res applyResult) {
	focus := snapshotFocus(snap)
	defer func() { res.FocusTitle, res.FocusTab, res.FocusErr = restoreFocus(ctx, c, focus) }()

	var moves []organize.Move
	for _, m := range plan.Moves {
		if !m.Blocked {
			moves = append(moves, m)
		}
	}

	if res.Err = runMoves(ctx, c, moves, &res); res.Err != nil {
		res.Skipped = []string{"workspace order", "tab order"}
		return res
	}
	if res.Err = runWorkspaceOrder(ctx, c, plan, &res); res.Err != nil {
		res.Skipped = []string{"tab order"}
		return res
	}
	if res.Err = runTabOrder(ctx, c, plan, &res); res.Err != nil {
		return res
	}
	if len(res.Refused) > 0 {
		res.Err = refusedError(res.Refused)
		res.Stage = "moving tabs"
	}
	return res
}

func refusedError(refused []refusedMove) error {
	parts := make([]string, 0, len(refused))
	for _, r := range refused {
		parts = append(parts, fmt.Sprintf("%q [%s]: %s", r.Move.Title, r.Move.TabID, r.Err))
	}
	return fmt.Errorf("herdr declined %s: %s", plural(len(refused), "move", "moves"), strings.Join(parts, "; "))
}

// canonicalWorkspace finds the workspace a label names: the lowest-numbered one
// with that label, else the one this run created for it, if it still exists.
func canonicalWorkspace(wss []herdr.Workspace, label string, created map[string]string) (string, bool) {
	best := -1
	for i, w := range wss {
		if w.Label == label && (best < 0 || w.Number < wss[best].Number) {
			best = i
		}
	}
	if best >= 0 {
		return wss[best].WorkspaceID, true
	}
	if id, ok := created[label]; ok {
		for _, w := range wss {
			if w.WorkspaceID == id {
				return id, true
			}
		}
	}
	return "", false
}

// paneByTerminal indexes panes by terminal id. A pane with no terminal id is
// left out: it identifies nothing, and several of them would collide.
func paneByTerminal(panes []herdr.Pane) map[string]herdr.Pane {
	out := make(map[string]herdr.Pane, len(panes))
	for _, p := range panes {
		if p.TerminalID == "" {
			continue
		}
		out[p.TerminalID] = p
	}
	return out
}

// runMoves makes the planned moves. It re-reads panes and workspaces before a
// move only when the previous turn changed the session: a move between
// workspaces renumbers tab ids and can create a workspace, while a tab that
// was gone, already placed, or declined changed nothing, so the last read
// still holds.
func runMoves(ctx context.Context, c *herdr.Client, moves []organize.Move, res *applyResult) error {
	created := map[string]string{}
	var (
		panes []herdr.Pane
		wss   []herdr.Workspace
		stale = true
	)
	for i, m := range moves {
		if stale {
			var err error
			panes, err = c.Panes(ctx)
			if err != nil {
				return failStage(res, moves[i:], `reading panes before moving "`+m.Title+`"`, err)
			}
			wss, err = c.Workspaces(ctx)
			if err != nil {
				return failStage(res, moves[i:], `reading workspaces before moving "`+m.Title+`"`, err)
			}
			stale = false
		}
		cur, ok := paneByTerminal(panes)[m.TerminalID]
		if !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("tab %q [%s] is gone; skipped", m.Title, m.TabID))
			continue
		}
		dest, exists := canonicalWorkspace(wss, m.To, created)
		if exists && dest == cur.WorkspaceID {
			res.AlreadyPlaced = append(res.AlreadyPlaced, m)
			continue
		}
		target := herdr.ToNewWorkspace(m.To)
		if exists {
			target = herdr.ToWorkspace(dest)
		}
		result, err := c.MoveTab(ctx, cur.TabID, target)
		var declined *herdr.Declined
		switch {
		case err == nil:
			stale = true
			if !exists {
				created[m.To] = result.WorkspaceID
			}
			res.Applied = append(res.Applied, m)
		case errors.As(err, &declined):
			res.Refused = append(res.Refused, refusedMove{Move: m, Err: err})
			res.NotRun = append(res.NotRun, m)
		default:
			return failStage(res, moves[i:], fmt.Sprintf("moving %q [%s] to %s", m.Title, cur.TabID, m.To), err)
		}
	}
	return nil
}

func failStage(res *applyResult, notRun []organize.Move, stage string, err error) error {
	res.Stage = stage
	res.NotRun = append(res.NotRun, notRun...)
	return err
}

func runWorkspaceOrder(ctx context.Context, c *herdr.Client, plan organize.Plan, res *applyResult) error {
	// One step per pass, re-reading the workspace list between passes: a
	// workspace move renumbers workspaces and its reply is not decoded, so a
	// later step must not act on the numbering from before it.
	for pass := 0; ; pass++ {
		wss, err := c.Workspaces(ctx)
		if err != nil {
			res.Stage = "reading workspaces to order them"
			return err
		}
		label := make(map[string]string, len(wss))
		for _, w := range wss {
			label[w.WorkspaceID] = w.Label
		}
		current, target := organize.WorkspaceOrderTarget(wss, plan.Layout)
		steps, ok := organize.OrderSteps(current, target)
		if !ok || len(steps) == 0 {
			return nil
		}
		if pass > len(wss) {
			res.Stage = "ordering workspaces"
			return fmt.Errorf("the workspace order did not settle after %d moves", pass)
		}
		s := steps[0]
		if err := c.MoveWorkspace(ctx, s.TerminalID, s.Position); err != nil {
			res.Stage = "ordering workspaces"
			return err
		}
		// A workspace step has no tab id; Title carries the workspace label.
		res.Reordered = append(res.Reordered, organize.OrderStep{TerminalID: s.TerminalID, Title: label[s.TerminalID], Position: s.Position})
	}
}

// tabsInOrder returns the terminal ids of a workspace's tabs, in tab order. A
// tab with no pane, or whose first pane has no terminal id, gets a stand-in id
// so the positions stay right, as the plan gives it one.
func tabsInOrder(tabs []herdr.Tab, panes []herdr.Pane) (terminals []string, tabOf map[string]string) {
	firstOfTab := map[string]string{}
	for _, p := range panes {
		if _, seen := firstOfTab[p.TabID]; !seen {
			firstOfTab[p.TabID] = p.TerminalID
		}
	}
	tabOf = make(map[string]string, len(tabs))
	for _, t := range tabs {
		term, ok := firstOfTab[t.TabID]
		if !ok || term == "" {
			term = organize.StandInID(t.TabID)
		}
		terminals = append(terminals, term)
		tabOf[term] = t.TabID
	}
	return terminals, tabOf
}

// runTabOrder puts each layout workspace's tabs in order, one index move per
// pass. It reads the workspace list, the workspace's tabs, and the panes once
// per workspace: an index move keeps every tab id (measured), and its reply
// carries the workspace's tab list in the new order, which the next pass uses
// instead of listing again. A reply whose list is empty or lacks the moved tab
// is not trusted, and the tabs are listed again.
func runTabOrder(ctx context.Context, c *herdr.Client, plan organize.Plan, res *applyResult) error {
	for _, lw := range plan.Layout.Workspaces {
		stage := "ordering tabs in " + lw.Label
		wss, err := c.Workspaces(ctx)
		if err != nil {
			res.Stage = stage
			return err
		}
		wsID, ok := canonicalWorkspace(wss, lw.Label, nil)
		if !ok {
			continue
		}
		tabs, err := c.Tabs(ctx, wsID)
		if err != nil {
			res.Stage = stage
			return err
		}
		panes, err := c.Panes(ctx)
		if err != nil {
			res.Stage = stage
			return err
		}
		// The bound stops a herdr that ignores index moves.
		for pass := 0; pass <= len(lw.Tabs)+1; pass++ {
			current, tabOf := tabsInOrder(tabs, panes)
			steps, _ := organize.OrderSteps(current, organize.ArrangeTarget(layoutTerminals(lw), current))
			if len(steps) == 0 {
				break
			}
			if pass > len(lw.Tabs) {
				res.Stage = stage
				return fmt.Errorf("the tab order in %s did not settle after %d moves", lw.Label, pass)
			}
			s := steps[0]
			moved, err := c.MoveTab(ctx, tabOf[s.TerminalID], herdr.ToIndex(s.Position))
			if err != nil {
				res.Stage = fmt.Sprintf("moving %q to position %d in %s", titleOf(lw, s.TerminalID), s.Position+1, lw.Label)
				return err
			}
			tabs = moved.Tabs
			if !slices.ContainsFunc(tabs, func(t herdr.Tab) bool { return t.TabID == tabOf[s.TerminalID] }) {
				// The reply's list is the only read between passes, so one that
				// is empty or lacks the tab just moved cannot be trusted: an
				// empty list would read as a settled workspace. List again.
				if tabs, err = c.Tabs(ctx, wsID); err != nil {
					res.Stage = stage
					return err
				}
			}
			s.TabID = tabOf[s.TerminalID]
			s.Title = titleOf(lw, s.TerminalID)
			res.Reordered = append(res.Reordered, s)
		}
	}
	return nil
}

func layoutTerminals(lw organize.LayoutWorkspace) []string {
	out := make([]string, 0, len(lw.Tabs))
	for _, t := range lw.Tabs {
		out = append(out, t.TerminalID)
	}
	return out
}

func titleOf(lw organize.LayoutWorkspace, terminal string) string {
	for _, t := range lw.Tabs {
		if t.TerminalID == terminal {
			return t.Title
		}
	}
	return terminal
}

// restoreFocus puts the UI back the way the caller had it: each workspace's
// active tab first, the caller's own tab last. It works at tab grain, since
// herdr has no focus-by-pane, so a focused pane inside a split tab is not
// restored to pane grain. A tab that left its workspace is not the workspace's
// active tab any more and is skipped, as is one that is gone.
func restoreFocus(ctx context.Context, c *herdr.Client, fs focusState) (title, tabID string, err error) {
	if fs.caller == "" && len(fs.active) == 0 {
		return "", "", nil
	}
	panes, err := c.Panes(ctx)
	if err != nil {
		return "", "", err
	}
	byTerminal := paneByTerminal(panes)
	var errs []error
	for _, a := range fs.active {
		p, ok := byTerminal[a.terminalID]
		if !ok || p.WorkspaceID != a.workspaceID || a.workspaceID == fs.focusedWS {
			continue
		}
		if err := c.FocusTab(ctx, p.TabID); err != nil {
			errs = append(errs, err)
		}
	}
	if p, ok := byTerminal[fs.caller]; ok {
		if err := c.FocusTab(ctx, p.TabID); err != nil {
			errs = append(errs, err)
		} else {
			title, tabID = p.TerminalTitleStripped, p.TabID
		}
	} else {
		// The caller's own tab is gone: leave the UI in the workspace it was
		// looking at rather than on whichever workspace was restored last.
		if fs.focusedWS != "" {
			if err := c.FocusWorkspace(ctx, fs.focusedWS); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return title, tabID, errors.Join(errs...)
}
