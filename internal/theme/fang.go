package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/fang"
)

// Sentinels handed to fang's lipgloss.LightDarkFunc to read back which side it
// chose. They must be distinct and comparable; the values are never rendered.
var (
	fangLightSentinel color.Color = color.RGBA{}
	fangDarkSentinel  color.Color = color.RGBA{R: 1, G: 1, B: 1, A: 1}
)

// Fang returns a fang.ColorSchemeFunc drawing every field from t.
//
// The LightDarkFunc fang hands in is a detection result only sometimes. Fang
// v1.0.0 (theme.go, mustColorscheme) probes the terminal background only when
// os.Stdout is a TTY, via lipgloss.HasDarkBackground; otherwise it passes
// lipgloss.LightDark(false), which is a placeholder and not a measurement. So
// the caller says whether to trust it (trustProbe): when true, t is in
// ModeAuto and ld is non-nil, the background fang chose wins over t.IsDark().
// An explicit dark or light Mode always wins, and a nil ld falls back to
// t.IsDark(). Re-check that gate whenever fang is bumped.
func (t Theme) Fang(trustProbe bool) fang.ColorSchemeFunc {
	return func(ld lipgloss.LightDarkFunc) fang.ColorScheme {
		th := t
		if ld != nil && trustProbe && t.Mode() == ModeAuto {
			th = t.WithDark(ld(fangLightSentinel, fangDarkSentinel) == fangDarkSentinel)
		}
		return fang.ColorScheme{
			Base:           th.Color(RoleFg),
			Title:          th.Color(RoleAccent),
			Description:    th.Color(RoleMeta),
			Codeblock:      th.Color(RoleSurfaceRaised),
			Program:        th.Color(RoleAccent),
			DimmedArgument: th.Color(RoleMuted),
			Comment:        th.Color(RoleMuted),
			Flag:           th.Color(RoleSteel),
			FlagDefault:    th.Color(RoleMeta),
			Command:        th.Color(RoleAccent),
			QuotedString:   th.Color(RoleSteel),
			Argument:       th.Color(RoleFg),
			Help:           th.Color(RoleMeta),
			Dash:           th.Color(RoleMuted),
			ErrorHeader:    [2]color.Color{th.Color(RoleOnUrgent), th.Color(RoleUrgentFill)},
			ErrorDetails:   th.Color(RoleFg),
		}
	}
}
