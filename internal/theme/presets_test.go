package theme

import "testing"

func TestLegacy_MatchesStylesGoHexes(t *testing.T) {
	p := Legacy()
	tests := []struct {
		role Role
		want string
	}{
		{RoleAccent, "#B0B9F9"},
		{RoleOK, "#b5bd68"},
		{RoleDanger, "#cc6666"},
		{RoleActive, "#f0c674"},
		{RoleMuted, "#666666"},
		{RoleFg, "#c5c8c6"},
		{RoleSteel, "#8abeb7"},
	}
	for _, tt := range tests {
		if got := p.Roles[tt.role].Dark; got != tt.want {
			t.Errorf("Legacy().Roles[%v].Dark = %q, want %q", tt.role, got, tt.want)
		}
		if got := p.Roles[tt.role].Light; got != tt.want {
			t.Errorf("Legacy().Roles[%v].Light = %q, want %q (single-mode palette)", tt.role, got, tt.want)
		}
	}
}

func TestLegacy_EveryRoleHasBothColors(t *testing.T) {
	p := Legacy()
	for i, pair := range p.Roles {
		if pair.Dark == "" || pair.Light == "" {
			t.Errorf("Legacy().Roles[%d] = %+v, has an empty hex", i, pair)
		}
	}
}

func TestArtificer_EveryRoleHasBothColors(t *testing.T) {
	p := Artificer()
	for i, pair := range p.Roles {
		if pair.Dark == "" || pair.Light == "" {
			t.Errorf("Artificer().Roles[%d] = %+v, has an empty hex", i, pair)
		}
	}
}
