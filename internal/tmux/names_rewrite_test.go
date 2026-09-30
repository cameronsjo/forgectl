package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// calledCommand reports whether the fake ran a tmux command whose first argv
// element is sub, or whose guarded command string starts with it.
func calledCommand(fake *exec.FakeRunner, sub string) bool {
	for _, call := range fake.Calls {
		if len(call.Args) == 0 {
			continue
		}
		if call.Args[0] == sub {
			return true
		}
		if call.Args[0] == "if-shell" && len(call.Args) > 5 && strings.HasPrefix(call.Args[5], sub+" ") {
			return true
		}
	}
	return false
}

// TestNewWindowRefusesNamesTmuxRewrites is forgectl#815 items 1 and 5 for
// windows. Each refused name would land in tmux as something else — 0x1F
// hides the row from every listing, "$" before a letter gains a backslash, a
// trailing ";" is dropped — so ResolveWindowExact could never find the window
// again. The allowed names land verbatim in a window name (measured on tmux
// 3.4) and must still reach new-window.
//
// Mutation that turns it red: drop the refuseRewrittenName call in
// NewWindowWithEnv (every refused name reaches new-window).
func TestNewWindowRefusesNamesTmuxRewrites(t *testing.T) {
	for _, name := range []string{"pr\x1fpad", "a$b", "a${x}", "a$_x", "$HOME", "x;"} {
		t.Run(name, func(t *testing.T) {
			fake, c, identity := opsFixture(t, false)
			_, err := c.NewWindow(context.Background(), identity, name, "")
			if !errors.Is(err, ErrUnsafeOperand) {
				t.Fatalf("NewWindow(%q) = %v, want ErrUnsafeOperand", name, err)
			}
			if calledCommand(fake, "new-window") {
				t.Fatalf("NewWindow(%q) ran new-window; the name must be refused before any command", name)
			}
		})
	}
	for _, name := range []string{"my.proj", "a:b", `a\b`, "a$1", "a$}", "a;b", "a$"} {
		t.Run("allows "+name, func(t *testing.T) {
			fake, c, identity := opsFixture(t, false)
			_, err := c.NewWindow(context.Background(), identity, name, "")
			if errors.Is(err, ErrUnsafeOperand) {
				t.Fatalf("NewWindow(%q) refused a name tmux lands verbatim: %v", name, err)
			}
			if !calledCommand(fake, "new-window") {
				t.Fatalf("NewWindow(%q) never reached new-window", name)
			}
		})
	}
}

// TestCreateSessionNormalizesAndRefuses is forgectl#815 items 3 and 5 for
// sessions. ':' and '.' are mapped to the '_' tmux stores, so the argv and
// the returned identity both carry the name every later listing shows. The
// names tmux would rewrite unpredictably are refused before any command.
//
// Mutation that turns it red: return name unchanged from normalizeSessionName
// (the argv carries "my.proj"), or drop its refuseRewrittenName call (the
// refused names reach new-session).
func TestCreateSessionNormalizesAndRefuses(t *testing.T) {
	for name, stored := range map[string]string{"my.proj": "my_proj", "a:b.c": "a_b_c", "a$1;b": "a$1;b"} {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
				argsEqual(t, args, createArgs(stored, ""))
				return identityOut("123", "456", "$4"), nil
			}}
			c := New(fake)
			identityEnv(c, "", "/tmp")
			got, err := c.CreateSession(context.Background(), name, "")
			if err != nil {
				t.Fatalf("CreateSession(%q): %v", name, err)
			}
			if got.Name != stored {
				t.Fatalf("CreateSession(%q) identity named %q, want %q", name, got.Name, stored)
			}
		})
	}
	for _, name := range []string{"a$b", "a${x}", `a\b`, "x;", "a\tb", "a\x7fb", "a\xffb", "a\x1fb"} {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := New(fake)
			if _, err := c.CreateSession(context.Background(), name, ""); !errors.Is(err, ErrUnsafeOperand) {
				t.Fatalf("CreateSession(%q) = %v, want ErrUnsafeOperand", name, err)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("CreateSession(%q) ran %v; the name must be refused before any command", name, fake.Calls)
			}
		})
	}
}

// TestEnsureSessionFindsTheStoredDottedName is the forgectl#815 duplicate
// create at the unit level: tmux lists `forgectl open my.proj`'s session as
// my_proj, so EnsureSession("my.proj") must adopt it rather than create
// another. TestEnsureSessionDottedNameOnceIsolated is the real-tmux half.
//
// Mutation that turns it red: drop the normalizeSessionName call at the top
// of EnsureSession (the lookup misses "my_proj" and new-session runs).
func TestEnsureSessionFindsTheStoredDottedName(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return sessionListRow("123", "456", "$7", "my_proj"), nil
		}
		return "", errors.New("unexpected command")
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	got, err := c.EnsureSession(context.Background(), "my.proj", "/src/my.proj")
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if got.ID != "$7" || got.Name != "my_proj" {
		t.Fatalf("EnsureSession adopted %+v, want $7 my_proj", got)
	}
	if calledCommand(fake, "new-session") {
		t.Fatal("EnsureSession created a second session for a name tmux already stores")
	}
}

