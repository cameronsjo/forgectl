//go:build unix

package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNamesLandVerbatimIsolated is forgectl#806 against a real tmux. tmux
// format-expands a new-session -s name, a new-window -n name and a
// rename-session name, so `#(cmd)` ran a shell job and `#{pid}` landed as a
// number. Each hostile name must now land byte for byte through all three,
// and the `#(...)` names must start no job.
//
// Mutation that turns it red: pass the name unescaped at any of the three
// sites (the landed name differs, and the canary appears).
func TestNamesLandVerbatimIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	canaryDir, err := os.MkdirTemp("/tmp", "f806-canary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(canaryDir) })
	canary := filepath.Join(canaryDir, "ran")

	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	base, err := c.ResolveSessionExact(ctx, "base")
	if err != nil {
		t.Fatalf("ResolveSessionExact(base): %v", err)
	}
	hostile := []string{
		"#(touch " + canary + ")", "#{pid}", "a##b", "#[fg=red]x", "##[x", "end#", "#,}", "q#{session_name}", "#H",
	}
	for i, name := range hostile {
		created, err := c.CreateSession(ctx, name, "")
		if err != nil {
			t.Fatalf("CreateSession(%q): %v", name, err)
		}
		if got, err := c.ResolveSessionExact(ctx, name); err != nil || got.ID != created.ID {
			t.Fatalf("created %q, but it resolves to (%+v, %v) — the name did not land as given", name, got, err)
		}
		if err := c.KillSession(ctx, created); err != nil {
			t.Fatalf("KillSession(%q): %v", name, err)
		}

		if err := c.RenameSession(ctx, base, name); err != nil {
			t.Fatalf("RenameSession(%q): %v", name, err)
		}
		if got, err := c.ResolveSessionExact(ctx, name); err != nil || got.ID != base.ID {
			t.Fatalf("renamed to %q, but it resolves to (%+v, %v) — the name did not land as given", name, got, err)
		}
		base.Name = name

		window, err := c.NewWindow(ctx, base, name, "", "sleep", "60")
		if err != nil {
			t.Fatalf("NewWindow(%q): %v", name, err)
		}
		windows, err := c.ListWindows(ctx)
		if err != nil {
			t.Fatalf("ListWindows: %v", err)
		}
		landed := ""
		for _, w := range windows {
			if w.ID == window.ID {
				landed = w.Name
			}
		}
		if landed != name {
			t.Fatalf("window %d named %q landed as %q", i, name, landed)
		}
	}

	// Duplicate detection still reads tmux's "duplicate session: <name>" line,
	// which names the expanded — so the original — name.
	if _, err := c.CreateSession(ctx, "dup#{pid}", ""); err != nil {
		t.Fatalf("CreateSession(dup): %v", err)
	}
	if _, err := c.CreateSession(ctx, "dup#{pid}", ""); !errors.Is(err, ErrDuplicateSession) {
		t.Fatalf("second CreateSession(dup) = %v, want ErrDuplicateSession", err)
	}

	// tmux runs #() jobs asynchronously; give one time to land before
	// concluding none ran.
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(canary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a #(...) name ran a shell job: canary stat = %v", err)
	}
}
