package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestAllRowsUnreadableIsAnEmptyListing is forgectl#826: when every row is
// dropped but the separator plainly survived (a row split into more fields
// than the format emits, or into exactly that many with a malformed id), the
// parser returns an empty listing and the count, not the locale error. A
// server whose only window carries FieldSep in its name produces exactly this.
//
// Mutation that turns it red: drop the separator-survived loop from
// parsedRows (every case errors with ErrUnreadableFields).
func TestAllRowsUnreadableIsAnEmptyListing(t *testing.T) {
	for label, parse := range map[string]func() (int, int, error){
		"window name carries FieldSep": func() (int, int, error) {
			w, n, err := parseWindowRows(windowRow("1", "2", "@1", "$1", "work", 0, "a"+FieldSep+"b"))
			return len(w), n, err
		},
		"session name carries FieldSep": func() (int, int, error) {
			s, n, err := parseSessionRows(sessionListRow("1", "2", "$1", "a"+FieldSep+"b"))
			return len(s), n, err
		},
		"pane command carries FieldSep": func() (int, int, error) {
			p, n, err := parsePaneRows(paneRow("%1", "@1", "t", "a"+FieldSep+"b"))
			return len(p), n, err
		},
		"escaped rendering, window name carries FieldSep": func() (int, int, error) {
			row := strings.ReplaceAll(windowRow("1", "2", "@1", "$1", "work", 0, "a"+FieldSep+"b"), FieldSep, escapedFieldSep)
			w, n, err := parseWindowRows(row)
			return len(w), n, err
		},
		"malformed id at the exact field count": func() (int, int, error) {
			s, n, err := parseSessionRows(sessionListRow("1", "2", "bogus", "work"))
			return len(s), n, err
		},
	} {
		rows, unreadable, err := parse()
		if err != nil {
			t.Errorf("%s: error = %v, want an empty listing", label, err)
			continue
		}
		if rows != 0 || unreadable != 1 {
			t.Errorf("%s: %d rows, %d unreadable; want 0 and 1", label, rows, unreadable)
		}
	}
}

// TestAllRowsUnreadableKeepsTheLocaleErrorWhenNoSeparatorSurvived is the
// control for the test above: output in which no line reaches the format's
// field count still fails closed with the locale hint. That covers tmux 3.7b's
// `_` substitution under LANG=C, where every row is one field, and a truncated
// row with too few fields.
//
// Mutation that turns it red: return rows, nil from parsedRows whenever any
// line split at all (len > 1) instead of into at least want fields.
func TestAllRowsUnreadableKeepsTheLocaleErrorWhenNoSeparatorSurvived(t *testing.T) {
	for label, out := range map[string]string{
		"underscore rendering": strings.ReplaceAll(windowRow("1", "2", "@1", "$1", "work", 0, "ok"), FieldSep, "_"),
		"too few fields":       strings.Join([]string{"1", "2", "@1"}, FieldSep),
	} {
		_, _, err := parseWindowRows(out)
		if !errors.Is(err, ErrUnreadableFields) {
			t.Errorf("%s: error = %v, want ErrUnreadableFields", label, err)
		}
		if err != nil && !strings.Contains(err.Error(), "UTF-8 locale") {
			t.Errorf("%s: error = %v, want the locale hint", label, err)
		}
	}
}

// TestForgedLossyRowKeepsTheLocaleError is forgectl#836 item 5. Under tmux
// 3.7b's lossy `_` rendering every row is one field, but a window name holding
// the literal text `\037` enough times splits its OWN row into the format's
// field count. Without the decimal-first-field rule that forged row "proves"
// the separator survived, so the whole listing, live windows included, comes
// back empty with no error. It must stay the locale error.
//
// Mutation that turns it red: drop `&& isDecimal(f[0])` from parsedRows.
func TestForgedLossyRowKeepsTheLocaleError(t *testing.T) {
	live := strings.ReplaceAll(windowRow("1", "2", "@1", "$1", "work", 0, "ok"), FieldSep, "_")
	forged := strings.ReplaceAll(windowRow("1", "2", "@2", "$1", "work", 1,
		strings.Repeat(escapedFieldSep, windowFieldCount)), FieldSep, "_")
	if n := len(splitFields(forged)); n < windowFieldCount {
		t.Fatalf("fixture: the forged row splits into %d fields, want at least %d", n, windowFieldCount)
	}
	windows, _, err := parseWindowRows(live + "\n" + forged)
	if !errors.Is(err, ErrUnreadableFields) {
		t.Fatalf("parseWindowRows = (%d windows, %v), want ErrUnreadableFields", len(windows), err)
	}
}

// TestNeverAttachedUnreadableRowIsEmpty is the parse-level half of
// TestNeverAttachedUnreadableSessionIsEmptyIsolated, which skips on tmux 3.7c
// (it refuses a 0x1F session name). tmux renders a never-attached session's
// #{session_last_attached} as "", so a format led by it has no decimal first
// field. The lone unreadable row must still read as no session plus one
// unreadable row, not the locale error.
//
// Mutation that turns it red: move #{session_last_attached} back to the front
// of lastAttachedFormat, and lastAttachedRow's ts with it.
func TestNeverAttachedUnreadableRowIsEmpty(t *testing.T) {
	out := lastAttachedRow("", "91", "1700000000", "$0", "a"+FieldSep+"b")
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
	got, unreadable, err := New(fake).mostRecentSession(context.Background())
	if err != nil {
		t.Fatalf("mostRecentSession: %v; want no session and one unreadable row", err)
	}
	if got.ID != "" || unreadable != 1 {
		t.Fatalf("mostRecentSession = %+v, %d unreadable; want none and 1", got, unreadable)
	}
}

// TestTreeListingWithOnlyUnreadableRows: `tmux tree` and the TUI get an empty
// tree plus the counts, so they print the unreadable-rows note instead of an
// error blaming the locale (forgectl#826).
//
// Mutation that turns it red: drop the separator-survived loop from
// parsedRows.
func TestTreeListingWithOnlyUnreadableRows(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return sessionListRow("1", "2", "$1", "work"), nil
		case "list-windows":
			return windowRow("1", "2", "@1", "$1", "work", 0, "a"+FieldSep+"b"), nil
		case "list-panes":
			return paneRow("%1", "@1", "t", "a"+FieldSep+"b"), nil
		}
		return "", nil
	}}
	_, unreadable, err := New(fake).TreeListing(context.Background(), false)
	if err != nil {
		t.Fatalf("TreeListing: %v", err)
	}
	if unreadable != (UnreadableRows{Windows: 1, Panes: 1}) {
		t.Fatalf("unreadable = %+v, want 1 window and 1 pane", unreadable)
	}
}
