package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// argsEqual is a small helper for asserting exact argv.
func argsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// opsFixture is one live session, $1 "alpha", on server 123/456 — enough for a
// revalidation to pass. Anything else the client asks for returns empty, so a
// test that accidentally drives an unexpected command gets an empty parse
// rather than a plausible-looking one.
func opsFixture(t *testing.T, inside bool) (*exec.FakeRunner, *Client, SessionIdentity) {
	t.Helper()
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return strings.Join([]string{"123", "456", "$1", "alpha", "2", "1", "1700000000", "/w"}, FieldSep), nil
		}
		return "", nil
	}}
	c := New(fake, WithInsideTmux(func() bool { return inside }))
	identityEnv(c, "", "/tmp")
	identity, err := c.ResolveSessionExact(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("ResolveSessionExact: %v", err)
	}
	fake.Calls = nil
	return fake, c, identity
}

// TestResolveSessionExactIsGoEquality is the heart of #237: resolution happens
// in Go, over a listing, so tmux's target grammar — where a missing "forge"
// falls through to the "forge-review" sibling — never runs at all.
func TestResolveSessionExactIsGoEquality(t *testing.T) {
	rows := []string{
		strings.Join([]string{"123", "456", "$1", "forge-review", "1", "0", "1700000000", "/w"}, FieldSep),
		strings.Join([]string{"123", "456", "$2", "=forge:", "1", "0", "1700000000", "/w"}, FieldSep),
		strings.Join([]string{"123", "456", "$3", "pr *", "1", "0", "1700000000", "/w"}, FieldSep),
	}
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return strings.Join(rows, "\n"), nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")

	// The reported defect: "forge" is absent, and its prefix sibling must NOT
	// answer for it.
	if _, err := c.ResolveSessionExact(context.Background(), "forge"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("resolving absent %q = %v, want ErrSessionNotFound (a prefix sibling answered)", "forge", err)
	}
	// Names that are tmux target syntax are just names here.
	for name, wantID := range map[string]string{"forge-review": "$1", "=forge:": "$2", "pr *": "$3"} {
		got, err := c.ResolveSessionExact(context.Background(), name)
		if err != nil {
			t.Fatalf("ResolveSessionExact(%q): %v", name, err)
		}
		if got.ID != wantID {
			t.Errorf("ResolveSessionExact(%q).ID = %q, want %q", name, got.ID, wantID)
		}
	}
}

func TestAttachSession_Inside(t *testing.T) {
	// Inside tmux: must switch-client (non-interactive), never attach — and the
	// -t operand is the native id, not the name.
	fake, c, identity := opsFixture(t, true)
	if err := c.AttachSession(context.Background(), identity); err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	call := fake.Last()
	if call.Interactive {
		t.Errorf("inside tmux must use the non-interactive switch path")
	}
	if call.Name != "tmux" {
		t.Errorf("expected tmux, got %q", call.Name)
	}
	argsEqual(t, call.Args, []string{"switch-client", "-t", "$1"})
}

func TestAttachSession_Outside(t *testing.T) {
	// Outside tmux: must attach-session, and it must take the tty (interactive).
	fake, c, identity := opsFixture(t, false)
	if err := c.AttachSession(context.Background(), identity); err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	call := fake.Last()
	if !call.Interactive {
		t.Errorf("outside tmux must use the interactive attach path")
	}
	argsEqual(t, call.Args, []string{"attach-session", "-t", "$1"})
}

func TestKillSession_TargetsNativeID(t *testing.T) {
	fake, c, identity := opsFixture(t, false)
	if err := c.KillSession(context.Background(), identity); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"kill-session", "-t", "$1"})
}

