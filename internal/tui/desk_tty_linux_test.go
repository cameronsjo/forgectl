//go:build linux

package tui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// util-linux script(1) starts the command with $SHELL -c, so the TTY run
// pins SHELL to bash, which reads no startup file when non-interactive.
func TestTTYEnvPinsShellToBash(t *testing.T) {
	got := ttyEnv([]string{"PATH=/bin", "SHELL=/usr/bin/zsh", "HOME=/h"})
	want := []string{"PATH=/bin", "HOME=/h", "SHELL=/bin/bash"}
	if !slices.Equal(got, want) {
		t.Fatalf("ttyEnv = %q, want %q", got, want)
	}
}

// The TTY run hands script(1) the pinned shell, not the operator's.
func TestTTYRunUsesTheLinuxTTYEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/usr/bin/zsh")
	h := newDeskHarness(t)
	h.drop("01-console.sh", ttyScript("console"))
	h.scan()
	snap, err := h.d.Scan()
	if err != nil || len(snap.Pending) != 1 {
		t.Fatalf("scan: %v, %d pending", err, len(snap.Pending))
	}
	c, err := h.backend.Claim("01-console", snap.Pending[0].Meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	rcPath := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rcPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := newTTYRun(h.backend, c, rcPath, func(string) []string { return []string{"/bin/true"} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.finish(nil) })
	if !slices.Contains(r.cmd.Env, "SHELL=/bin/bash") || slices.Contains(r.cmd.Env, "SHELL=/usr/bin/zsh") {
		t.Fatalf("env = %q; want SHELL pinned to /bin/bash", r.cmd.Env)
	}
}
