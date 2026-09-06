package theme

import (
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestRoleNames_MatchConfig pins the contract Task 3 already shipped:
// config.ThemeRoleNames is the operator-facing list, and this package's
// RoleNames() must agree with it byte-for-byte and in the same order.
func TestRoleNames_MatchConfig(t *testing.T) {
	if !reflect.DeepEqual(RoleNames(), config.ThemeRoleNames) {
		t.Errorf("theme.RoleNames() = %v, want config.ThemeRoleNames = %v", RoleNames(), config.ThemeRoleNames)
	}
}

func TestRoleNames_LengthMatchesNumRoles(t *testing.T) {
	if got, want := len(RoleNames()), int(numRoles); got != want {
		t.Errorf("len(RoleNames()) = %d, want numRoles = %d", got, want)
	}
}

func TestArtificerRoleSources_CoverEveryRoleExactlyOnce(t *testing.T) {
	seen := make(map[Role]bool, numRoles)
	for _, src := range ArtificerRoleSources {
		if seen[src.Role] {
			t.Errorf("role %v appears twice in ArtificerRoleSources", src.Role)
		}
		seen[src.Role] = true
		if src.PaletteKey == "" {
			t.Errorf("role %v has an empty PaletteKey", src.Role)
		}
	}
	if len(seen) != int(numRoles) {
		t.Errorf("ArtificerRoleSources covers %d roles, want %d", len(seen), numRoles)
	}
}

func TestRoleByName_CaseAndUnknown(t *testing.T) {
	if r, ok := roleByName("accent"); !ok || r != RoleAccent {
		t.Errorf("roleByName(accent) = %v, %v, want RoleAccent, true", r, ok)
	}
	if _, ok := roleByName("nonexistent"); ok {
		t.Error("roleByName(nonexistent) reported ok, want false")
	}
}
