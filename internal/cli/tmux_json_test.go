package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

func TestTmuxWindowsJSON_EmptyEncodesArray(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTmuxWindowsJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("empty windows = %s, want []", got)
	}
}

func TestTmuxWindowsJSON_CarriesNativeIDs(t *testing.T) {
	var buf bytes.Buffer
	wins := []tmux.Window{{ID: "@3", SessionID: "$1", Session: "work", Index: 2, Name: "edit", Active: true, Panes: 2}}
	if err := writeTmuxWindowsJSON(&buf, wins); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	want := map[string]any{"id": "@3", "session_id": "$1", "session": "work", "index": 2.0, "name": "edit", "active": true, "panes": 2.0}
	if len(rows) != 1 || len(rows[0]) != len(want) {
		t.Fatalf("rows = %v, want one row with %d fields", rows, len(want))
	}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("%s = %v, want %v", k, rows[0][k], v)
		}
	}
}

func TestTmuxTreeJSON_EmptyEncodesArray(t *testing.T) {
	var buf bytes.Buffer
	if err := encodeTmuxTreeJSON(&buf, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("empty tree = %s, want []", got)
	}
}

// Grouping is by native parent id and ordering matches the text tree:
// sessions by name, windows and panes by index. A window with no panes still
// encodes panes as [], never null.
func TestTmuxTreeJSON_GroupsByIDAndSorts(t *testing.T) {
	sessions := []tmux.Session{{ID: "$2", Name: "zeta"}, {ID: "$1", Name: "alpha", Attached: true}}
	windows := []tmux.Window{
		{ID: "@2", SessionID: "$1", Index: 1, Name: "second"},
		{ID: "@1", SessionID: "$1", Index: 0, Name: "first", Active: true},
		{ID: "@9", SessionID: "$2", Index: 0, Name: "lonely"},
	}
	panes := []tmux.Pane{
		{ID: "%2", WindowID: "@1", Index: 1, Command: "vim"},
		{ID: "%1", WindowID: "@1", Index: 0, Command: "zsh", Active: true},
	}
	var buf bytes.Buffer
	if err := encodeTmuxTreeJSON(&buf, sessions, windows, panes); err != nil {
		t.Fatal(err)
	}
	var got []tmuxTreeSessionJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("sessions out of order: %+v", got)
	}
	if w := got[0].Windows; len(w) != 2 || w[0].Name != "first" || w[1].Name != "second" {
		t.Fatalf("windows out of order: %+v", w)
	}
	if p := got[0].Windows[0].Panes; len(p) != 2 || p[0].ID != "%1" || p[1].ID != "%2" {
		t.Errorf("panes out of order: %+v", p)
	}
	if !strings.Contains(buf.String(), `"panes": []`) {
		t.Errorf("a window with no panes should encode panes as []:\n%s", buf.String())
	}
}
