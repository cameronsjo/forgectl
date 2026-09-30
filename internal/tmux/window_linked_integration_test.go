//go:build unix

package tmux

import (
	"context"
	"testing"
)

// TestRevalidateWindowAcceptsLinkedWindowIsolated is forgectl#762 against a
// real tmux: `link-window` makes list-windows -a print one @id once per
// session, and a captured parent that sorts after a foreign session must
// still revalidate.
//
// Mutation that turns it red: return ErrWrongParent on the first row with
// the id whose session differs.
func TestRevalidateWindowAcceptsLinkedWindowIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	for _, name := range []string{"aaa-other", "zzz-home"} {
		if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", name, "sleep 30"); err != nil {
			t.Fatalf("seed session %s: %v", name, err)
		}
	}
	home, err := c.ResolveSessionExact(ctx, "zzz-home")
	if err != nil {
		t.Fatalf("ResolveSessionExact: %v", err)
	}
	win, err := c.NewWindow(ctx, home, "linked", "", "sleep", "30")
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	other, err := c.ResolveSessionExact(ctx, "aaa-other")
	if err != nil {
		t.Fatalf("ResolveSessionExact: %v", err)
	}
	if _, err := runner.Run(ctx, tmuxBin, "link-window", "-d", "-s", win.ID, "-t", other.ID+":"); err != nil {
		t.Fatalf("link-window: %v", err)
	}

	// The test only means something if the foreign row really comes first.
	windows, err := c.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	var parents []string
	for _, w := range windows {
		if w.ID == win.ID {
			parents = append(parents, w.SessionID)
		}
	}
	if len(parents) != 2 || parents[0] != other.ID || parents[1] != home.ID {
		t.Fatalf("rows for %s name sessions %v, want [%s %s] — the listing order this test relies on changed",
			win.ID, parents, other.ID, home.ID)
	}

	got, err := c.RevalidateWindow(ctx, win)
	if err != nil {
		t.Fatalf("RevalidateWindow of a linked window under its captured parent: %v", err)
	}
	if got.SessionID != home.ID {
		t.Errorf("revalidated parent = %s, want %s", got.SessionID, home.ID)
	}
}
