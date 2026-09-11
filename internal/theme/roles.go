package theme

// Role identifies one semantic colour slot a Palette resolves. Roles are the
// unit every Palette, config override, and rendering adapter (Styles, Huh,
// Fang, List) speaks in — never a raw hex or a palette JSON key.
type Role int

// The seventeen roles, in the exact order config.ThemeRoleNames documents to
// an operator. RoleNames() mirrors this order and a test pins the two lists
// equal — reorder here only alongside that config list.
const (
	RoleAccent Role = iota
	RoleOK
	RoleDanger
	RoleWarn
	RoleActive
	RoleMuted
	RoleMeta
	RoleDim
	RoleFg
	RoleSteel
	RoleBrand
	RoleSurfaceRaised
	RoleAccentFill
	RoleOnAccent
	RoleUrgentFill
	RoleOnUrgent
	RoleBg
	numRoles
)

// roleNames is roleNames[r] for every Role r, lowercase — the exact spelling
// an operator writes as a [theme.colors] key and the wording ThemeConfig's
// unknown-role error joins.
var roleNames = [numRoles]string{
	"accent", "ok", "danger", "warn", "active", "muted", "meta", "dim", "fg", "steel",
	"brand", "surfaceraised", "accentfill", "onaccent", "urgentfill", "onurgent", "bg",
}

// RoleNames returns every role name, lowercase, in Role order. It must stay
// byte-for-byte equal to config.ThemeRoleNames — TestRoleNames_MatchConfig
// pins that.
func RoleNames() []string {
	out := make([]string, numRoles)
	copy(out, roleNames[:])
	return out
}

// roleByName resolves a lowercase role name back to its Role, for FromConfig
// mapping a [theme.colors] key onto the Overrides map.
func roleByName(name string) (Role, bool) {
	for i, n := range roleNames {
		if n == name {
			return Role(i), true
		}
	}
	return 0, false
}

// RoleSource names, for one Role, the _palette.json key (from artificer's
// "dark"/"light" blocks) that role resolves from when generating Artificer().
// This is the single source of truth palettegen reads — keep it in lockstep
// with the Role order above; a mismatch is a wrong colour in the generated
// file, not a compile error.
//
// Two roles intentionally repeat a key: Warn and Active both read "attention"
// (attention is a fill/border/dot hue that this CLI reuses for both, per
// _palette.json's own $notes.attentionNotTextRole carve-out for terminal log
// severity conventions). Role.Bg has no analogue in the source palette keys
// beyond "bg" itself — it exists so Theme.Contrast has something to measure
// every other role against.
type RoleSource struct {
	Role       Role
	PaletteKey string
}

// ArtificerRoleSources is RoleSource for every role, in Role order, mapping
// _palette.json's dark/light blocks onto Artificer()'s roles.
var ArtificerRoleSources = []RoleSource{
	{RoleAccent, "accent"},
	{RoleOK, "success"},
	{RoleDanger, "urgentText"},
	{RoleWarn, "attention"},
	{RoleActive, "attention"},
	{RoleMuted, "fgMuted"},
	{RoleMeta, "fgSecondary"},
	{RoleDim, "fgDisabled"},
	{RoleFg, "fg"},
	{RoleSteel, "steel"},
	{RoleBrand, "brandPurpleBright"},
	{RoleSurfaceRaised, "bgRaised"},
	{RoleAccentFill, "accentFill"},
	{RoleOnAccent, "ink"},
	{RoleUrgentFill, "urgent"},
	{RoleOnUrgent, "ivory"},
	{RoleBg, "bg"},
}

// PalettePath is the vendored Artificer palette, relative to this package's
// directory — the single path both palettegen (the generator) and
// TestArtificerGen_IsCurrent (the freshness test) read, so the two cannot
// silently disagree about which file backs Artificer().
const PalettePath = "artificer/_palette.json"
