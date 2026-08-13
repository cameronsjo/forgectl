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
		row := render(escaped, "123", "456", "@7", "reviews", "1", "pr-o-r-1", "0", "2")
		got[i] = parseWindows(row)
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
	if w.ServerPID != "123" || w.ServerStart != "456" || w.ID != "@7" || w.Name != "pr-o-r-1" || w.Panes != 2 {
		t.Errorf("window = %+v, want identity 123/456/@7 name pr-o-r-1 panes 2", w)
	}
}

func TestParsePanesReadsBothTmuxRenderings(t *testing.T) {
	var got [2][]Pane
	for i, escaped := range []bool{false, true} {
		row := render(escaped, "reviews", "1", "0", "title", "claude", "1")
		got[i] = parsePanes(row)
		if len(got[i]) != 1 {
			t.Fatalf("escaped=%v: parsePanes(%q) = %d rows, want 1", escaped, row, len(got[i]))
		}
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("rendering changed the parse:\n raw     = %+v\n escaped = %+v", got[0], got[1])
	}
	if got[0][0].Target != "reviews:1.0" || got[0][0].Command != "claude" {
		t.Errorf("pane = %+v, want target reviews:1.0 command claude", got[0][0])
	}
}

func TestParseSessionsReadsBothTmuxRenderings(t *testing.T) {
	var got [2][]Session
	for i, escaped := range []bool{false, true} {
		row := render(escaped, "reviews", "2", "1", "1700000000", "/tmp/wt")
		got[i] = parseSessions(row)
		if len(got[i]) != 1 {
			t.Fatalf("escaped=%v: parseSessions(%q) = %d rows, want 1", escaped, row, len(got[i]))
		}
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("rendering changed the parse:\n raw     = %+v\n escaped = %+v", got[0], got[1])
	}
	if got[0][0].Name != "reviews" || !got[0][0].Attached || got[0][0].Path != "/tmp/wt" {
		t.Errorf("session = %+v, want reviews attached /tmp/wt", got[0][0])
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
		row := render(escaped, "123", "456", "@7", "reviews", "1", forged, "0", "1")
		if got := parseWindows(row); len(got) != 0 {
			t.Fatalf("parseWindows(%q) = %+v, want the row dropped", row, got)
		}
	})
}

func TestParsePanesRejectsSeparatorInPaneTitle(t *testing.T) {
	renderings(t, func(t *testing.T, escaped bool) {
		row := render(escaped, "reviews", "1", "0", render(escaped, "title", "pad"), "claude", "1")
		if got := parsePanes(row); len(got) != 0 {
			t.Fatalf("parsePanes(%q) = %+v, want the row dropped", row, got)
		}
	})
}
