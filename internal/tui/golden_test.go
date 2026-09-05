package tui

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// The goldens exist for exactly one job: proving the charm.land/v2 migration is
// behavior-neutral. They were captured on the pre-swap tree (bubbles/bubbletea/
// lipgloss v1) and are asserted unchanged afterwards, so a chrome or color
// regression the swap would otherwise ship shows up as a diff rather than as
// nothing at all.
//
// Rendering is forced to truecolor so the goldens carry real SGR sequences. A
// golden captured under the test binary's default (no TTY → no color) would be
// plain text, and a palette change would slip through it silently.
var updateGolden = flag.Bool("update", false, "rewrite the .golden files from the current render")

// goldenRows is a fixed tmux server: two sessions, two windows. Deterministic
// by construction — no clock, no cwd, no environment.
func goldenRows(_ string, args []string) (string, error) {
	for _, a := range args {
		switch a {
		case "list-sessions":
			return strings.Join([]string{
				row("123", "456", "$1", "alpha", "2", "1", "1700000000", "/tmp/alpha"),
				row("123", "456", "$2", "beta", "1", "0", "1700000001", "/tmp/beta"),
			}, "\n"), nil
		case "list-windows":
			return strings.Join([]string{
				row("123", "456", "@1", "$1", "alpha", "0", "editor", "1", "2"),
				row("123", "456", "@2", "$1", "alpha", "1", "logs", "0", "1"),
			}, "\n"), nil
		}
	}
	return "", nil
}

func row(fields ...string) string { return strings.Join(fields, sep) }

func goldenModel(t *testing.T) model {
	t.Helper()
	fake := &exec.FakeRunner{RunFunc: goldenRows}
	return sized(newModel(context.Background(), tmux.New(fake), false), 80, 24)
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (capture it with `go test ./internal/tui -run Golden -update`): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("render drifted from %s\n--- want ---\n%q\n--- got ---\n%q", path, want, got)
	}
}

func TestGoldenMenu(t *testing.T) {
	forceTrueColor(t)
	assertGolden(t, "menu", goldenModel(t).View().Content)
}

func TestGoldenSessions(t *testing.T) {
	forceTrueColor(t)
	out, _ := goldenModel(t).Update(key("2"))
	assertGolden(t, "sessions", out.(model).View().Content)
}

func TestGoldenWindows(t *testing.T) {
	forceTrueColor(t)
	out, _ := goldenModel(t).Update(key("3"))
	assertGolden(t, "windows", out.(model).View().Content)
}
