package tmux

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// identityEnv builds the getenv seam for a Client under test: TMUX names the
// socket a client is attached to, TMUX_TMPDIR moves the default socket, and
// together they are the whole of what selects which server a bare `tmux`
// command reaches.
func identityEnv(c *Client, tmuxEnv, tmpDir string) {
	c.getenv = func(key string) string {
		switch key {
		case "TMUX":
			return tmuxEnv
		case "TMUX_TMPDIR":
			return tmpDir
		}
		return ""
	}
}

func TestValidateNativeIDs(t *testing.T) {
	tests := []struct {
		name    string
		fn      func(string) error
		value   string
		wantErr bool
	}{
		{"session ok", ValidateSessionID, "$0", false},
		{"session multi digit", ValidateSessionID, "$1234", false},
		{"session no sigil", ValidateSessionID, "1", true},
		{"session wrong sigil", ValidateSessionID, "@1", true},
		{"session empty", ValidateSessionID, "", true},
		{"session name", ValidateSessionID, "$forge", true},
		{"session trailing colon", ValidateSessionID, "$1:", true},
		{"session negative", ValidateSessionID, "$-1", true},
		{"window ok", ValidateWindowID, "@7", false},
		{"window wrong sigil", ValidateWindowID, "$7", true},
		{"window index", ValidateWindowID, "7", true},
		{"pane ok", ValidatePaneID, "%12", false},
		{"pane wrong sigil", ValidatePaneID, "@12", true},
		{"pane empty", ValidatePaneID, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.value)
			if tt.wantErr != (err != nil) {
				t.Fatalf("validate(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

// TestIdentityRejectsUnqualified is the "a bare ID may not cross a boundary"
// rule: an identity carrying an ID but no generation must be refused before any
// tmux command runs, not silently acted on against whatever server is current.
func TestIdentityRejectsUnqualified(t *testing.T) {
	gen := ServerGeneration{Selector: ServerSelector{}, PID: "9", StartTime: "100"}
	tests := []struct {
		name string
		id   SessionIdentity
	}{
		{"no generation at all", SessionIdentity{ID: "$1", Name: "forge"}},
		{"generation without pid", SessionIdentity{Generation: ServerGeneration{StartTime: "100"}, ID: "$1"}},
		{"generation without start time", SessionIdentity{Generation: ServerGeneration{PID: "9"}, ID: "$1"}},
		{"qualified but malformed id", SessionIdentity{Generation: gen, ID: "forge"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &internalexec.FakeRunner{}
			c := New(run)
			identityEnv(c, "", "/tmp")
			if _, err := c.RevalidateSession(context.Background(), tt.id); err == nil {
				t.Fatal("RevalidateSession accepted an unqualified identity")
			}
			if len(run.Calls) != 0 {
				t.Fatalf("ran %d tmux command(s) on a refusal, want 0: %+v", len(run.Calls), run.Calls)
			}
		})
	}
}

// TestRevalidateSessionRefusesSelectorChange is the socket-identity gate:
// default and custom server identity must never cross, and a changed selection
// must refuse rather than silently fall back to the default server.
func TestRevalidateSessionRefusesSelectorChange(t *testing.T) {
	captured := ServerGeneration{
		Selector:  ServerSelector{TmuxEnv: "/private/tmp/custom,4242,0", TmpDir: "/tmp"},
		PID:       "4242",
		StartTime: "1000",
	}
	run := &internalexec.FakeRunner{}
	c := New(run)
	// Now outside that custom server: TMUX is unset, so a bare tmux command
	// would reach the DEFAULT socket instead.
	identityEnv(c, "", "/tmp")
	_, err := c.RevalidateSession(context.Background(), SessionIdentity{Generation: captured, ID: "$1", Name: "forge"})
	if !errors.Is(err, ErrSelectorChanged) {
		t.Fatalf("RevalidateSession error = %v, want ErrSelectorChanged", err)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("ran %d tmux command(s) after a selector change, want 0: %+v", len(run.Calls), run.Calls)
	}
}

func sessionRow(pid, start, id, name, path string) string {
	return strings.Join([]string{pid, start, id, name, "1", "0", "1700000000", path}, FieldSep)
}

func TestRevalidateSession(t *testing.T) {
	gen := ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "9", StartTime: "100"}
	want := SessionIdentity{Generation: gen, ID: "$1", Name: "forge"}

	tests := []struct {
		name    string
		out     string
		wantErr error
		wantNm  string
	}{
		{"present", sessionRow("9", "100", "$1", "forge", "/w"), nil, "forge"},
		{
			// The name is display-only: a session renamed out from under the
			// capture is still the same object, and acting on it by ID is correct.
			"renamed since capture", sessionRow("9", "100", "$1", "forge-2", "/w"), nil, "forge-2",
		},
		{"gone", sessionRow("9", "100", "$4", "other", "/w"), ErrObjectGone, ""},
		{"server restarted", sessionRow("11", "900", "$1", "forge", "/w"), ErrGenerationChanged, ""},
		{
			// The dangerous one: the ID was reused by a NEW server generation.
			// Matching on ID alone would attach to a stranger.
			"id reused after restart", sessionRow("11", "900", "$1", "forge", "/w"), ErrGenerationChanged, "",
		},
		{"empty server", "", ErrObjectGone, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &internalexec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
				return tt.out, nil
			}}
			c := New(run)
			identityEnv(c, "", "/tmp")
			got, err := c.RevalidateSession(context.Background(), want)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != want.ID || got.Name != tt.wantNm {
				t.Fatalf("identity = %+v, want ID %q name %q", got, want.ID, tt.wantNm)
			}
			if got.Generation != gen {
				t.Fatalf("generation = %+v, want %+v", got.Generation, gen)
			}
		})
	}
}

