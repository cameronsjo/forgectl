package theme

// Legacy hex values — every one of these is lifted verbatim from
// internal/tui/styles.go's pre-theme palette (lavender accent, Tomorrow-family
// semantics matched to the tmux status bar). Nothing here is newly designed.
const (
	legacyAccent = "#B0B9F9"
	legacyOK     = "#b5bd68"
	legacyDanger = "#cc6666"
	legacyActive = "#f0c674"
	legacyMuted  = "#666666"
	legacyFg     = "#c5c8c6"
	legacySteel  = "#8abeb7" // styles.go named this colorCyan; Steel is its role here

	// legacySurface, legacyOnAccent, and legacyBg have no source in
	// styles.go at all — the pre-theme CLI never painted a background or a
	// fill/on-fill pair, it only set foregrounds against whatever the
	// terminal already showed. These three are the one place Legacy()
	// invents rather than reuses: legacyBg is Tomorrow Night's own
	// background (the scheme styles.go's other seven hexes are drawn from),
	// so this is a scoped extension of the plan's "fill sensibly from that
	// set", not an unrelated addition.
	legacySurface  = "#282a2e" // Tomorrow Night bgHighlight, one step up from bg
	legacyOnAccent = "#1d1f21" // Tomorrow Night bg, used as dark ink on a light fill
	legacyBg       = "#1d1d1d" // Tomorrow Night bg
)

// Legacy is the pre-Artificer palette: today's internal/tui/styles.go hexes,
// single-mode (styles.go never had a light variant, so Dark and Light are
// identical for every role). It exists purely as the migration escape for
// [theme].preset = "legacy" — a designed palette this is not.
func Legacy() Palette {
	solid := func(hex string) Pair { return Pair{Dark: hex, Light: hex} }
	var p Palette
	p.Name = "Legacy"
	p.Roles[RoleAccent] = solid(legacyAccent)
	p.Roles[RoleOK] = solid(legacyOK)
	p.Roles[RoleDanger] = solid(legacyDanger)
	p.Roles[RoleWarn] = solid(legacyActive) // styles.go never distinguished warn from active
	p.Roles[RoleActive] = solid(legacyActive)
	p.Roles[RoleMuted] = solid(legacyMuted)
	p.Roles[RoleMeta] = solid(legacyMuted)
	p.Roles[RoleDim] = solid(legacyMuted)
	p.Roles[RoleFg] = solid(legacyFg)
	p.Roles[RoleSteel] = solid(legacySteel)
	p.Roles[RoleBrand] = solid(legacyAccent)
	p.Roles[RoleSurfaceRaised] = solid(legacySurface)
	p.Roles[RoleAccentFill] = solid(legacyAccent)
	p.Roles[RoleOnAccent] = solid(legacyOnAccent)
	p.Roles[RoleUrgentFill] = solid(legacyDanger)
	p.Roles[RoleOnUrgent] = solid(legacyFg)
	p.Roles[RoleBg] = solid(legacyBg)
	return p
}
