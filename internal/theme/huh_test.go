package theme

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestHuh_IgnoresItsArgument pins the deliberate behavior: huh v2 never
// requests the background for a standalone form, so the isDark argument
// ThemeFunc receives is meaningless — both calls must resolve identically,
// driven only by the receiver's own IsDark().
func TestHuh_IgnoresItsArgument(t *testing.T) {
	th := New(Options{}, true) // dark

	viaTrue := th.Huh().Theme(true)
	viaFalse := th.Huh().Theme(false)
	if viaTrue.Focused.Title.String() != viaFalse.Focused.Title.String() {
		t.Error("Huh() rendered different styles for Theme(true) vs Theme(false); it must ignore the argument")
	}
}

// TestHuh_ResolvesFromReceiverIsDark asserts WHICH colour each side rendered,
// not merely that the two differ.
//
// The earlier version of this test compared the two results and skipped when
// they matched — so it had no assertion at all and could not fail. Worse, the
// regression it is named for would have kept it green: if Huh() read the
// passed bool instead of the receiver, the two calls below would still produce
// two different colours, just swapped. Pinning each side to the receiver's own
// Accent is what makes the swap detectable.
func TestHuh_ResolvesFromReceiverIsDark(t *testing.T) {
	dark := New(Options{}, true)
	light := New(Options{}, false)

	// Each is deliberately passed the WRONG argument. A correct Huh() ignores
	// it and follows the receiver.
	gotDark := dark.Huh().Theme(false).Focused.Title.GetForeground()
	gotLight := light.Huh().Theme(true).Focused.Title.GetForeground()

	wantDark := dark.Color(RoleAccent)
	wantLight := light.Color(RoleAccent)

	if wantDark == wantLight {
		t.Fatal("dark and light Accent resolve to the same colour; this test cannot distinguish the two paths")
	}
	if gotDark != wantDark {
		t.Errorf("dark theme rendered %v, want its own accent %v — Huh() followed the argument, not the receiver", gotDark, wantDark)
	}
	if gotLight != wantLight {
		t.Errorf("light theme rendered %v, want its own accent %v — Huh() followed the argument, not the receiver", gotLight, wantLight)
	}
}

func TestHuh_ZeroValueRendersWithoutPanicking(t *testing.T) {
	var zero Theme
	s := zero.Huh().Theme(false)
	if s == nil {
		t.Fatal("Huh() on the zero Theme returned nil")
	}
}

// TestHuh_ButtonFocusIsMarkedInText pins that the focused confirm button is
// marked in the text itself. NO_COLOR strips the fill and a plain capture has
// no attributes, so colour alone left the selected button invisible (#1103).
// Both buttons must render the same width, or the row shifts on every toggle.
func TestHuh_ButtonFocusIsMarkedInText(t *testing.T) {
	s := New(Options{}, true).Huh().Theme(true)

	focused := s.Focused.FocusedButton.Render("Yes")
	blurred := s.Focused.BlurredButton.Render("Yes")

	if !strings.Contains(stripANSI(focused), "> Yes") {
		t.Errorf("focused button = %q, want a leading \"> \" marker", stripANSI(focused))
	}
	if strings.Contains(stripANSI(blurred), ">") {
		t.Errorf("blurred button = %q, must not carry the marker", stripANSI(blurred))
	}
	if lipgloss.Width(focused) != lipgloss.Width(blurred) {
		t.Errorf("focused width %d != blurred width %d; the row would shift", lipgloss.Width(focused), lipgloss.Width(blurred))
	}
}

func stripANSI(s string) string { return ansi.Strip(s) }
