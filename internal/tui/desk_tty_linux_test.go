//go:build linux

package tui

import (
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
