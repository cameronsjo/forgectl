package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/fang"
)

// Fang returns a fang.ColorSchemeFunc that ignores the lipgloss.LightDarkFunc
// fang hands it and resolves every field from t.IsDark() instead — the same
// reasoning as Huh(): fang's own dark-detection runs a single process-wide
// probe at startup (mustColorscheme), which this Theme has already done (or
// been told the answer to) more carefully via Detect.
func (t Theme) Fang() fang.ColorSchemeFunc {
	return func(lipgloss.LightDarkFunc) fang.ColorScheme {
		return fang.ColorScheme{
			Base:           t.Color(RoleFg),
			Title:          t.Color(RoleAccent),
			Description:    t.Color(RoleMeta),
			Codeblock:      t.Color(RoleSurfaceRaised),
			Program:        t.Color(RoleAccent),
			DimmedArgument: t.Color(RoleMuted),
			Comment:        t.Color(RoleMuted),
			Flag:           t.Color(RoleSteel),
			FlagDefault:    t.Color(RoleMeta),
			Command:        t.Color(RoleAccent),
			QuotedString:   t.Color(RoleSteel),
			Argument:       t.Color(RoleFg),
			Help:           t.Color(RoleMeta),
			Dash:           t.Color(RoleMuted),
			ErrorHeader:    [2]color.Color{t.Color(RoleOnUrgent), t.Color(RoleUrgentFill)},
			ErrorDetails:   t.Color(RoleFg),
		}
	}
}
