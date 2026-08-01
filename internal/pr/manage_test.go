package pr

// Test plan for manage.go
//
// Attach (Classification: tmux dispatch + failure-path hinting)
//   [x] Happy: select-window argv targets the breadcrumb's window, interactive
//   [x] A missing-window failure from tmux is wrapped with an upgrade hint and
//       still unwraps (errors.Is) to the underlying tmux error
// Open (Classification: tmux dispatch argv)
//   [x] new-window argv targets the tmux session, workspace-shell window name,
//       and the breadcrumb's workspace as cwd
// List (Classification: breadcrumb enumeration)
//   [x] a $TMPDIR change between session creation and List does not hide
//       sessions (#184)

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

func TestAttach_Success(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Attach(context.Background(), path); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	tmux, ok := findCall(fake.Calls, "tmux")
	if !ok {
		t.Fatal("no tmux call")
	}
	want := []string{"select-window", "-t", "forgectl:pr-o-r-7"}
	if !equalArgs(tmux.Args, want) {
		t.Errorf("tmux args = %v, want %v", tmux.Args, want)
	}
	if !tmux.Interactive {
		t.Error("Attach should dispatch through the interactive path")
	}
}

func TestAttach_MissingWindow_Hints(t *testing.T) {
	underlying := errors.New("can't find window: pr-o-r-7")
	fake := &exec.FakeRunner{InteractiveErr: underlying}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	err := c.Attach(context.Background(), path)
	if err == nil {
		t.Fatal("expected an error when the review window is missing")
	}
	if !strings.Contains(err.Error(), "predate a forgectl upgrade") {
		t.Errorf("error = %q, want it to hint at a forgectl upgrade", err.Error())
	}
	if !strings.Contains(err.Error(), "relaunch the review with `pr <ref>`") {
		t.Errorf("error = %q, want it to include the relaunch instruction", err.Error())
	}
	if !errors.Is(err, underlying) {
		t.Errorf("error = %q, want it to wrap the underlying tmux error", err.Error())
	}
}

func TestOpen_TargetPins(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	path, ws := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}

	tmux, ok := findCall(fake.Calls, "tmux")
	if !ok {
		t.Fatal("no tmux call")
	}
	want := []string{"new-window", "-t", "forgectl", "-n", "pr-o-r-7-shell", "-c", ws}
	if !equalArgs(tmux.Args, want) {
		t.Errorf("tmux args = %v, want %v", tmux.Args, want)
	}
	if tmux.Interactive {
		t.Error("Open should dispatch through the non-interactive Run path")
	}
}

// TestList_SurvivesTempDirChange is the user-visible half of the #184
// regression: validateWorkspace used to read $TMPDIR at call time, so a
// `forgectl pr list` running under a different (or unset) $TMPDIR than the
// session that created the breadcrumbs skipped every one of them and printed an
// empty list rather than an error.
func TestList_SurvivesTempDirChange(t *testing.T) {
	// Both t.TempDir() calls here and the ones inside testClient/seedSession
	// have to precede t.Setenv — they resolve against $TMPDIR themselves.
	c := testClient(t, &exec.FakeRunner{})
	_, ws1 := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())
	_, ws2 := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, time.Now().UTC().Add(-time.Minute))
	otherRoot := t.TempDir()

	for _, ws := range []string{ws1, ws2} {
		if sandbox.WithinWorkspace(otherRoot, ws) {
			t.Fatalf("precondition: otherRoot %q must not contain workspace %q", otherRoot, ws)
		}
	}

	t.Setenv("TMPDIR", otherRoot)

	sessions, err := c.List()
	if err != nil {
		t.Fatalf("List after a $TMPDIR change: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("List() returned %d sessions after a $TMPDIR change, want 2", len(sessions))
	}
}
