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

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// reviewServer answers the listings a manage-path call needs: the review
// session "forgectl" as $1, holding the named review windows. windowNames are
// window names, in order, each getting a distinct @id starting at @5.
func reviewServer(windowNames ...string) *exec.FakeRunner {
	sessionRow := strings.Join([]string{"123", "456", "$1", "forgectl", "1", "0", "1700000000", "/w"}, "\x1f")
	rows := make([]string, 0, len(windowNames))
	for i, name := range windowNames {
		rows = append(rows, strings.Join([]string{
			"123", "456", "@" + strconv.Itoa(5+i), "$1", "forgectl", strconv.Itoa(i), name, "0", "1",
		}, "\x1f"))
	}
	windowsOut := strings.Join(rows, "\n")
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return sessionRow, nil
		case "list-windows":
			return windowsOut, nil
		}
		return "", nil
	}}
}

func TestAttach_Success(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	fake := reviewServer(mustWindowName(t, ref))
	c := testClient(t, fake)
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Attach(context.Background(), path); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	call, ok := findCallVerb(fake.Calls, "tmux", "select-window")
	if !ok {
		t.Fatal("no select-window call")
	}
	want := []string{"select-window", "-t", "@5"}
	if !equalArgs(call.Args, want) {
		t.Errorf("tmux args = %v, want %v", call.Args, want)
	}
	if !call.Interactive {
		t.Error("Attach should dispatch through the interactive path")
	}
}

// TestAttach_SiblingSessionWindowIsNotSelected is the parentage half of #237
// on the PR path. Window names are not unique across a tmux server, so a
// same-named window in ANOTHER session must not answer for this review's.
func TestAttach_SiblingSessionWindowIsNotSelected(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	sessionRow := strings.Join([]string{"123", "456", "$1", "forgectl", "1", "0", "1700000000", "/w"}, "\x1f")
	// The only window with this review's name lives in a DIFFERENT session ($2).
	strayRow := strings.Join([]string{"123", "456", "@9", "$2", "other", "0", mustWindowName(t, ref), "0", "1"}, "\x1f")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return sessionRow, nil
		case "list-windows":
			return strayRow, nil
		}
		return "", nil
	}}
	c := testClient(t, fake)
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Attach(context.Background(), path); err == nil {
		t.Fatal("Attach selected a same-named window belonging to another session")
	}
	if _, ok := findCallVerb(fake.Calls, "tmux", "select-window"); ok {
		t.Error("select-window ran against a window outside the review session")
	}
}

func TestAttach_MissingWindow_Hints(t *testing.T) {
	// A running review session with no windows at all: the review's window is
	// genuinely gone, which is the case the hint is written for.
	fake := reviewServer()
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
	if !errors.Is(err, tmux.ErrObjectGone) {
		t.Errorf("error = %q, want it to wrap tmux.ErrObjectGone", err.Error())
	}
}

func TestOpen_TargetPins(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	fake := reviewServer(mustWindowName(t, ref))
	c := testClient(t, fake)
	path, ws := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}

	call, ok := findCallVerb(fake.Calls, "tmux", "new-window")
	if !ok {
		t.Fatal("no new-window call")
	}
	// The destination is the review session's native id with its trailing
	// colon — never the session NAME, and never a "=name:" spelling. The window
	// name is the shell ROLE's own name, not the review name plus a suffix.
	shell, err := shellWindowName(ref)
	if err != nil {
		t.Fatalf("shellWindowName: %v", err)
	}
	want := []string{"new-window", "-t", "$1:", "-n", shell, "-c", ws}
	if !equalArgs(call.Args, want) {
		t.Errorf("tmux args = %v, want %v", call.Args, want)
	}
	if call.Interactive {
		t.Error("Open should dispatch through the non-interactive Run path")
	}
}
