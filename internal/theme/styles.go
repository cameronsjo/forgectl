package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Color returns r's resolved colour as a color.Color, ready for
// lipgloss.NewStyle().Foreground/Background or any other color.Color
// consumer (fang.ColorScheme, image/color-based rendering).
func (t Theme) Color(r Role) color.Color {
	return lipgloss.Color(t.Hex(r))
}

// Style returns a bare lipgloss.Style with r as its foreground — the building
// block Styles() composes into named, ready-to-use styles.
func (t Theme) Style(r Role) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Color(r))
}

// Styles is the named, ready-to-render style set every screen composes from
// — the theme-aware replacement for internal/tui/styles.go's package-level
// style vars.
type Styles struct {
	Header   lipgloss.Style
	Accent   lipgloss.Style
	Selected lipgloss.Style
	OK       lipgloss.Style
	Warn     lipgloss.Style
	Danger   lipgloss.Style
	Active   lipgloss.Style
	Muted    lipgloss.Style
	Meta     lipgloss.Style
	Dim      lipgloss.Style
	Steel    lipgloss.Style
	Fg       lipgloss.Style
	Brand    lipgloss.Style
}

// Styles builds the named style set for t's current mode.
func (t Theme) Styles() Styles {
	return Styles{
		Header:   t.Style(RoleAccent).Bold(true),
		Accent:   t.Style(RoleAccent),
		Selected: t.Style(RoleAccent).Bold(true),
		OK:       t.Style(RoleOK),
		Warn:     t.Style(RoleWarn),
		Danger:   t.Style(RoleDanger).Bold(true),
		Active:   t.Style(RoleActive),
		Muted:    t.Style(RoleMuted),
		Meta:     t.Style(RoleMeta),
		Dim:      t.Style(RoleDim),
		Steel:    t.Style(RoleSteel),
		Fg:       t.Style(RoleFg),
		Brand:    t.Style(RoleBrand),
	}
}

// Marks is the pre-rendered status glyph vocabulary — one styled string per
// outcome, so a call site never re-derives "which colour is success" itself.
type Marks struct {
	OK   string
	Warn string
	Fail string
	Skip string
}

// Marks renders the four status glyphs in t's current mode.
func (t Theme) Marks() Marks {
	return Marks{
		OK:   t.Style(RoleOK).Render("✓"),
		Warn: t.Style(RoleWarn).Render("!"),
		Fail: t.Style(RoleDanger).Render("✗"),
		Skip: t.Style(RoleDim).Render("-"),
	}
}
