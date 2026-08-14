package tmux

import (
	"reflect"
	"testing"
)

// TestParseWindowsReadsBothTmuxRenderings is the regression test for the Linux
// CI failure: on tmux 3.4 every -F field arrived joined by the literal four
// characters \037, so a parser splitting only on the raw byte saw one field and
// dropped every row. Asserting both renderings produce the IDENTICAL Window
// means whichever one the local tmux does not emit still fails here.
func TestParseWindowsReadsBothTmuxRenderings(t *testing.T) {
	var got [2][]Window
	for i, escaped := range []bool{false, true} {
		row := render(escaped, "123", "456", "@7", "$3", "reviews", "1", "pr-o-r-1", "0", "2")
		parsed, err := parseWindows(row)
		if err != nil {
			t.Fatalf("escaped=%v: parseWindows(%q): %v", escaped, row, err)
		}
		got[i] = parsed
		if len(got[i]) != 1 {
			t.Fatalf("escaped=%v: parseWindows(%q) = %d rows, want 1", escaped, row, len(got[i]))
		}
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("rendering changed the parse:\n raw     = %+v\n escaped = %+v", got[0], got[1])
	}
	// The identity fields are what VerifyDispatched matches on, so pin them
	// explicitly rather than resting on the two renderings merely agreeing.
	w := got[0][0]
	if w.ServerPID != "123" || w.ServerStart != "456" || w.ID != "@7" || w.SessionID != "$3" || w.Name != "pr-o-r-1" || w.Panes != 2 {
		t.Errorf("window = %+v, want identity 123/456/@7 under $3, name pr-o-r-1, panes 2", w)
	}
}

func TestParsePanesReadsBothTmuxRenderings(t *testing.T) {
	var got [2][]Pane
	for i, escaped := range []bool{false, true} {
		row := render(escaped, "123", "456", "%2", "@7", "0", "title", "claude", "1")
		parsed, err := parsePanes(row)
		if err != nil {
			t.Fatalf("escaped=%v: parsePanes(%q): %v", escaped, row, err)
		}
		got[i] = parsed
		if len(got[i]) != 1 {
			t.Fatalf("escaped=%v: parsePanes(%q) = %d rows, want 1", escaped, row, len(got[i]))
		}
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("rendering changed the parse:\n raw     = %+v\n escaped = %+v", got[0], got[1])
	}
	if got[0][0].ID != "%2" || got[0][0].WindowID != "@7" || got[0][0].Command != "claude" {
		t.Errorf("pane = %+v, want %%2 under @7, command claude", got[0][0])
	}
}

func TestParseSessionsReadsBothTmuxRenderings(t *testing.T) {
	var got [2][]Session
	for i, escaped := range []bool{false, true} {
		row := render(escaped, "123", "456", "$1", "reviews", "2", "1", "1700000000", "/tmp/wt")
		parsed, err := parseSessions(row)
		if err != nil {
			t.Fatalf("escaped=%v: parseSessions(%q): %v", escaped, row, err)
		}
		got[i] = parsed
		if len(got[i]) != 1 {
			t.Fatalf("escaped=%v: parseSessions(%q) = %d rows, want 1", escaped, row, len(got[i]))
		}
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("rendering changed the parse:\n raw     = %+v\n escaped = %+v", got[0], got[1])
	}
	if got[0][0].ID != "$1" || got[0][0].Name != "reviews" || !got[0][0].Attached || got[0][0].Path != "/tmp/wt" {
		t.Errorf("session = %+v, want $1 reviews attached /tmp/wt", got[0][0])
	}
}

func TestParseGenerationIdentityReadsBothTmuxRenderings(t *testing.T) {
	renderings(t, func(t *testing.T, escaped bool) {
		got, err := parseGenerationIdentity(render(escaped, "4677", "1786644304", "@1"))
		if err != nil {
			t.Fatalf("parseGenerationIdentity: %v", err)
		}
		// Normalized onto the raw separator whichever way it arrived, so the
		// captured identity and a list-windows row stay comparable.
		if want := render(false, "4677", "1786644304", "@1"); got != want {
			t.Errorf("identity = %q, want %q", got, want)
		}
	})
}

// TestParseWindowsRejectsSeparatorInWindowName is the finding-3 regression. A
// window name may legally carry FieldSep, and under the old `len(f) >= 8` check
// `pr-o-r-1<sep>padding` parsed as Name == "pr-o-r-1" — so WindowsLive reported
// a torn-down review as still live in `pr list`. A separator inside a name can
// only push the count above 8, so the exact check drops the row instead.
func TestParseWindowsRejectsSeparatorInWindowName(t *testing.T) {
	renderings(t, func(t *testing.T, escaped bool) {
		forged := render(escaped, "pr-o-r-1", "padding")
		good := render(escaped, "123", "456", "@7", "$3", "reviews", "0", "shell", "1", "1")
		row := render(escaped, "123", "456", "@8", "$3", "reviews", "1", forged, "0", "1")
		// The good row alongside it is load-bearing: it keeps parsedRows' zero-row
		// contract from firing, so this pins the silent DROP rather than the
		// refusal — which is what stops a hostile window name from taking down
		// the whole listing.
		out := good + "\n" + row
		got, err := parseWindows(out)
		if err != nil {
			t.Fatalf("parseWindows(%q): %v", out, err)
		}
		if len(got) != 1 || got[0].Name != "shell" {
			t.Fatalf("parseWindows(%q) = %+v, want only the well-formed row", out, got)
		}
	})
}

func TestParsePanesRejectsSeparatorInPaneTitle(t *testing.T) {
	renderings(t, func(t *testing.T, escaped bool) {
		good := render(escaped, "123", "456", "%1", "@7", "1", "plain", "zsh", "0")
		row := render(escaped, "123", "456", "%2", "@7", "0", render(escaped, "title", "pad"), "claude", "1")
		out := good + "\n" + row
		got, err := parsePanes(out)
		if err != nil {
			t.Fatalf("parsePanes(%q): %v", out, err)
		}
		if len(got) != 1 || got[0].Title != "plain" {
			t.Fatalf("parsePanes(%q) = %+v, want only the well-formed row", out, got)
		}
	})
}

// parseSessions had no separator-in-name test at all while parseWindows and
// parsePanes did — and it was the parser still on a `< 5` check, so a session
// named `work<sep>pad` parsed as Name "work" with Path reading a window count.
func TestParseSessionsRejectsSeparatorInSessionName(t *testing.T) {
	renderings(t, func(t *testing.T, escaped bool) {
		forged := render(escaped, "work", "pad")
		good := render(escaped, "123", "456", "$1", "plain", "2", "1", "1700000000", "/tmp/good")
		row := render(escaped, "123", "456", "$2", forged, "2", "0", "1700000000", "/tmp/wt")
		out := good + "\n" + row
		got, err := parseSessions(out)
		if err != nil {
			t.Fatalf("parseSessions(%q): %v", out, err)
		}
		if len(got) != 1 || got[0].Name != "plain" || got[0].Path != "/tmp/good" {
			t.Fatalf("parseSessions(%q) = %+v, want only the well-formed row", out, got)
		}
	})
}
