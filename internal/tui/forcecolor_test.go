package tui

import "testing"

// forceTrueColor makes Style.Render emit truecolor SGR sequences inside the
// test binary, which has no TTY and would otherwise render everything plain.
//
// This is the one seam the charm.land/v2 migration was allowed to change, and
// on v2 it is a no-op by design. v1 resolved the colour profile from a package
// global that Render consulted, so a test binary needed lipgloss.SetColorProfile
// to get colour at all. v2 moved the downgrade out of Render and into
// colorprofile.Writer: Render now always emits truecolor and there is no global
// to set. Both sides produce the same bytes, which is what lets the goldens
// captured on v1 carry across the swap unchanged.
//
// It stays as a function rather than being deleted at the call sites so the
// goldens keep saying out loud that they depend on truecolor rendering — if a
// future change reintroduces a profile global, this is where it gets set.
func forceTrueColor(t *testing.T) { t.Helper() }
