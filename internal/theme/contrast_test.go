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

// TestContrast_MeasuresAgainstTheRightSurface pins that an on-fill role is
// measured against its FILL, not the page background.
//
// The numbers are the palette's own: $notes.v0.19.0 records ink at 5.65:1 on
// accentFill, which is why light onAccent moved from ivory to ink. Against bg
// the same colour reads about 1.1 — a "failure" that would be reported for a
// pairing nothing ever renders, teaching a reader to ignore the column.
func TestContrast_MeasuresAgainstTheRightSurface(t *testing.T) {
	th := New(Options{}, true)

	onAccent := th.Contrast(RoleOnAccent)
	if onAccent < 5.0 || onAccent > 6.0 {
		t.Errorf("OnAccent contrast = %.2f, want ~5.65 (ink on accentFill, per the palette's $notes); "+
			"a value near 1.1 means it is being measured against Bg", onAccent)
	}

	// The wrong surface, computed explicitly, must be materially different —
	// otherwise the assertion above could pass for the wrong reason.
	againstBg := contrastRatio(th.Hex(RoleOnAccent), th.Hex(RoleBg))
	if againstBg > 2.0 {
		t.Errorf("OnAccent against Bg = %.2f; expected a poor ratio, so this test is not distinguishing the two surfaces", againstBg)
	}

	// An ordinary text role still measures against the background.
	if got, want := th.Contrast(RoleFg), contrastRatio(th.Hex(RoleFg), th.Hex(RoleBg)); got != want {
		t.Errorf("Fg contrast = %v, want %v (Fg must measure against Bg)", got, want)
	}
}

// TestCarriesText_ExemptsSurfaces pins which roles are owed the 4.5:1 text
// floor at all — the palette's ruleUsageSetsRatio: a fill carries no
// text-contrast obligation, so flagging it would report a rule it is not under.
func TestCarriesText_ExemptsSurfaces(t *testing.T) {
	for _, r := range []Role{RoleAccentFill, RoleUrgentFill, RoleSurfaceRaised, RoleBg} {
		if CarriesText(r) {
			t.Errorf("%s is a surface/fill; it must not be held to the text floor", RoleNames()[r])
		}
	}
	for _, r := range []Role{RoleFg, RoleAccent, RoleDanger, RoleOnAccent, RoleOnUrgent} {
		if !CarriesText(r) {
			t.Errorf("%s is drawn as text; it must be held to the text floor", RoleNames()[r])
		}
	}
}