func TestKillOthers_TargetsNativeID(t *testing.T) {
	fake, c, identity := opsFixture(t, false)
	if err := c.KillOthers(context.Background(), identity); err != nil {
		t.Fatalf("KillOthers: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"kill-session", "-a", "-t", "$1"})
}

// TestRenameSession_NewNameIsAnOperand pins the argv split that makes rename
// safe: -t carries the native id of the session being renamed, and the trailing
// bare arg is the new name, passed through untouched behind `--`. A new name
// that reads as target syntax must not be "helpfully" rewritten.
func TestRenameSession_NewNameIsAnOperand(t *testing.T) {
	fake, c, identity := opsFixture(t, false)
	const newName = "=fresh:"
	if err := c.RenameSession(context.Background(), identity, newName); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"rename-session", "-t", "$1", "--", newName})
}

// TestRenameSession_DashNameStaysAnOperand is the flag-injection half. The new
// name is the only operator-controlled positional this package hands tmux, and
// the TUI's rename field is free text — so a name that opens with a dash must
// land after `--` rather than in tmux's own flag parser.
func TestRenameSession_DashNameStaysAnOperand(t *testing.T) {
	for _, newName := range []string{"-t$9", "--help", "-X", "-"} {
		t.Run(newName, func(t *testing.T) {
			fake, c, identity := opsFixture(t, false)
			if err := c.RenameSession(context.Background(), identity, newName); err != nil {
				t.Fatalf("RenameSession: %v", err)
			}
			args := fake.Last().Args
			argsEqual(t, args, []string{"rename-session", "-t", "$1", "--", newName})
			// The terminator must sit BEFORE the name; a `--` appended after it
			// would satisfy a set comparison while leaving the name parseable.
			if args[len(args)-2] != "--" {
				t.Fatalf("args = %v; the terminator must immediately precede the new name", args)
			}
		})
	}
}

// TestZeroMutationOnStaleIdentity is the destructive gate. Every mutating and
// selecting action must issue its command ZERO times when the captured identity
// no longer validates — the only tmux call permitted on a refusal is the
// read-only listing that discovered the problem.
func TestZeroMutationOnStaleIdentity(t *testing.T) {
	gen := ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"}
	stale := map[string]SessionIdentity{
		"server restarted":   {Generation: ServerGeneration{Selector: gen.Selector, PID: "999", StartTime: "999"}, ID: "$1", Name: "alpha"},
		"session gone":       {Generation: gen, ID: "$9", Name: "ghost"},
		"unqualified":        {ID: "$1", Name: "alpha"},
		"malformed id":       {Generation: gen, ID: "alpha", Name: "alpha"},
		"different selector": {Generation: ServerGeneration{Selector: ServerSelector{TmuxEnv: "/tmp/other,1,0"}, PID: "123", StartTime: "456"}, ID: "$1", Name: "alpha"},
	}
	actions := map[string]func(*Client, SessionIdentity) error{
		"AttachSession": func(c *Client, id SessionIdentity) error { return c.AttachSession(context.Background(), id) },
		"KillSession":   func(c *Client, id SessionIdentity) error { return c.KillSession(context.Background(), id) },
		"KillOthers":    func(c *Client, id SessionIdentity) error { return c.KillOthers(context.Background(), id) },
		"RenameSession": func(c *Client, id SessionIdentity) error { return c.RenameSession(context.Background(), id, "fresh") },
	}
	for reason, identity := range stale {
		for action, run := range actions {
			t.Run(reason+"/"+action, func(t *testing.T) {
				fake, c, _ := opsFixture(t, false)
				if err := run(c, identity); err == nil {
					t.Fatalf("%s accepted a stale identity (%s)", action, reason)
				}
				for _, call := range fake.Calls {
					if len(call.Args) > 0 && call.Args[0] != "list-sessions" {
						t.Fatalf("%s ran %v on a refusal; only the read-only listing is allowed", action, call.Args)
					}
				}
			})
		}
	}
}

