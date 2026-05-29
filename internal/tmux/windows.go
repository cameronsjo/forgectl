package tmux

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// windowFormat is the -F spec for list-windows -a. Fields:
// session, window index, window name, active(1/0), pane count.
const windowFormat = "#{session_name}" + fieldSep +
	"#{window_index}" + fieldSep +
	"#{window_name}" + fieldSep +
	"#{?window_active,1,0}" + fieldSep +
	"#{window_panes}"

// paneFormat is the -F spec for list-panes -a. Fields:
// session, window index, pane index, title, current command, active(1/0).
const paneFormat = "#{session_name}" + fieldSep +
	"#{window_index}" + fieldSep +
	"#{pane_index}" + fieldSep +
	"#{pane_title}" + fieldSep +
	"#{pane_current_command}" + fieldSep +
	"#{?pane_active,1,0}"

// ListWindows returns every window across all sessions (list-windows -a).
func (c *Client) ListWindows(ctx context.Context) ([]Window, error) {
	out, err := c.run.Run(ctx, c.tmuxBin, "list-windows", "-a", "-F", windowFormat)
	if err != nil {
		if isNoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseWindows(out), nil
}

func parseWindows(out string) []Window {
	lines := splitLines(out)
	windows := make([]Window, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		if len(f) < 5 {
			continue
		}
		idx := atoi(f[1])
		windows = append(windows, Window{
			Session: f[0],
			Index:   idx,
			Name:    f[2],
			Active:  f[3] == "1",
			Panes:   atoi(f[4]),
			Target:  fmt.Sprintf("%s:%d", f[0], idx),
		})
	}
	return windows
}

// ListPanes returns every pane across all sessions (list-panes -a).
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := c.run.Run(ctx, c.tmuxBin, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		if isNoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return parsePanes(out), nil
}

func parsePanes(out string) []Pane {
	lines := splitLines(out)
	panes := make([]Pane, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		if len(f) < 6 {
			continue
		}
		win := atoi(f[1])
		idx := atoi(f[2])
		panes = append(panes, Pane{
			Session: f[0],
			Window:  win,
			Index:   idx,
			Title:   f[3],
			Command: f[4],
			Active:  f[5] == "1",
			Target:  fmt.Sprintf("%s:%d.%d", f[0], win, idx),
		})
	}
	return panes
}

// JumpToWindow jumps to a "session:index" target. It routes through
// AttachOrSwitch, so the headline cross-session jump works both inside tmux
// (switch-client) and outside (attach-session).
func (c *Client) JumpToWindow(ctx context.Context, target string) error {
	return c.AttachOrSwitch(ctx, target)
}

// KillOthers kills every session except keep (kill-session -a -t keep).
func (c *Client) KillOthers(ctx context.Context, keep string) error {
	_, err := c.run.Run(ctx, c.tmuxBin, "kill-session", "-a", "-t", keep)
	return err
}

// treeMarkers are the structural glyphs buildTree uses. Kept here (not in the
// tui glyph set) because the tree is assembled in the ops layer, which must
// not import tui. The icons flag on Tree selects between the two sets.
type treeMarkers struct {
	attached, detached, active string
}

var (
	iconTreeMarkers  = treeMarkers{attached: "●", detached: "○", active: "*"}
	asciiTreeMarkers = treeMarkers{attached: "*", detached: "-", active: "+"}
)

// Tree renders the session → window → pane hierarchy as indented text, ready
// to drop into a viewport. Sessions are sorted by name, windows by index,
// panes by index; the attached session and active window/pane are marked. Pass
// icons=false for ASCII markers (NO_COLOR / --no-icons / misconfigured term).
func (c *Client) Tree(ctx context.Context, icons bool) (string, error) {
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	windows, err := c.ListWindows(ctx)
	if err != nil {
		return "", err
	}
	panes, err := c.ListPanes(ctx)
	if err != nil {
		return "", err
	}
	m := iconTreeMarkers
	if !icons {
		m = asciiTreeMarkers
	}
	return buildTree(sessions, windows, panes, m), nil
}

// buildTree is the pure assembly step — no exec, no I/O — so it's directly
// testable from a fixture.
func buildTree(sessions []Session, windows []Window, panes []Pane, m treeMarkers) string {
	winBySession := map[string][]Window{}
	for _, w := range windows {
		winBySession[w.Session] = append(winBySession[w.Session], w)
	}
	type paneKey struct {
		session string
		window  int
	}
	panesByWindow := map[paneKey][]Pane{}
	for _, p := range panes {
		k := paneKey{p.Session, p.Window}
		panesByWindow[k] = append(panesByWindow[k], p)
	}

	sorted := make([]Session, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	for _, s := range sorted {
		marker := m.detached
		if s.Attached {
			marker = m.attached
		}
		fmt.Fprintf(&b, "%s %s\n", marker, s.Name)

		ws := winBySession[s.Name]
		sort.Slice(ws, func(i, j int) bool { return ws[i].Index < ws[j].Index })
		for _, w := range ws {
			active := ""
			if w.Active {
				active = " " + m.active
			}
			unit := "panes"
			if w.Panes == 1 {
				unit = "pane"
			}
			fmt.Fprintf(&b, "  %d: %s%s (%d %s)\n", w.Index, w.Name, active, w.Panes, unit)

			ps := panesByWindow[paneKey{s.Name, w.Index}]
			sort.Slice(ps, func(i, j int) bool { return ps[i].Index < ps[j].Index })
			for _, p := range ps {
				active := ""
				if p.Active {
					active = " " + m.active
				}
				cmd := p.Command
				if cmd == "" {
					cmd = p.Title
				}
				fmt.Fprintf(&b, "    %d: %s%s\n", p.Index, cmd, active)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