func windowRow(pid, start, id, sessionID, sessionName string, index int, winName string) string {
	return strings.Join([]string{
		pid, start, id, sessionID, sessionName, strconv.Itoa(index), winName, "1", "1",
	}, FieldSep)
}

func TestRevalidateWindowProvesParentage(t *testing.T) {
	gen := ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "9", StartTime: "100"}
	want := WindowIdentity{Generation: gen, ID: "@3", SessionID: "$1", Name: "pr-o-r-1"}

	tests := []struct {
		name    string
		out     string
		wantErr error
	}{
		{"present under captured parent", windowRow("9", "100", "@3", "$1", "forge", 0, "pr-o-r-1"), nil},
		{
			// A window moved (`move-window`) to another session keeps its @ID.
			// Killing it by ID would kill a window in a session the operator
			// never selected.
			"reparented since capture", windowRow("9", "100", "@3", "$2", "other", 0, "pr-o-r-1"), ErrWrongParent,
		},
		{"gone", windowRow("9", "100", "@9", "$1", "forge", 0, "other"), ErrObjectGone},
		{"server restarted", windowRow("11", "900", "@3", "$1", "forge", 0, "pr-o-r-1"), ErrGenerationChanged},
		{"empty server", "", ErrObjectGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &internalexec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
				return tt.out, nil
			}}
			c := New(run)
			identityEnv(c, "", "/tmp")
			got, err := c.RevalidateWindow(context.Background(), want)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != want.ID || got.SessionID != want.SessionID {
				t.Fatalf("identity = %+v, want %+v", got, want)
			}
		})
	}
}