// TestAttachWindow_SelectsThenAttachesByID pins the two-step window jump: the
// window is selected by its @id, then its PARENT session is attached by $id.
// Neither step ever sees a "session:index" composite.
func TestAttachWindow_SelectsThenAttachesByID(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-windows" {
			return strings.Join([]string{"123", "456", "@3", "$1", "alpha", "2", "shell", "1", "1"}, FieldSep), nil
		}
		return "", nil
	}}
	c := New(fake, WithInsideTmux(func() bool { return true }))
	identityEnv(c, "", "/tmp")
	want := WindowIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"},
		ID:         "@3", SessionID: "$1", Name: "shell",
	}
	if err := c.AttachWindow(context.Background(), want); err != nil {
		t.Fatalf("AttachWindow: %v", err)
	}
	var mutating [][]string
	for _, call := range fake.Calls {
		if len(call.Args) > 0 && call.Args[0] != "list-windows" {
			mutating = append(mutating, call.Args)
		}
	}
	if len(mutating) != 2 {
		t.Fatalf("calls = %v, want select-window then switch-client", mutating)
	}
	argsEqual(t, mutating[0], []string{"select-window", "-t", "@3"})
	argsEqual(t, mutating[1], []string{"switch-client", "-t", "$1"})
}

// TestAttachWindow_ZeroActionOnReparent is the parentage half of the gate: a
// window that moved to another session since capture keeps its @id, so "still
// exists" would wrongly pass. Selecting it would surface a window in a session
// the operator never chose.
func TestAttachWindow_ZeroActionOnReparent(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-windows" {
			return strings.Join([]string{"123", "456", "@3", "$2", "other", "0", "shell", "1", "1"}, FieldSep), nil
		}
		return "", nil
	}}
	c := New(fake, WithInsideTmux(func() bool { return true }))
	identityEnv(c, "", "/tmp")
	want := WindowIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"},
		ID:         "@3", SessionID: "$1", Name: "shell",
	}
	err := c.AttachWindow(context.Background(), want)
	if !errors.Is(err, ErrWrongParent) {
		t.Fatalf("AttachWindow error = %v, want ErrWrongParent", err)
	}
	for _, call := range fake.Calls {
		if len(call.Args) > 0 && call.Args[0] != "list-windows" {
			t.Fatalf("ran %v after a reparent refusal, want only the listing", call.Args)
		}
	}
}

// windowOpsFixture builds a client whose list-windows reports one window @3
// parented by the given session id, plus the identity that names @3 under $1.
// parentID lets a test declare the window has been reparented since capture.
func windowOpsFixture(t *testing.T, parentID string) (*exec.FakeRunner, *Client, WindowIdentity) {
	t.Helper()
	session := "alpha"
	if parentID != "$1" {
		session = "other"
	}
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-windows" {
			return strings.Join([]string{"123", "456", "@3", parentID, session, "2", "shell", "1", "1"}, FieldSep), nil
		}
		return "", nil
	}}
	c := New(fake, WithInsideTmux(func() bool { return true }))
	identityEnv(c, "", "/tmp")
	return fake, c, WindowIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"},
		ID:         "@3", SessionID: "$1", Name: "shell",
	}
}

// TestWindowVerbs_TargetNativeID covers the two window verbs AttachWindow's own
// tests do not: the destructive kill and the in-session select. Both go through
// RevalidateWindow, so both must issue a single @id-targeted command.
func TestWindowVerbs_TargetNativeID(t *testing.T) {
	for name, tc := range map[string]struct {
		run         func(*Client, WindowIdentity) error
		want        []string
		interactive bool
	}{
		"KillWindow": {
			run:  func(c *Client, id WindowIdentity) error { return c.KillWindow(context.Background(), id) },
			want: []string{"kill-window", "-t", "@3"},
		},
		"SelectWindow": {
			run:         func(c *Client, id WindowIdentity) error { return c.SelectWindow(context.Background(), id) },
			want:        []string{"select-window", "-t", "@3"},
			interactive: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, c, identity := windowOpsFixture(t, "$1")
			if err := tc.run(c, identity); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			call := fake.Last()
			argsEqual(t, call.Args, tc.want)
			if call.Interactive != tc.interactive {
				t.Errorf("interactive = %v, want %v", call.Interactive, tc.interactive)
			}
		})
	}
}

