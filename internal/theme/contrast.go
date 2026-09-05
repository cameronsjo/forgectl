package theme

import (
	"math"
	"strconv"
)

// Contrast returns the WCAG 2.x contrast ratio for r against the surface it is
// actually drawn on, in t's current mode.
//
// "Against Bg" is wrong for the on-fill roles and would make theme show cry
// wolf. OnAccent is ink, which sits on AccentFill and never on the page
// background: measured against Bg it reads 1.12:1, and against the fill it
// reads 5.65:1 — the number _palette.json's own $notes records for that pair.
// OnUrgent on UrgentFill is the same shape but NOT the same number: it
// measures 5.14:1. Reporting the against-Bg figure would flag two correct
// colours as failures and teach a reader to ignore the column.
//
// A malformed hex resolves to luminance 0 rather than panicking. It should not
// occur: a generated colour is validated by palettegen, and an override by
// theme.FromConfig — which is the check that actually runs at startup, since
// config.Load is tolerant and never calls config.ThemeConfig.Validate. Citing
// that one here would name a gate this path does not pass through.
func (t Theme) Contrast(r Role) float64 {
	return contrastRatio(t.Hex(r), t.Hex(t.contrastAgainst(r)))
}

// contrastAgainst names the role r is drawn on top of.
func (t Theme) contrastAgainst(r Role) Role {
	switch r {
	case RoleOnAccent:
		return RoleAccentFill
	case RoleOnUrgent:
		return RoleUrgentFill
	default:
		return RoleBg
	}
}

// CarriesText reports whether r is used as TEXT, and so whether its Contrast
// is owed the 4.5:1 AA floor at all.
//
// This follows _palette.json's own $notes.ruleUsageSetsRatio — "the contrast a
// colour MUST clear is a function of how it is USED, not the hue". A fill, a
// raised surface, and the background itself carry no text obligation, so
// flagging them below AA would be reporting a rule they were never under.
func CarriesText(r Role) bool {
	switch r {
	case RoleAccentFill, RoleUrgentFill, RoleSurfaceRaised, RoleBg:
		return false
	default:
		return true
	}
}

// contrastRatio is the WCAG 2.x ratio between two "#rrggbb" colours.
func contrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	lighter, darker := la, lb
	if darker > lighter {
		lighter, darker = darker, lighter
	}
	return (lighter + 0.05) / (darker + 0.05)
}

// relativeLuminance computes WCAG 2.x relative luminance for a "#rrggbb" hex
// string.
func relativeLuminance(hex string) float64 {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return 0
	}
	return 0.2126*srgbToLinear(r) + 0.7152*srgbToLinear(g) + 0.0722*srgbToLinear(b)
}

// srgbToLinear converts one sRGB channel (0..1) to linear light, per the WCAG
// 2.x relative luminance formula.
func srgbToLinear(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// parseHex parses a strict "#rrggbb" string into 0..1 channel values.
func parseHex(hex string) (r, g, b float64, ok bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, false
	}
	ri, err1 := strconv.ParseUint(hex[1:3], 16, 8)
	gi, err2 := strconv.ParseUint(hex[3:5], 16, 8)
	bi, err3 := strconv.ParseUint(hex[5:7], 16, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return float64(ri) / 255, float64(gi) / 255, float64(bi) / 255, true
}
