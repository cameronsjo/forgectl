package herdrcli

// Test plan for herdr_focus_terminal.go
//
//   [x] checkFocusTerminalArgs refuses a hostile terminal id and any
//       herdr-app path but an absolute one to herdr-app
//   [x] focusTerminal selects the located space, then its tab
//   [x] A terminal no tab holds selects nothing and returns the error

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckFocusTerminalArgs(t *testing.T) {
	ok := "/Users/x/.local/bin/herdr-app"
	cases := []struct {
		terminal, app string
		wantErr       bool
	}{
		{"t146", ok, false},
		{"t1'; rm -rf ~", ok, true},
		{"", ok, true},
		{"t146", "herdr-app", true},
		{"t146", "/bin/sh", true},
		{"t146", "/tmp/herdr-app/../sh", true},
	}
	for _, c := range cases {
		if err := checkFocusTerminalArgs(c.terminal, c.app); (err != nil) != c.wantErr {
			t.Errorf("checkFocusTerminalArgs(%q, %q) = %v, wantErr %v", c.terminal, c.app, err, c.wantErr)
		}
	}
}

type fakeFocuser struct {
	calls  []string
	locErr error
}

func (f *fakeFocuser) LocateTerminal(_ context.Context, terminal string) (string, string, error) {
	f.calls = append(f.calls, "locate "+terminal)
	return "sp1", "tb1", f.locErr
}

func (f *fakeFocuser) FocusWorkspace(_ context.Context, id string) error {
	f.calls = append(f.calls, "space "+id)
	return nil
}

func (f *fakeFocuser) FocusTab(_ context.Context, id string) error {
	f.calls = append(f.calls, "tab "+id)
	return nil
}

func TestFocusTerminal_SelectsSpaceThenTab(t *testing.T) {
	f := &fakeFocuser{}
	if err := focusTerminal(context.Background(), f, "t146"); err != nil {
		t.Fatalf("focusTerminal: %v", err)
	}
	if got := strings.Join(f.calls, ", "); got != "locate t146, space sp1, tab tb1" {
		t.Errorf("calls = %s", got)
	}
}

func TestFocusTerminal_UnknownTerminalSelectsNothing(t *testing.T) {
	gone := errors.New("no tab holds terminal")
	f := &fakeFocuser{locErr: gone}
	if err := focusTerminal(context.Background(), f, "t9"); !errors.Is(err, gone) {
		t.Fatalf("err = %v, want the locate error", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want only the locate", f.calls)
	}
}