// TestRevalidateSurfacesUnreadableFields keeps #242's fail-closed contract
// reachable through the identity layer: a lossy separator rendering must not be
// read as "the object is gone", which would send a caller down a create or
// no-op path against a server that is very much alive.
func TestRevalidateSurfacesUnreadableFields(t *testing.T) {
	gen := ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "9", StartTime: "100"}
	run := &internalexec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		// tmux 3.7b under LANG=C substitutes "_" for the separator: one field.
		return "9_100_$1_forge_1_0_1700000000_/w", nil
	}}
	c := New(run)
	identityEnv(c, "", "/tmp")
	_, err := c.RevalidateSession(context.Background(), SessionIdentity{Generation: gen, ID: "$1", Name: "forge"})
	if !errors.Is(err, ErrUnreadableFields) {
		t.Fatalf("error = %v, want ErrUnreadableFields", err)
	}
	if errors.Is(err, ErrObjectGone) {
		t.Fatal("unreadable field separator was reported as a missing object")
	}
}

// TestParsersRejectMalformedNativeIDs pins the second half of the row-level
// defence. The exact field count stops a shifted row from impersonating a
// shorter one; this stops a row that survives the count from handing a caller
// an id-shaped value that is not an id. Every id leaving a parser goes straight
// into a `-t` operand, so an unvalidated one is the whole bug again.
//
// Each case pairs the bad row with a good one, so parsedRows' zero-row contract
// does not fire and the assertion is about the DROP.
func TestParsersRejectMalformedNativeIDs(t *testing.T) {
	goodSession := sessionRow("9", "100", "$1", "keep", "/w")
	goodWindow := windowRow("9", "100", "@1", "$1", "keep", 0, "keep")
	goodPane := strings.Join([]string{"9", "100", "%1", "@1", "0", "t", "zsh", "1"}, FieldSep)

	t.Run("session id", func(t *testing.T) {
		out := goodSession + "\n" + sessionRow("9", "100", "1", "forged", "/w")
		got, err := parseSessions(out)
		if err != nil {
			t.Fatalf("parseSessions: %v", err)
		}
		if len(got) != 1 || got[0].Name != "keep" {
			t.Fatalf("parseSessions = %+v, want only the well-formed row", got)
		}
	})
	t.Run("window parent session id", func(t *testing.T) {
		// The window id is fine; its PARENT is not. Both halves must hold, or
		// the row cannot prove parentage later.
		out := goodWindow + "\n" + windowRow("9", "100", "@2", "notanid", "forged", 0, "forged")
		got, err := parseWindows(out)
		if err != nil {
			t.Fatalf("parseWindows: %v", err)
		}
		if len(got) != 1 || got[0].Name != "keep" {
			t.Fatalf("parseWindows = %+v, want only the well-formed row", got)
		}
	})
	t.Run("pane id", func(t *testing.T) {
		out := goodPane + "\n" + strings.Join([]string{"9", "100", "@2", "@1", "1", "t", "zsh", "0"}, FieldSep)
		got, err := parsePanes(out)
		if err != nil {
			t.Fatalf("parsePanes: %v", err)
		}
		if len(got) != 1 || got[0].ID != "%1" {
			t.Fatalf("parsePanes = %+v, want only the well-formed row", got)
		}
	})
}

// TestServerStateErrorsAreTyped proves the identity layer routes #242's
// classifier verdicts into errors a caller can branch on — specifically that an
// absent DEFAULT server is distinguishable from an unreadable one, because only
// the former may proceed to create a session.
func TestServerStateErrorsAreTyped(t *testing.T) {
	args := []string{"list-sessions", "-F", sessionFormat}
	tests := []struct {
		name     string
		tmuxEnv  string
		lstatErr error
		want     error
	}{
		{"absent default", "", ErrNotExist, ErrNoServer},
		{"custom socket", "/tmp/custom,1,0", ErrNotExist, ErrServerUnreadable},
		{"stale socket", "", nil, ErrServerUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &internalexec.FakeRunner{RunFunc: func(_ string, a []string) (string, error) {
				return "", commandFailure("tmux", a, "no server running")
			}}
			c := New(run)
			identityEnv(c, tt.tmuxEnv, "/tmp")
			c.getuid = func() int { return 501 }
			c.lstat = func(string) (FileInfo, error) { return nil, tt.lstatErr }
			err := c.serverStateError(context.Background(), args, commandFailure("tmux", args, "x"))
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}
