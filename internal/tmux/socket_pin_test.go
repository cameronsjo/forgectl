package tmux

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

const testSocket = "/tmp/fc-surface/sock"

// pinnedClient builds a socket-pinned client whose filesystem and environment
// seams are driven, so nothing here depends on a real tmux or a real socket.
func pinnedClient(t *testing.T, run *internalexec.FakeRunner) *Client {
	t.Helper()
	c, err := NewPinned(run, testSocket)
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	c.getenv = func(string) string { return "" }
	c.getuid = func() int { return 501 }
	c.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	return c
}

func TestNewPinnedValidatesTheSocketPath(t *testing.T) {
	tests := []struct {
		name    string
		socket  string
		wantErr string
	}{
		{"empty", "", "empty"},
		{"relative", "sock/tmux", "absolute"},
		{"bare name", "sock", "absolute"},
		{"unclean traversal", "/tmp/fc/../fc/sock", "clean"},
		{"unclean trailing slash", "/tmp/fc/sock/", "clean"},
		{"unclean dot", "/tmp/fc/./sock", "clean"},
		{"absolute and clean", testSocket, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewPinned(&internalexec.FakeRunner{}, tt.socket)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NewPinned(%q) = %v, want nil", tt.socket, err)
				}
				if c.socket != tt.socket {
					t.Errorf("socket = %q, want %q", c.socket, tt.socket)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewPinned(%q) = nil error, want one mentioning %q", tt.socket, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewPinned(%q) error = %q, want it to mention %q", tt.socket, err, tt.wantErr)
			}
		})
	}
}

// TestPinnedClientPinsEveryCommand is the structural assertion behind the whole
// design: it drives every tmux-issuing method on the Client and requires each
// resulting argv to lead with the pin.
//
// It asserts over the recorded calls rather than per-method so that a NEW tmux
// command added later without going through tmuxArgs fails here — a per-method
// test only covers the methods someone remembered to add a case for.
func TestPinnedClientPinsEveryCommand(t *testing.T) {
	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) { return "", nil },
	}
	c := pinnedClient(t, run)
	ctx := context.Background()

	session := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "$1",
		Name:       "forge",
	}
	window := WindowIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "@1",
		SessionID:  "$1",
	}

	// Every one of these is expected to fail somewhere downstream (the fake
	// returns no rows, so revalidation finds nothing) — the argv is what is
	// under test, not the outcome.
	_, _ = c.ListSessions(ctx)
	_, _ = c.ListWindows(ctx)
	_, _ = c.ListPanes(ctx)
	_, _ = c.CreateSession(ctx, "forge", "/tmp")
	_ = c.RenameSession(ctx, session, "renamed")
	_ = c.KillSession(ctx, session)
	_ = c.KillOthers(ctx, session)
	_ = c.KillWindow(ctx, window)
	_ = c.AttachSession(ctx, session)
	_ = c.SelectWindow(ctx, window)
	_ = c.LastSession(ctx)
	_, _ = c.CheckGenerationCapability(ctx)

	if len(run.Calls) == 0 {
		t.Fatal("no commands recorded — the test drove nothing")
	}
	for _, call := range run.Calls {
		if call.Name != "tmux" {
			t.Errorf("unexpected binary %q with args %v", call.Name, call.Args)
			continue
		}
		if len(call.Args) < 2 || call.Args[0] != "-S" || call.Args[1] != testSocket {
			t.Errorf("argv %v does not lead with the socket pin -S %s", call.Args, testSocket)
		}
	}
}

func TestUnpinnedClientArgvIsUnchanged(t *testing.T) {
	run := &internalexec.FakeRunner{}
	c := New(run)
	c.getenv = func(string) string { return "" }
	c.getuid = func() int { return 501 }
	c.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	if _, err := c.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(run.Calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(run.Calls))
	}
	want := []string{"list-sessions", "-F", sessionFormat}
	if !slices.Equal(run.Calls[0].Args, want) {
		t.Errorf("argv = %v, want %v — an unpinned client must be byte-identical to before the pin existed", run.Calls[0].Args, want)
	}
}

