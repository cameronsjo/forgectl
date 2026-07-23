package preflight

// Test plan for settings.go
//
// ReadDocument (Classification: I/O — temp-dir fixtures)
//   [x] Happy: a file declaring both keys decodes them
//   [x] Edge: a missing file yields a Present:false zero Document, not an error
//   [x] Edge: a present file declaring neither key yields Present:true with
//       both maps nil
//
// EffectiveEnabled / EffectiveMarketplaces (Classification: pure precedence)
//   [x] Happy: local wins outright over project and user
//   [x] Happy: project wins over user when local is absent
//   [x] Happy: user is the floor when neither project nor local is present
//   [x] Edge: none present → empty, non-nil map
//
// WriteLocal (Classification: I/O — RMW round-trip)
//   [x] Happy: fresh file — enabledPlugins + extraKnownMarketplaces land, dir created
//   [x] Happy: existing file's unrelated keys (permissions) survive untouched
//   [x] Happy: existing file's OWN enabledPlugins/extraKnownMarketplaces are replaced, not merged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDocument_Present(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{"enabledPlugins":{"cadence@workbench":true},"extraKnownMarketplaces":{"workbench":{"source":{"source":"github"}}},"other":"ignored"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, err := ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument: %v", err)
	}
	if !doc.Present {
		t.Fatal("Present = false, want true")
	}
	if !doc.EnabledPlugins["cadence@workbench"] {
		t.Errorf("EnabledPlugins = %v, want cadence@workbench=true", doc.EnabledPlugins)
	}
	if _, ok := doc.Marketplaces["workbench"]; !ok {
		t.Errorf("Marketplaces = %v, want a workbench entry", doc.Marketplaces)
	}
}

func TestReadDocument_Missing(t *testing.T) {
	doc, err := ReadDocument(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("ReadDocument: %v", err)
	}
	if doc.Present {
		t.Error("Present = true, want false for a missing file")
	}
	if doc.EnabledPlugins != nil {
		t.Error("EnabledPlugins != nil for a missing file")
	}
}

func TestReadDocument_PresentButNeitherKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, err := ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument: %v", err)
	}
	if !doc.Present {
		t.Error("Present = false, want true")
	}
	if doc.EnabledPlugins != nil {
		t.Errorf("EnabledPlugins = %v, want nil when the key is absent", doc.EnabledPlugins)
	}
}

func TestEffectiveEnabled_LocalWinsOutright(t *testing.T) {
	user := Document{Present: true, EnabledPlugins: map[string]bool{"a@m": true, "b@m": true}}
	project := Document{Present: true, EnabledPlugins: map[string]bool{"c@m": true}}
	local := Document{Present: true, EnabledPlugins: map[string]bool{"d@m": true}}

	got := EffectiveEnabled(user, project, local)
	if len(got) != 1 || !got["d@m"] {
		t.Errorf("EffectiveEnabled() = %v, want exactly local's set (replace, not merge)", got)
	}
}

func TestEffectiveEnabled_ProjectWinsOverUserWhenLocalAbsent(t *testing.T) {
	user := Document{Present: true, EnabledPlugins: map[string]bool{"a@m": true}}
	project := Document{Present: true, EnabledPlugins: map[string]bool{"c@m": true}}
	local := Document{}

	got := EffectiveEnabled(user, project, local)
	if len(got) != 1 || !got["c@m"] {
		t.Errorf("EffectiveEnabled() = %v, want project's set", got)
	}
}

func TestEffectiveEnabled_UserIsTheFloor(t *testing.T) {
	user := Document{Present: true, EnabledPlugins: map[string]bool{"a@m": true}}

	got := EffectiveEnabled(user, Document{}, Document{})
	if len(got) != 1 || !got["a@m"] {
		t.Errorf("EffectiveEnabled() = %v, want user's set", got)
	}
}

func TestEffectiveEnabled_NonePresent(t *testing.T) {
	got := EffectiveEnabled(Document{}, Document{}, Document{})
	if got == nil {
		t.Fatal("EffectiveEnabled() = nil, want an empty non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("EffectiveEnabled() = %v, want empty", got)
	}
}

func TestWriteLocal_FreshFile(t *testing.T) {
	dir := t.TempDir()
	enabled := map[string]bool{"cadence@workbench": true}
	marketplaces := map[string]json.RawMessage{"workbench": json.RawMessage(`{"source":{"source":"github"}}`)}

	path, err := WriteLocal(dir, enabled, marketplaces)
	if err != nil {
		t.Fatalf("WriteLocal: %v", err)
	}
	if path != LocalPath(dir) {
		t.Errorf("WriteLocal() path = %q, want %q", path, LocalPath(dir))
	}

	doc, err := ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument after WriteLocal: %v", err)
	}
	if !doc.EnabledPlugins["cadence@workbench"] {
		t.Errorf("round-tripped EnabledPlugins = %v", doc.EnabledPlugins)
	}
	if _, ok := doc.Marketplaces["workbench"]; !ok {
		t.Errorf("round-tripped Marketplaces = %v", doc.Marketplaces)
	}
}

func TestWriteLocal_PreservesUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{"permissions":{"allow":["Read"]},"enabledPlugins":{"stale@workbench":true}}`
	if err := os.WriteFile(LocalPath(dir), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := WriteLocal(dir, map[string]bool{"cadence@workbench": true}, nil)
	if err != nil {
		t.Fatalf("WriteLocal: %v", err)
	}

	raw, err := os.ReadFile(LocalPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["permissions"]; !ok {
		t.Error("WriteLocal dropped the unrelated \"permissions\" key")
	}

	doc, err := ReadDocument(LocalPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if doc.EnabledPlugins["stale@workbench"] {
		t.Error("WriteLocal merged instead of replacing enabledPlugins — stale key survived")
	}
	if !doc.EnabledPlugins["cadence@workbench"] {
		t.Errorf("WriteLocal did not write the new enabledPlugins set: %v", doc.EnabledPlugins)
	}
}
