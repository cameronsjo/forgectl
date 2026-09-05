package theme

import "fmt"

// String renders a Mode as the string an operator writes in [theme].mode, so
// `theme show` and an error message name the same spelling the config takes.
func (m Mode) String() string {
	switch m {
	case ModeDark:
		return "dark"
	case ModeLight:
		return "light"
	default:
		return "auto"
	}
}

// PresetName is the preset this Theme resolves from, spelled as [theme].preset
// takes it. An empty Options.Preset means the default, which is reported by its
// name rather than as "" — "the default" is not something an operator can look
// up, and `theme show` exists to be looked up.
func (t Theme) PresetName() string {
	if t.opts.Preset == "legacy" {
		return "legacy"
	}
	return "artificer"
}

// Provenance says where r's current value came from: the preset, or an
// override from [theme.colors]. It is per-role and per-mode, because an
// override may set only one mode — a role overridden dark-only is still
// showing the preset's colour in light, and reporting it as "override" there
// would be a lie in exactly the case a reader is trying to debug.
func (t Theme) Provenance(r Role) string {
	if pair, ok := t.opts.Overrides[r]; ok {
		if t.IsDark() && pair.Dark != "" {
			return "override"
		}
		if !t.IsDark() && pair.Light != "" {
			return "override"
		}
	}
	return t.PresetName()
}

// Warnings reports conditions a reader of `theme show` should know about but
// which are not errors — things forgectl accepted and then did not use.
//
// The load-bearing case is an override that names a mode this Theme is not
// resolved to. It is silently inert, it looks correct in the config file, and
// without this it is invisible: the operator sees their hex in config.toml and
// the preset's hex on screen, with nothing connecting the two.
func (t Theme) Warnings() []string {
	var out []string
	names := RoleNames()
	for i := range numRoles {
		r := Role(i)
		pair, ok := t.opts.Overrides[r]
		if !ok {
			continue
		}
		if t.IsDark() && pair.Dark == "" && pair.Light != "" {
			out = append(out, fmt.Sprintf(
				"[theme.colors].%s sets only light, but the theme resolved dark — the override is not in effect", names[r]))
		}
		if !t.IsDark() && pair.Light == "" && pair.Dark != "" {
			out = append(out, fmt.Sprintf(
				"[theme.colors].%s sets only dark, but the theme resolved light — the override is not in effect", names[r]))
		}
	}
	return out
}
