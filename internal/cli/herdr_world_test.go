package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/herdr"
)

// The stateful half of herdrWorld: it answers list calls from live state and
// applies the mutating calls organize --apply makes, renumbering tab ids the
// way herdr does on a cross-workspace move.

func (w *herdrWorld) workspaceRows() []herdr.Workspace {
	out := make([]herdr.Workspace, len(w.workspaces))
	for i, ws := range w.workspaces {
		ws.ActiveTabID = w.active[ws.WorkspaceID]
		if ws.ActiveTabID == "" && len(w.tabs[ws.WorkspaceID]) > 0 {
			ws.ActiveTabID = w.tabs[ws.WorkspaceID][0].TabID
		}
		out[i] = ws
	}
	return out
}

func (w *herdrWorld) paneRows() []herdr.Pane {
	out := make([]herdr.Pane, len(w.panes))
	firstOfTab := map[string]bool{}
	for i, p := range w.panes {
		p.Focused = false
		if p.TabID == w.focusedTab && !firstOfTab[p.TabID] {
			p.Focused = true
		}
		firstOfTab[p.TabID] = true
		out[i] = p
	}
	return out
}

func (w *herdrWorld) wsIndex(id string) int {
	for i, ws := range w.workspaces {
		if ws.WorkspaceID == id {
			return i
		}
	}
	return -1
}

func (w *herdrWorld) findTab(id string) (wsID string, idx int, ok bool) {
	for ws, tabs := range w.tabs {
		for i, t := range tabs {
			if t.TabID == id {
				return ws, i, true
			}
		}
	}
	return "", 0, false
}

func (w *herdrWorld) retag(oldID, newID, newWS string) {
	for i := range w.panes {
		if w.panes[i].TabID == oldID {
			w.panes[i].TabID = newID
			w.panes[i].WorkspaceID = newWS
		}
	}
	if w.focusedTab == oldID {
		w.focusedTab = newID
	}
	for ws, id := range w.active {
		if id == oldID {
			w.active[ws] = newID
		}
	}
}

