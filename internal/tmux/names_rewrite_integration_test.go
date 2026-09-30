//go:build unix

package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestEnsureSessionDottedNameOnceIsolated is forgectl#815 end to end on a real
// tmux: `forgectl open` names its session after a directory basename, and
// tmux stores "my.proj" as "my_proj". EnsureSession run twice on that name
// must create exactly one session and return it both times. Before the fix
// the second lookup of "my.proj" missed "my_proj" and created again.
//
// Mutation that turns it red: drop the normalizeSessionName call at the top
// of EnsureSession (the second call fails, or a second session appears).
func TestEnsureSessionDottedNameOnceIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, err := c.EnsureSession(ctx, "my.proj", "")
	if err != nil {
		t.Fatalf("first EnsureSession: %v", err)
	}
	second, err := c.EnsureSession(ctx, "my.proj", "")
	if err != nil {
		t.Fatalf("second EnsureSession: %v", err)
	}
	if first.ID != second.ID || second.Name != "my_proj" {
		t.Fatalf("EnsureSession returned %+v then %+v, want one session named my_proj", first, second)
	}
	names := sessionNames(t, c)
	if len(names) != 2 || names[0] != "base" || names[1] != "my_proj" {
		t.Fatalf("sessions = %q, want exactly [base my_proj]", names)
	}
}

// TestNameRulesMatchTmuxIsolated checks forgectl#815's name rules against a
// real tmux. Every name forgectl ALLOWS must land verbatim (or, for a session
// ':' or '.', as the predicted '_' spelling), and resolve exactly afterwards.
// Every name it REFUSES is also created through raw tmux, and the test logs
// when tmux no longer rewrites one — the refusal can then be relaxed. That
// half only logs, so a future tmux that stops rewriting does not fail CI.
//
// Mutation that turns it red: map ':' or '.' to anything other than '_' in
// sessionNameReplacer, refuse "a$1" (startsTmuxVariable accepting digits), or
// run normalizeSessionName's refusal before its mapping ("r$.b" is created).
func TestNameRulesMatchTmuxIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	base, err := c.ResolveSessionExact(ctx, "base")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}

	for asked, stored := range map[string]string{"a.b:c": "a_b_c", "a$1": "a$1", "a$}": "a$}", "a;b": "a;b", "a$": "a$"} {
		created, err := c.EnsureSession(ctx, asked, "")
		if err != nil {
			t.Fatalf("EnsureSession(%q): %v", asked, err)
		}
		got, err := c.ResolveSessionExact(ctx, stored)
		if err != nil || got.ID != created.ID {
			t.Fatalf("EnsureSession(%q) made %+v, but %q resolves to (%+v, %v)", asked, created, stored, got, err)
		}
	}
	// No backslash: tmux 3.7c vis-encodes one in a window name, so NewWindow
	// refuses it (checked below).
	for _, name := range []string{"w.x:y", "w$1", "w;v"} {
		window, err := c.NewWindow(ctx, base, name, "", "sleep", "60")
		if err != nil {
			t.Fatalf("NewWindow(%q): %v", name, err)
		}
		got, err := c.ResolveWindowExact(ctx, base, name)
		if err != nil || got.ID != window.ID {
			t.Fatalf("NewWindow(%q) made %s, but it resolves to (%+v, %v)", name, window.ID, got, err)
		}
	}

	if _, err := c.NewWindow(ctx, base, `w\z`, "", "sleep", "60"); !errors.Is(err, ErrUnsafeOperand) {
		t.Fatalf("NewWindow(w\\z) = %v, want ErrUnsafeOperand", err)
	}
	for _, name := range []string{"r$b", "r${x}", `r\b`, "r;", "r$.b", "r$:c"} {
		if _, err := c.CreateSession(ctx, name, ""); !errors.Is(err, ErrUnsafeOperand) {
			t.Fatalf("CreateSession(%q) = %v, want ErrUnsafeOperand", name, err)
		}
		out, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-P", "-F", "#{session_id}", "-s", name)
		if err != nil {
			t.Fatalf("raw new-session %q: %v", name, err)
		}
		sessions, err := c.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		for _, s := range sessions {
			if s.ID == out && s.Name == StoredSessionName(name) {
				t.Logf("tmux now stores session name %q verbatim; its refusal could be relaxed", name)
			}
		}
	}
}

// TestWindowWithFieldSepIsCountedIsolated is forgectl#815 item 1 on a real
// tmux: a window renamed by hand to carry 0x1F drops out of the listing, and
// DisplayWindowListing counts it. tmux 3.4 renders the byte as `\037` under a
// non-UTF-8 locale and raw under a UTF-8 one; either splits the row.
//
// Mutation that turns it red: return 0 in place of the count from
// parseWindowRows.
func TestWindowWithFieldSepIsCountedIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := runner.Run(ctx, tmuxBin, "new-window", "-d", "-t", "base:", "sleep 60"); err != nil {
		t.Fatalf("second window: %v", err)
	}
	if _, err := runner.Run(ctx, tmuxBin, "rename-window", "-t", "base:1", "--", "hid"+FieldSep+"den"); err != nil {
		// tmux 3.7 and later refuse the name ("invalid window name": check_name
		// accepts only printable ASCII and valid UTF-8), so such a window
		// cannot exist there. TestParseWindowRowsCountsDroppedRows still
		// covers the counting on every version.
		if strings.Contains(err.Error(), "invalid window name") {
			version, _ := runner.Run(ctx, tmuxBin, "-V")
			t.Skipf("%s refuses a window name carrying 0x1F, so there is no such row to count: %v", strings.TrimSpace(version), err)
		}
		t.Fatalf("rename-window: %v", err)
	}
	windows, unreadable, err := c.DisplayWindowListing(ctx)
	if err != nil {
		t.Fatalf("DisplayWindowListing: %v", err)
	}
	if len(windows) != 1 || unreadable != 1 {
		t.Fatalf("DisplayWindowListing = %d windows, %d unreadable; want 1 and 1", len(windows), unreadable)
	}
}
