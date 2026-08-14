package tmux

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// windowFormat is the -F spec for list-windows -a. Fields:
// server pid, server start, native window id, native PARENT session id,
// session name, window index, window name, active(1/0), pane count.
//
// The parent session id is what makes a window action provable: a window keeps
// its @id across `move-window`, so "still exists" is not the same question as
// "still belongs to the session the operator selected".
const windowFormat = IdentityFormat + FieldSep +
	"#{session_id}" + FieldSep +
	"#{session_name}" + FieldSep +
	"#{window_index}" + FieldSep +
	"#{window_name}" + FieldSep +
	"#{?window_active,1,0}" + FieldSep +
	"#{window_panes}"

// paneFormat is the -F spec for list-panes -a. Fields:
// server pid, server start, native pane id, native PARENT window id, pane
// index, title, current command, active(1/0).
const paneFormat = "#{pid}" + FieldSep +
	"#{start_time}" + FieldSep +
	"#{pane_id}" + FieldSep +
	"#{window_id}" + FieldSep +
	"#{pane_index}" + FieldSep +
	"#{pane_title}" + FieldSep +
	"#{pane_current_command}" + FieldSep +
	"#{?pane_active,1,0}"

// windowFieldCount and paneFieldCount are how many fields the two formats
// above emit. Named so the exact-count check and the fail-closed error can
// never report different numbers.
const (
	windowFieldCount = 9
	paneFieldCount   = 8
)

// ListWindows returns every window across all sessions (list-windows -a).
func (c *Client) ListWindows(ctx context.Context) ([]Window, error) {
	args := []string{"list-windows", "-a", "-F", windowFormat}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentDefaultServer(ctx, args, err) {
			return nil, nil
		}
		return nil, err
	}
	return parseWindows(out)
}

func parseWindows(out string) ([]Window, error) {
	lines := splitLines(out)
	windows := make([]Window, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		// EXACT, not >=: windowFormat emits exactly 8 fields, and a window name
		// may legally contain FieldSep (`tmux rename-window $'pr-o-r-1\x1fpad'`).
		// Under a >= check that name splits into a row whose Name reads
		// "pr-o-r-1", so WindowsLive (internal/pr/admission.go) would report a
		// torn-down review as still live in `pr list`. A separator in a name can
		// only ever push the count ABOVE 8, so requiring exactly 8 drops the
		// forged row instead of misreading it.
		if len(f) != windowFieldCount {
			continue
		}
		windows = append(windows, Window{
			ServerPID:   f[0],
			ServerStart: f[1],
			ID:          f[2],
			SessionID:   f[3],
			Session:     f[4],
			Index:       atoi(f[5]),
			Name:        f[6],
			Active:      f[7] == "1",
			Panes:       atoi(f[8]),
		})
	}
	// Every row failing at once is the separator being gone, not eight forged
	// names — see parsedRows for why that must be loud.
	return parsedRows(windows, lines, "list-windows", windowFieldCount)
}

// ListPanes returns every pane across all sessions (list-panes -a).
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	args := []string{"list-panes", "-a", "-F", paneFormat}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentDefaultServer(ctx, args, err) {
			return nil, nil
		}
		return nil, err
	}
	return parsePanes(out)
}

func parsePanes(out string) ([]Pane, error) {
	lines := splitLines(out)
	panes := make([]Pane, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		// Exact for the same reason parseWindows is: pane_title and
		// pane_current_command are no more separator-free than a window name.
		if len(f) != paneFieldCount {
			continue
		}
		panes = append(panes, Pane{
			ServerPID:   f[0],
			ServerStart: f[1],
			ID:          f[2],
			WindowID:    f[3],
			Index:       atoi(f[4]),
			Title:       f[5],
			Command:     f[6],
			Active:      f[7] == "1",
		})
	}
	return parsedRows(panes, lines, "list-panes", paneFieldCount)
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
// Grouping is by native id, not by name-and-index. The old composite key
// ("session name" + "window index") re-derived parentage from two mutable
// values, so a session renamed or a window moved between the three listings
// silently filed a window under the wrong session — the same class of mistake
// as targeting by name, showing up in the display layer.
func buildTree(sessions []Session, windows []Window, panes []Pane, m treeMarkers) string {
	winBySession := map[string][]Window{}
	for _, w := range windows {
		winBySession[w.SessionID] = append(winBySession[w.SessionID], w)
	}
	panesByWindow := map[string][]Pane{}
	for _, p := range panes {
		panesByWindow[p.WindowID] = append(panesByWindow[p.WindowID], p)
	}

	sorted := make([]Session, len(sessions))
	copy(sorted, sessions)
	// Native id breaks the tie: tmux forbids duplicate session names, but a
	// listing can still show two rows with one name mid-rename, and an
	// unspecified order there would render differently run to run.
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].ID < sorted[j].ID
	})

	var b strings.Builder
	for _, s := range sorted {
		marker := m.detached
		if s.Attached {
			marker = m.attached
		}
		fmt.Fprintf(&b, "%s %s\n", marker, s.Name)

		ws := winBySession[s.ID]
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

			ps := panesByWindow[w.ID]
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
