package preflight

// Test plan for align.go
//
// Diff (Classification: pure)
//   [x] Happy: aligned when current already equals target
//   [x] Happy: target-only true key → Enable
//   [x] Happy: current-only true key → Disable
//   [x] Edge: target has a key explicitly false → Disable if current has it true
//   [x] Edge: a key both false in current is not disabled again
//
// Target (Classification: pure)
//   [x] Happy: committed project entries fold in alongside catalog core
//   [x] Happy: a committed entry OVERRIDES the catalog default for the same key
//   [x] Happy: nil committedProject leaves the catalog core set untouched
//
// FilterMarketplaces (Classification: pure — the marketplace-injection fix)
//   [x] Happy: a target plugin whose marketplace IS trusted → its source lands in marketplaces
//   [x] Sad: a target plugin whose marketplace is NOT trusted → key lands in unregistered, nothing written
//   [x] Edge: a target key false is skipped entirely (not enabled, not unregistered)
//   [x] Edge: two target plugins sharing one trusted marketplace both resolve, one entry

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDiff_Aligned(t *testing.T) {
	current := map[string]bool{"a@m": true}
	target := map[string]bool{"a@m": true}

	got := Diff(current, target)
	if !got.Aligned() {
		t.Errorf("Diff() = %+v, want Aligned()", got)
	}
}

func TestDiff_EnableAndDisable(t *testing.T) {
	current := map[string]bool{"a@m": true, "b@m": true}
	target := map[string]bool{"a@m": true, "c@m": true}

	got := Diff(current, target)
	if !reflect.DeepEqual(got.Enable, []string{"c@m"}) {
		t.Errorf("Enable = %v, want [c@m]", got.Enable)
	}
	if !reflect.DeepEqual(got.Disable, []string{"b@m"}) {
		t.Errorf("Disable = %v, want [b@m]", got.Disable)
	}
}

func TestDiff_TargetExplicitFalseDisables(t *testing.T) {
	current := map[string]bool{"a@m": true}
	target := map[string]bool{"a@m": false}

	got := Diff(current, target)
	if !reflect.DeepEqual(got.Disable, []string{"a@m"}) {
		t.Errorf("Disable = %v, want [a@m]", got.Disable)
	}
	if len(got.Enable) != 0 {
		t.Errorf("Enable = %v, want none", got.Enable)
	}
}

func TestDiff_BothFalseIsNotDisabled(t *testing.T) {
	current := map[string]bool{"a@m": false}
	target := map[string]bool{"a@m": false}

	got := Diff(current, target)
	if !got.Aligned() {
		t.Errorf("Diff() = %+v, want Aligned() when both sides agree a@m is false", got)
	}
}

func TestTarget_FoldsInCommittedProject(t *testing.T) {
	core := map[string]bool{"cadence@workbench": true}
	committed := map[string]bool{"project-only@workbench": true}

	got := Target(core, committed)
	want := map[string]bool{"cadence@workbench": true, "project-only@workbench": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Target() = %v, want %v", got, want)
	}
}

func TestTarget_CommittedOverridesCatalogDefault(t *testing.T) {
	core := map[string]bool{"cadence@workbench": true}
	committed := map[string]bool{"cadence@workbench": false}

	got := Target(core, committed)
	if got["cadence@workbench"] {
		t.Errorf("Target()[cadence@workbench] = true, want the committed override (false) to win")
	}
}

func TestTarget_NilCommittedLeavesCoreUntouched(t *testing.T) {
	core := map[string]bool{"cadence@workbench": true}

	got := Target(core, nil)
	want := map[string]bool{"cadence@workbench": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Target() = %v, want %v", got, want)
	}
}

func TestFilterMarketplaces_TrustedResolves(t *testing.T) {
	target := map[string]bool{"cadence@workbench": true}
	trusted := map[string]json.RawMessage{"workbench": json.RawMessage(`{"source":"github"}`)}

	marketplaces, unregistered := FilterMarketplaces(target, trusted)
	if len(unregistered) != 0 {
		t.Errorf("unregistered = %v, want none", unregistered)
	}
	if _, ok := marketplaces["workbench"]; !ok {
		t.Errorf("marketplaces = %v, want workbench present", marketplaces)
	}
}

func TestFilterMarketplaces_UntrustedIsUnregistered(t *testing.T) {
	target := map[string]bool{"evilplugin@evil-marketplace": true}
	trusted := map[string]json.RawMessage{"workbench": json.RawMessage(`{}`)}

	marketplaces, unregistered := FilterMarketplaces(target, trusted)
	if len(marketplaces) != 0 {
		t.Errorf("marketplaces = %v, want none written for an untrusted marketplace", marketplaces)
	}
	if !reflect.DeepEqual(unregistered, []string{"evilplugin@evil-marketplace"}) {
		t.Errorf("unregistered = %v, want [evilplugin@evil-marketplace]", unregistered)
	}
}

func TestFilterMarketplaces_FalseTargetSkipped(t *testing.T) {
	target := map[string]bool{"cadence@workbench": false}
	trusted := map[string]json.RawMessage{}

	marketplaces, unregistered := FilterMarketplaces(target, trusted)
	if len(marketplaces) != 0 || len(unregistered) != 0 {
		t.Errorf("marketplaces=%v unregistered=%v, want both empty for a false target entry", marketplaces, unregistered)
	}
}

func TestFilterMarketplaces_SharedMarketplaceOneEntry(t *testing.T) {
	target := map[string]bool{"cadence@workbench": true, "cadence-voice@workbench": true}
	trusted := map[string]json.RawMessage{"workbench": json.RawMessage(`{"source":"github"}`)}

	marketplaces, unregistered := FilterMarketplaces(target, trusted)
	if len(unregistered) != 0 {
		t.Errorf("unregistered = %v, want none", unregistered)
	}
	if len(marketplaces) != 1 {
		t.Errorf("marketplaces = %v, want exactly one entry (workbench, shared by both plugins)", marketplaces)
	}
}
