package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestEscapeFormat pins the rule measured against tmux 3.4 (forgectl#806):
// every '#' is doubled except a run of '#' directly followed by '[', which
// tmux keeps verbatim. The real-tmux round trip is
// TestNamesLandVerbatimIsolated.
//
// Mutations that turn it red: double every '#' (the '[' rows grow); drop the
// doubling (every other row is unchanged input).
func TestEscapeFormat(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            "plain",
		"":                 "",
		"#":                "##",
		"a#b":              "a##b",
		"#{pid}":           "##{pid}",
		"#(touch x)":       "##(touch x)",
		"x##y":             "x####y",
		"end#":             "end##",
		"#[fg=red]":        "#[fg=red]",
		"##[x":             "##[x",
		"a#[b#c":           "a#[b##c",
		"##[{}#S":          "##[{}##S",
		"q#{session_name}": "q##{session_name}",
	} {
		if got := escapeFormat(in); got != want {
			t.Errorf("escapeFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCreateSessionRefusesFieldSep: a session named with 0x1F could never be
// listed again (its row splits into too many fields), so it is refused before
// tmux runs (forgectl#806).
//
// Mutation that turns it red: drop CreateSession's FieldSep check.
func TestCreateSessionRefusesFieldSep(t *testing.T) {
	fake := &internalexec.FakeRunner{}
	c := New(fake)
	_, err := c.CreateSession(context.Background(), "a"+FieldSep+"b", "")
	if !errors.Is(err, ErrUnsafeOperand) {
		t.Fatalf("CreateSession(0x1F name) = %v, want ErrUnsafeOperand", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("a refused create ran %v", fake.Calls)
	}
}

// TestParseSessionRowsCountsUnreadableRows: a row that does not split into
// exactly sessionFieldCount fields, or carries a malformed id, is dropped AND
// counted, so a listing can say a session exists that it cannot show
// (forgectl#806). Partial loss still does not fail the list.
//
// Mutation that turns it red: stop counting either drop.
func TestParseSessionRowsCountsUnreadableRows(t *testing.T) {
	row := func(id, name string) string {
		return strings.Join([]string{"1", "2", id, name, "1", "0", "1700000000", "/w"}, FieldSep)
	}
	out := strings.Join([]string{
		row("$0", "ok"),
		row("$1", "hidden"+FieldSep+"name"),
		row("bogus", "badid"),
	}, "\n")
	sessions, unreadable, err := parseSessionRows(out)
	if err != nil {
		t.Fatalf("parseSessionRows: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "ok" {
		t.Fatalf("sessions = %+v, want only the readable row", sessions)
	}
	if unreadable != 2 {
		t.Fatalf("unreadable = %d, want 2", unreadable)
	}
}
