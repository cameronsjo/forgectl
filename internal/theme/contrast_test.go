package theme

import "testing"

func TestContrast_BlackOnWhiteIs21To1(t *testing.T) {
	th := New(Options{Overrides: map[Role]Pair{
		RoleBg:     {Dark: "#ffffff"},
		RoleAccent: {Dark: "#000000"},
	}}, true)

	const want = 21.0
	const tolerance = 0.01
	c := th.Contrast(RoleAccent)
	if diff := c - want; diff > tolerance || diff < -tolerance {
		t.Errorf("Contrast(black on white) = %v, want %v (+/- %v)", c, want, tolerance)
	}
}

func TestContrast_SameColorIs1To1(t *testing.T) {
	th := New(Options{Overrides: map[Role]Pair{
		RoleBg:     {Dark: "#808080"},
		RoleAccent: {Dark: "#808080"},
	}}, true)
	if c := th.Contrast(RoleAccent); c < 0.999 || c > 1.001 {
		t.Errorf("Contrast(same colour) = %v, want ~1.0", c)
	}
}

func TestContrast_MalformedHexDoesNotPanic(t *testing.T) {
	th := New(Options{Overrides: map[Role]Pair{
		RoleAccent: {Dark: "not-a-color"},
	}}, true)
	_ = th.Contrast(RoleAccent) // must not panic
}
