package theme

import "testing"

func TestPalette_WithOverrides_LeavesUnsetModeAlone(t *testing.T) {
	base := Palette{Roles: [numRoles]Pair{RoleAccent: {Dark: "#111111", Light: "#222222"}}}
	got := base.withOverrides(map[Role]Pair{RoleAccent: {Dark: "#333333"}})
	if got.Roles[RoleAccent].Dark != "#333333" {
		t.Errorf("Dark = %q, want overridden #333333", got.Roles[RoleAccent].Dark)
	}
	if got.Roles[RoleAccent].Light != "#222222" {
		t.Errorf("Light = %q, want the original #222222 (override left it alone)", got.Roles[RoleAccent].Light)
	}
}

func TestPalette_WithOverrides_EmptyMapIsNoop(t *testing.T) {
	base := Palette{Name: "x", Roles: [numRoles]Pair{RoleAccent: {Dark: "#111111"}}}
	got := base.withOverrides(nil)
	if got != base {
		t.Errorf("withOverrides(nil) = %+v, want the original palette unchanged", got)
	}
}

func TestPalette_Hex(t *testing.T) {
	p := Palette{Roles: [numRoles]Pair{RoleAccent: {Dark: "#111111", Light: "#222222"}}}
	if got := p.hex(RoleAccent, true); got != "#111111" {
		t.Errorf("hex(dark) = %q", got)
	}
	if got := p.hex(RoleAccent, false); got != "#222222" {
		t.Errorf("hex(light) = %q", got)
	}
}