// TestWindowVerbs_ZeroActionOnReparent is the assertion AttachWindow already
// carries, extended to the destructive verb: a window that moved to another
// session since capture keeps its @id, so killing it would destroy a window in
// a session the operator never selected.
func TestWindowVerbs_ZeroActionOnReparent(t *testing.T) {
	for name, run := range map[string]func(*Client, WindowIdentity) error{
		"KillWindow":   func(c *Client, id WindowIdentity) error { return c.KillWindow(context.Background(), id) },
		"SelectWindow": func(c *Client, id WindowIdentity) error { return c.SelectWindow(context.Background(), id) },
	} {
		t.Run(name, func(t *testing.T) {
			fake, c, identity := windowOpsFixture(t, "$2")
			if err := run(c, identity); !errors.Is(err, ErrWrongParent) {
				t.Fatalf("%s error = %v, want ErrWrongParent", name, err)
			}
			for _, call := range fake.Calls {
				if len(call.Args) > 0 && call.Args[0] != "list-windows" {
					t.Fatalf("%s ran %v after a reparent refusal, want only the listing", name, call.Args)
				}
			}
		})
	}
}

// fakeLookPathFound always resolves — used by tests that exercise sesh
// delegation but must not depend on a real sesh binary on PATH.
func fakeLookPathFound(bin string) (string, error) {
	return "/usr/bin/" + bin, nil
}

func TestPick_DelegatesToSesh(t *testing.T) {
	// Pick must shell out to `sesh connect -- <name>` interactively (it takes the
	// tty). The name comes from `sesh list`, i.e. from whatever sessions exist on
	// the box — so it is terminated even though forgectl never composes it.
	for _, name := range []string{"projectx", "--help", "-t"} {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := New(fake, WithBins("tmux", "sesh"), WithLookPath(fakeLookPathFound))
			if err := c.Pick(context.Background(), name); err != nil {
				t.Fatalf("Pick: %v", err)
			}
			call := fake.Last()
			if call.Name != "sesh" {
				t.Errorf("expected sesh binary, got %q", call.Name)
			}
			if !call.Interactive {
				t.Errorf("sesh connect must take the tty (interactive)")
			}
			argsEqual(t, call.Args, []string{"connect", "--", name})
		})
	}
}

func TestSeshList_Parse(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "main\nprojectx\n~/Projects/foo", nil
	}}
	c := New(fake, WithBins("tmux", "sesh"), WithLookPath(fakeLookPathFound))
	names, err := c.SeshList(context.Background())
	if err != nil {
		t.Fatalf("SeshList: %v", err)
	}
	argsEqual(t, names, []string{"main", "projectx", "~/Projects/foo"})
	argsEqual(t, fake.Last().Args, []string{"list"})
}

func TestPick_SeshNotFound(t *testing.T) {
	// Without sesh on PATH, both sesh-delegating calls must fail with a clear,
	// attributed error rather than shelling out and letting exec.Runner's
	// generic not-found error surface unattributed.
	notFound := func(string) (string, error) {
		return "", errors.New("exec: \"sesh\": executable file not found in $PATH")
	}
	fake := &exec.FakeRunner{}
	c := New(fake, WithBins("tmux", "sesh"), WithLookPath(notFound))

	const wantMsg = "sesh not found on PATH"
	if err := c.Pick(context.Background(), "projectx"); err == nil {
		t.Fatal("Pick: expected error when sesh is not on PATH")
	} else if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("Pick: error = %q, want it to contain %q (a regression to an unattributed runner error must fail)", err, wantMsg)
	}
	if _, err := c.SeshList(context.Background()); err == nil {
		t.Fatal("SeshList: expected error when sesh is not on PATH")
	} else if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("SeshList: error = %q, want it to contain %q (a regression to an unattributed runner error must fail)", err, wantMsg)
	}
	if last := fake.Last(); last.Name != "" {
		t.Errorf("expected no exec call when the sesh guard fails, got %+v", last)
	}
}
