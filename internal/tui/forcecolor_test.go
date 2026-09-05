package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// forceTrueColor makes Style.Render emit truecolor SGR sequences inside the
// test binary, which has no TTY and would otherwise render everything plain.
//
// This is the one seam the charm.land/v2 migration is allowed to change: v1
// resolves the profile from a package global, while v2 moved the downgrade out
// of Render and into the writer, so Render always emits truecolor and this
// becomes a no-op. Both sides produce the same bytes, which is what lets the
// goldens carry across the swap.
func forceTrueColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}