func reply(t *testing.T, result map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"id": "x", "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mutate handles the mutating herdr calls; ok is false for a call it does not
// own (the list calls).
func (w *herdrWorld) mutate(t *testing.T, args []string) (string, bool) {
	t.Helper()
	if w.active == nil {
		w.active = map[string]string{}
	}
	switch {
	case len(args) >= 3 && args[0] == "tab" && args[1] == "move":
		return w.moveTab(t, args), true
	case len(args) >= 5 && args[0] == "workspace" && args[1] == "move" && args[3] == "--index":
		id := args[2]
		idx, _ := strconv.Atoi(args[4])
		from := w.wsIndex(id)
		if from < 0 {
			t.Errorf("workspace move of unknown workspace %q", id)
			return reply(t, map[string]any{}), true
		}
		ws := w.workspaces[from]
		w.workspaces = append(w.workspaces[:from], w.workspaces[from+1:]...)
		w.workspaces = append(w.workspaces[:idx], append([]herdr.Workspace{ws}, w.workspaces[idx:]...)...)
		// herdr's workspace number is its 1-based position (workspace_list.json
		// numbers them 1..n in list order), so a reorder renumbers them.
		for i := range w.workspaces {
			w.workspaces[i].Number = i + 1
		}
		return reply(t, map[string]any{}), true
	case len(args) == 3 && args[0] == "tab" && args[1] == "focus":
		w.focusLog = append(w.focusLog, "tab:"+args[2])
		if ws, _, ok := w.findTab(args[2]); ok {
			w.focusedTab = args[2]
			w.active[ws] = args[2]
		} else {
			t.Errorf("tab focus of unknown tab %q", args[2])
		}
		return reply(t, map[string]any{}), true
	case len(args) == 3 && args[0] == "workspace" && args[1] == "focus":
		w.focusLog = append(w.focusLog, "workspace:"+args[2])
		return reply(t, map[string]any{}), true
	}
	return "", false
}

func (w *herdrWorld) moveTab(t *testing.T, args []string) string {
	t.Helper()
	tabID := args[2]
	srcWS, srcIdx, ok := w.findTab(tabID)
	if !ok {
		t.Errorf("tab move of unknown tab %q (a stale id?)", tabID)
		return reply(t, map[string]any{"move_result": map[string]any{"changed": false, "reason": "tab_not_found"}})
	}
	rest := args[3:]
	switch {
	case len(rest) == 2 && rest[0] == "--index":
		idx, _ := strconv.Atoi(rest[1])
		tabs := w.tabs[srcWS]
		if idx < 0 || idx >= len(tabs) {
			t.Errorf("index move of %q to %d, outside 0..%d", tabID, idx, len(tabs)-1)
			idx = max(0, min(idx, len(tabs)-1))
		}
		tab := tabs[srcIdx]
		tabs = append(tabs[:srcIdx], tabs[srcIdx+1:]...)
		tabs = append(tabs[:idx], append([]herdr.Tab{tab}, tabs[idx:]...)...)
		w.tabs[srcWS] = tabs
		return reply(t, map[string]any{"tabs": tabs})
	case len(rest) >= 1 && (rest[0] == "--workspace" || rest[0] == "--new-workspace"):
		if len(w.tabs[srcWS]) <= 1 {
			return reply(t, map[string]any{"move_result": map[string]any{"changed": false, "reason": "last_tab_in_workspace"}})
		}
		dst := ""
		if rest[0] == "--workspace" {
			dst = rest[1]
			if w.wsIndex(dst) < 0 {
				t.Errorf("tab move into unknown workspace %q", dst)
			}
		} else {
			w.nextID++
			dst = fmt.Sprintf("wn%d", w.nextID)
			label := ""
			if len(rest) == 3 && rest[1] == "--label" {
				label = rest[2]
			}
			w.workspaces = append(w.workspaces, herdr.Workspace{WorkspaceID: dst, Label: label, Number: len(w.workspaces) + 1})
		}
		tab := w.tabs[srcWS][srcIdx]
		w.tabs[srcWS] = append(w.tabs[srcWS][:srcIdx], w.tabs[srcWS][srcIdx+1:]...)
		w.nextID++
		newID := fmt.Sprintf("n%d", w.nextID)
		wasActive := w.active[srcWS] == tabID
		w.retag(tabID, newID, dst)
		if wasActive { // the source workspace activates a tab that is still in it
			delete(w.active, srcWS)
			if len(w.tabs[srcWS]) > 0 {
				w.active[srcWS] = w.tabs[srcWS][0].TabID
			}
		}
		tab.TabID, tab.WorkspaceID = newID, dst
		w.tabs[dst] = append(w.tabs[dst], tab)
		if w.renumberAll {
			for i := range w.tabs[srcWS] {
				w.nextID++
				oldID := w.tabs[srcWS][i].TabID
				fresh := fmt.Sprintf("n%d", w.nextID)
				w.tabs[srcWS][i].TabID = fresh
				w.retag(oldID, fresh, srcWS)
			}
		}
		return reply(t, map[string]any{
			"move_result": map[string]any{"changed": true, "tab_id": newID, "workspace_id": dst},
			"tabs":        w.tabs[dst],
		})
	}
	t.Errorf("unhandled tab move: %s", strings.Join(args, " "))
	return reply(t, map[string]any{})
}

// order returns the terminal ids of a workspace's tabs, in order.
func (w *herdrWorld) order(wsID string) []string {
	var out []string
	for _, tab := range w.tabs[wsID] {
		for _, p := range w.panes {
			if p.TabID == tab.TabID {
				out = append(out, p.TerminalID)
				break
			}
		}
	}
	return out
}

func (w *herdrWorld) labels() []string {
	var out []string
	for _, ws := range w.workspaces {
		out = append(out, ws.Label)
	}
	return out
}

func (w *herdrWorld) wsByLabel(label string) string {
	for _, ws := range w.workspaces {
		if ws.Label == label {
			return ws.WorkspaceID
		}
	}
	return ""
}
