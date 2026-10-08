//go:build unix

package tmux

import (
	"context"
	"errors"
	"testing"
)

// TestKillWindowGenerationGuardIsolated measures the forgectl#756 guard
// against a real tmux: the one command that kills must refuse when the server
// receiving it is not the captured incarnation, and must kill when it is.
//
// It drives killWindowGuarded directly with a doctored start time, because
// KillWindow's own revalidation would refuse that identity before the kill
// ever ran — the point here is that the kill command refuses ON ITS OWN, which
// is what closes the gap between revalidation and kill.
//
// Mutation that turns it red: have killWindowGuarded run a bare
// `kill-window -t <id>` — the replaced-server case then kills the window.
func TestKillWindowGenerationGuardIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "home", "sleep 30"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	session, err := c.ResolveSessionExact(ctx, "home")
	if err != nil {
		t.Fatalf("ResolveSessionExact: %v", err)
	}
	win, err := c.NewWindow(ctx, session, "victim", "", "sleep", "30")
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	stranger := win
	stranger.Generation.StartTime += "0"
	err = c.killWindowGuarded(ctx, stranger, stranger)
	if !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("guarded kill under another generation: err = %v, want ErrGenerationChanged", err)
	}
	if _, err := c.RevalidateWindow(ctx, win); err != nil {
		t.Fatalf("the window did not survive a kill aimed at another generation: %v", err)
	}

	if err := c.KillWindow(ctx, win); err != nil {
		t.Fatalf("KillWindow under the captured generation: %v", err)
	}
	if _, err := c.RevalidateWindow(ctx, win); !errors.Is(err, ErrObjectGone) {
		t.Fatalf("after KillWindow: revalidate err = %v, want ErrObjectGone", err)
	}

	// The #746 answer must survive the guard byte for byte: a guarded kill of
	// an id that no longer exists is tmux's exact "can't find window" line.
	if err := c.killWindowGuarded(ctx, win, win); !errors.Is(err, ErrObjectGone) {
		t.Fatalf("guarded kill of a gone window: err = %v, want ErrObjectGone", err)
	}
}