// TestRenameSessionRefusesNamesTmuxRewrites: a typed rename that tmux would
// store as something else is refused, and a ':' or '.' refusal names the '_'
// spelling to use instead. A trailing ';' is allowed, because the guarded
// rename's quotes keep it.
//
// Mutation that turns it red: drop the sessionNameReplacer check in
// RenameSession ("my.proj" reaches rename-session), or its refuseRewrittenName
// call ("a$b" does).
func TestRenameSessionRefusesNamesTmuxRewrites(t *testing.T) {
	for _, name := range []string{"my.proj", "a:b", "a$b", `a\b`, "a\xffb"} {
		t.Run(name, func(t *testing.T) {
			fake, c, identity := opsFixture(t, false)
			err := c.RenameSession(context.Background(), identity, name)
			if !errors.Is(err, ErrUnsafeOperand) {
				t.Fatalf("RenameSession(%q) = %v, want ErrUnsafeOperand", name, err)
			}
			if calledCommand(fake, "rename-session") {
				t.Fatalf("RenameSession(%q) ran rename-session", name)
			}
		})
	}
	_, c, identity := opsFixture(t, false)
	err := c.RenameSession(context.Background(), identity, "my.proj")
	if err == nil || !strings.Contains(err.Error(), `"my_proj"`) {
		t.Fatalf("RenameSession(my.proj) = %v, want it to name \"my_proj\"", err)
	}
	fake, c, identity := opsFixture(t, false)
	if err := c.RenameSession(context.Background(), identity, "x;"); err != nil {
		t.Fatalf("RenameSession(x;) = %v; the guarded quotes keep a trailing ';'", err)
	}
	if !calledCommand(fake, "rename-session") {
		t.Fatal("RenameSession(x;) never reached rename-session")
	}
}

// TestParseWindowRowsCountsDroppedRows is forgectl#815 item 1: a window row
// parseWindows drops is counted, as parseSessionRows already counted session
// rows (forgectl#806). A blank line is not a row.
//
// Mutation that turns it red: return 0 in place of the count from
// parseWindowRows.
func TestParseWindowRowsCountsDroppedRows(t *testing.T) {
	out := strings.Join([]string{
		windowRow("1", "2", "@1", "$1", "work", 0, "ok"),
		windowRow("1", "2", "@2", "$1", "work", 1, "pr"+FieldSep+"pad"),
		windowRow("1", "2", "bogus", "$1", "work", 2, "bad-id"),
		"",
	}, "\n")
	windows, unreadable, err := parseWindowRows(out)
	if err != nil {
		t.Fatalf("parseWindowRows: %v", err)
	}
	if len(windows) != 1 || windows[0].Name != "ok" {
		t.Fatalf("windows = %+v, want only \"ok\"", windows)
	}
	if unreadable != 2 {
		t.Fatalf("unreadable = %d, want 2", unreadable)
	}
}

// TestMostRecentSessionCountsDroppedRows is forgectl#815 item 2: the
// LastSession target goes through the counting parser, so a dropped row —
// possibly the real most-recent session — is counted rather than silent.
//
// Mutation that turns it red: return 0 in place of the count from
// mostRecentSession.
func TestMostRecentSessionCountsDroppedRows(t *testing.T) {
	out := strings.Join([]string{
		strings.Join([]string{"100", "1", "2", "$1", "older"}, FieldSep),
		strings.Join([]string{"200", "1", "2", "$2", "newer" + FieldSep + "pad"}, FieldSep),
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
	got, unreadable, err := New(fake).mostRecentSession(context.Background())
	if err != nil {
		t.Fatalf("mostRecentSession: %v", err)
	}
	if got.ID != "$1" || unreadable != 1 {
		t.Fatalf("mostRecentSession = %+v, %d unreadable; want $1 and 1", got, unreadable)
	}
}

// TestUnreadableRowsNote pins the note's wording: the sessions-only form is
// byte-identical to what `tmux ls` printed before forgectl#815, and a zero
// count says nothing.
func TestUnreadableRowsNote(t *testing.T) {
	const tail = " could not be read and are not listed — a name carrying the 0x1F field separator hides its row; " +
		"rename or kill it with tmux itself"
	for u, want := range map[UnreadableRows]string{
		{}:                        "",
		{Sessions: 2}:             "2 session(s)" + tail,
		{Windows: 1}:              "1 window(s)" + tail,
		{Sessions: 1, Windows: 3}: "1 session(s) and 3 window(s)" + tail,
	} {
		if got := u.Note(); got != want {
			t.Errorf("%+v.Note() = %q, want %q", u, got, want)
		}
	}
}

// TestTreeListingCountsSessionsAndWindows: TreeListing reports both counts
// from one render, so `tmux tree` and the TUI can print the note.
//
// Mutation that turns it red: return UnreadableRows{} from TreeListing.
func TestTreeListingCountsSessionsAndWindows(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return sessionListRow("1", "2", "$1", "work") + "\n" + sessionListRow("1", "2", "$2", "hid"+FieldSep+"den"), nil
		case "list-windows":
			return windowRow("1", "2", "@1", "$1", "work", 0, "ok") + "\n" +
				windowRow("1", "2", "@2", "$1", "work", 1, "a"+FieldSep+"b"), nil
		}
		return "", nil
	}}
	tree, unreadable, err := New(fake).TreeListing(context.Background(), false)
	if err != nil {
		t.Fatalf("TreeListing: %v", err)
	}
	if unreadable != (UnreadableRows{Sessions: 1, Windows: 1}) {
		t.Fatalf("unreadable = %+v, want 1 session and 1 window", unreadable)
	}
	if !slices.Contains(strings.Split(tree, "\n"), "- work") {
		t.Fatalf("tree = %q, want the readable session drawn", tree)
	}
}
