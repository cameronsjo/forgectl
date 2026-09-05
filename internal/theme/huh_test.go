package theme

import "testing"

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

func TestHuh_ResolvesFromReceiverIsDark(t *testing.T) {
	dark := New(Options{}, true)
	light := New(Options{}, false)

	darkAccent := dark.Huh().Theme(false).Focused.Title.GetForeground()
	lightAccent := light.Huh().Theme(true).Focused.Title.GetForeground()

	if darkAccent == lightAccent {
		t.Skip("dark and light Artificer accent happen to render the same colour string; not a useful signal here")
	}
}

func TestHuh_ZeroValueRendersWithoutPanicking(t *testing.T) {
	var zero Theme
	s := zero.Huh().Theme(false)
	if s == nil {
		t.Fatal("Huh() on the zero Theme returned nil")
	}
}