// TestPinnedClientMayCreateItsFirstServer is the finding that blocked #332: a
// `-S`-pinned argv used to be refused a serverAbsent verdict outright, so the
// surface adapter could never create the session it exists to create.
//
// The assertion is on ErrNoServer specifically, because that is the ONE
// classification documented as permitting a create.
func TestPinnedClientMayCreateItsFirstServer(t *testing.T) {
	c := pinnedClient(t, &internalexec.FakeRunner{})
	args := c.tmuxArgs("new-session", "-d", "-s", "forge")

	lstatted := ""
	c.lstat = func(path string) (os.FileInfo, error) {
		lstatted = path
		return nil, os.ErrNotExist
	}

	err := c.serverStateError(context.Background(), args, commandFailure("tmux", args, "no server running"))
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("serverStateError = %v, want ErrNoServer", err)
	}
	if lstatted != testSocket {
		t.Errorf("inspected socket %q, want the pinned %q — a pinned client must judge absence by ITS socket, never the derived default", lstatted, testSocket)
	}
}

// TestPinnedClientRefusesAnArgvAimedElsewhere covers both halves of pinnedArgs.
// The trailing-override case is the one that matters most: tmux honours the
// LAST `-S`, so an argv that merely starts with the pin can still run against a
// different server.
func TestPinnedClientRefusesAnArgvAimedElsewhere(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no pin at all", []string{"list-windows"}},
		{"a different socket", []string{"-S", "/tmp/someone-else", "list-windows"}},
		{"pin not leading", []string{"list-windows", "-S", testSocket}},
		{"pin overridden later", []string{"-S", testSocket, "list-windows", "-S", "/tmp/someone-else"}},
		{"pin overridden by label", []string{"-S", testSocket, "list-windows", "-L", "other"}},
		{"truncated pin", []string{"-S"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := pinnedClient(t, &internalexec.FakeRunner{})
			lstatCalls := 0
			c.lstat = func(string) (os.FileInfo, error) {
				lstatCalls++
				return nil, os.ErrNotExist
			}
			err := c.serverStateError(context.Background(), tt.args, commandFailure("tmux", tt.args, "no server running"))
			if errors.Is(err, ErrNoServer) {
				t.Errorf("argv %v got the create-permitting ErrNoServer", tt.args)
			}
			if !errors.Is(err, ErrServerUnreadable) {
				t.Errorf("argv %v = %v, want ErrServerUnreadable", tt.args, err)
			}
			if lstatCalls != 0 {
				t.Errorf("argv %v caused %d lstat calls — a refused argv must never reach the filesystem", tt.args, lstatCalls)
			}
		})
	}
}

// TestPinnedSelectorIgnoresTheEnvironment pins the two-mode contract on
// ServerSelector. $TMUX being set is the case that would otherwise manufacture
// a spurious ErrSelectorChanged the moment the operator attaches to any session.
func TestPinnedSelectorIgnoresTheEnvironment(t *testing.T) {
	c := pinnedClient(t, &internalexec.FakeRunner{})
	c.getenv = func(key string) string {
		switch key {
		case "TMUX":
			return "/tmp/tmux-501/default,42,0"
		case "TMUX_TMPDIR":
			return "/somewhere/else"
		}
		return ""
	}
	got := c.currentSelector()
	want := ServerSelector{Socket: testSocket}
	if got != want {
		t.Errorf("currentSelector() = %+v, want %+v", got, want)
	}
}

