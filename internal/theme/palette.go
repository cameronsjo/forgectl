package theme

// Pair is one role's colour in both terminal backgrounds.
type Pair struct {
	Dark  string
	Light string
}

// Palette is a full set of role colours — either a generated Artificer()
// preset or the hand-written Legacy() escape hatch.
type Palette struct {
	Name  string
	Roles [numRoles]Pair
}

// hex returns the Pair for r, resolved to isDark.
func (p Palette) hex(r Role, isDark bool) string {
	pair := p.Roles[r]
	if isDark {
		return pair.Dark
	}
	return pair.Light
}

// withOverrides returns a copy of p with every override in overrides applied.
// An override missing Dark or Light leaves that mode's existing colour alone
// — ColorOverride's whole point is "no override for that mode", not "blank
// it out".
func (p Palette) withOverrides(overrides map[Role]Pair) Palette {
	if len(overrides) == 0 {
		return p
	}
	out := p
	for r, ov := range overrides {
		if int(r) < 0 || int(r) >= int(numRoles) {
			continue
		}
		pair := out.Roles[r]
		if ov.Dark != "" {
			pair.Dark = ov.Dark
		}
		if ov.Light != "" {
			pair.Light = ov.Light
		}
		out.Roles[r] = pair
	}
	return out
}
