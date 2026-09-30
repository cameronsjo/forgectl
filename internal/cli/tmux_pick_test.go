package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// seshPickClient builds a client whose sesh PATH check is stubbed, so a call
// that gets past forgectl's own gate is recorded on the fake runner instead of
// needing a real sesh binary.
func seshPickClient(fake *exec.FakeRunner) *tmux.Client {
	return tmux.New(fake,
		tmux.WithInsideTmux(func() bool { return true }),
		tmux.WithLookPath(func(string) (string, error) { return "sesh", nil }),
	)
}

// seshCalls counts the recorded invocations of sesh.
func seshCalls(fake *exec.FakeRunner) int {
	n := 0
	for _, c := range fake.Calls {
		if c.Name == "sesh" {
			n++
		}
	}
	return n
}

// hostileSeshNames are candidates sesh would hand to `tmux new-session -c`
// unescaped (#841): a directory path and a zoxide-listed path carrying a
// tmux #() job, plus a bare '#' format.
var hostileSeshNames = []string{
	"/tmp/x#(touch marker)",
	"~/src/proj#(id)",
	"#{pane_pid}",
}

func TestSeshPick_RefusesHashAtBothEntryPoints(t *testing.T) {
	ctx := context.Background()
	entryPoints := []struct {
		name string
		call func(*tmux.Client, string) error
	}{
		{"tmux pick <name>", func(c *tmux.Client, name string) error {
			cmd := newTmuxPickCmd(c)
			cmd.SetArgs([]string{"--", name})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			return cmd.ExecuteContext(ctx)
		}},
		{"TUI picker dispatch", func(c *tmux.Client, name string) error {
			return dispatchAction(ctx, c, tui.Action{Kind: tui.ActionPick, Pick: name})
		}},
	}
	for _, ep := range entryPoints {
		for _, name := range hostileSeshNames {
			t.Run(ep.name+"/"+name, func(t *testing.T) {
				fake := liveServer()
				err := ep.call(seshPickClient(fake), name)
				if !errors.Is(err, errSeshUnsafeCandidate) {
					t.Fatalf("err = %v, want errSeshUnsafeCandidate", err)
				}
				if n := seshCalls(fake); n != 0 {
					t.Fatalf("sesh was invoked %d time(s) for %q; the gate must refuse before sesh runs", n, name)
				}
			})
		}
	}
}

func TestSeshPick_PassesOrdinaryNameToSesh(t *testing.T) {
	fake := liveServer()
	if err := seshPick(context.Background(), seshPickClient(fake), "/tmp/project"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := seshCalls(fake); n != 1 {
		t.Fatalf("sesh calls = %d, want 1", n)
	}
	last := fake.Last()
	want := []string{"connect", "--", "/tmp/project"}
	if len(last.Args) != len(want) {
		t.Fatalf("args = %q, want %q", last.Args, want)
	}
	for i := range want {
		if last.Args[i] != want[i] {
			t.Fatalf("args = %q, want %q", last.Args, want)
		}
	}
}