// TestPinnedIdentityIsRefusedByAnUnpinnedClient is the cross-server guarantee
// the Socket field exists to provide: without it both selectors are {"", ""}
// and an id minted on the pinned socket would be acted on against the default
// server, which is a stranger's session with the same native id.
func TestPinnedIdentityIsRefusedByAnUnpinnedClient(t *testing.T) {
	run := &internalexec.FakeRunner{}
	unpinned := New(run)
	unpinned.getenv = func(string) string { return "" }

	pinnedIdentity := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "$1",
		Name:       "forge",
	}
	_, err := unpinned.RevalidateSession(context.Background(), pinnedIdentity)
	if !errors.Is(err, ErrSelectorChanged) {
		t.Fatalf("RevalidateSession = %v, want ErrSelectorChanged", err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("ran %d commands before refusing: %v — a selector refusal must run zero tmux commands", len(run.Calls), run.Calls)
	}
}

// TestPinnedIdentityIsRefusedByADifferentPin is the same guarantee between two
// pinned clients — the case a single boolean "is pinned" flag would miss.
func TestPinnedIdentityIsRefusedByADifferentPin(t *testing.T) {
	run := &internalexec.FakeRunner{}
	other, err := NewPinned(run, "/tmp/fc-surface/other")
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	pinnedIdentity := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{Socket: testSocket}, PID: "9", StartTime: "1"},
		ID:         "$1",
	}
	if _, err := other.RevalidateSession(context.Background(), pinnedIdentity); !errors.Is(err, ErrSelectorChanged) {
		t.Fatalf("RevalidateSession = %v, want ErrSelectorChanged", err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("ran %d commands before refusing", len(run.Calls))
	}
}

// TestCreatedSessionCarriesThePinnedSelector closes the loop: an identity minted
// by a pinned CreateSession must be revalidatable by that same client, and the
// selector it carries is what makes the two previous tests refusals rather than
// accidents.
func TestCreatedSessionCarriesThePinnedSelector(t *testing.T) {
	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "new-session") {
				return "9" + FieldSep + "1" + FieldSep + "$3", nil
			}
			return "", nil
		},
	}
	c := pinnedClient(t, run)

	got, err := c.CreateSession(context.Background(), "forge", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	want := ServerSelector{Socket: testSocket}
	if got.Generation.Selector != want {
		t.Errorf("minted selector = %+v, want %+v", got.Generation.Selector, want)
	}
	if got.ID != "$3" {
		t.Errorf("minted id = %q, want $3", got.ID)
	}
}

// TestPinnedClientRefusesSesh guards the one delegation the pin cannot reach:
// sesh has no -S to thread, so it would act on the environmental server.
func TestPinnedClientRefusesSesh(t *testing.T) {
	run := &internalexec.FakeRunner{}
	c := pinnedClient(t, run)
	// A resolving lookPath proves the refusal is unconditional rather than an
	// incidental "sesh is not installed" failure.
	c.lookPath = func(string) (string, error) { return "/usr/bin/sesh", nil }

	if _, err := c.SeshList(context.Background()); !errors.Is(err, ErrSeshUnavailableWhenPinned) {
		t.Errorf("SeshList = %v, want ErrSeshUnavailableWhenPinned", err)
	}
	if err := c.Pick(context.Background(), "forge"); !errors.Is(err, ErrSeshUnavailableWhenPinned) {
		t.Errorf("Pick = %v, want ErrSeshUnavailableWhenPinned", err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("ran %d commands: %v — a refused sesh delegation must run nothing", len(run.Calls), run.Calls)
	}
}

// TestPinnedClientIsNeverInsideTmux: $TMUX describes a client of a DIFFERENT
// server under a pin, so reporting true would route switch-client to the pinned
// server on behalf of a client not attached to it.
func TestPinnedClientIsNeverInsideTmux(t *testing.T) {
	c, err := NewPinned(&internalexec.FakeRunner{}, testSocket, WithInsideTmux(func() bool { return true }))
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	if c.InsideTmux() {
		t.Error("InsideTmux() = true for a pinned client")
	}

	unpinned := New(&internalexec.FakeRunner{}, WithInsideTmux(func() bool { return true }))
	if !unpinned.InsideTmux() {
		t.Error("InsideTmux() = false for an unpinned client — the pin must not change the environmental answer")
	}
}
