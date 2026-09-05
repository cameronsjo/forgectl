package theme

import (
	"math"
	"strconv"
)

// Contrast returns the WCAG 2.x relative-contrast ratio between r and Bg, in
// t's current mode — the number theme show compares against the 4.5:1 AA
// text floor. A malformed hex (which should not occur — every colour reaching
// here was either generated from the vendored palette or validated by
// config.ThemeConfig.Validate) resolves to a luminance of 0 rather than
// panicking.
func (t Theme) Contrast(r Role) float64 {
	fg := relativeLuminance(t.Hex(r))
	bg := relativeLuminance(t.Hex(RoleBg))
	lighter, darker := fg, bg
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
