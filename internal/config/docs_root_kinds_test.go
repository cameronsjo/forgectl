package config

// Test plan for DocsConfig.RootKinds (config.go)
//
// DocsConfig.RootKinds (Classification: TOML decode + IsZero coverage)
//   [x] Happy: [docs.root_kinds] decodes into a map keyed by root path
//   [x] Happy: IsZero is false when only root_kinds is set (roots/addr empty)
//   [x] Happy: IsZero is true when the [docs] section is fully absent
//   [x] Unhappy: Validate rejects an unknown value, naming the key, and
//       reports the alphabetically first bad key when several are bad
//   [x] Unhappy: ValidatePath surfaces that error for `launch doctor`

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDocsConfig_RootKinds_Decodes(t *testing.T) {
	body := "[docs]\nroots = [\"/tmp/extra\"]\n" +
		"[docs.root_kinds]\n\"/tmp/extra\" = \"vault\"\n\"/tmp/other\" = \"docs\"\n"

	got, err := DecodeStrict([]byte(body))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	want := map[string]string{"/tmp/extra": "vault", "/tmp/other": "docs"}
	if !reflect.DeepEqual(got.Docs.RootKinds, want) {
		t.Errorf("Docs.RootKinds = %v, want %v", got.Docs.RootKinds, want)
	}
}

func TestDocsConfig_IsZero_FalseWhenOnlyRootKindsSet(t *testing.T) {
	dc := DocsConfig{RootKinds: map[string]string{"/tmp/x": "vault"}}
	if dc.IsZero() {
		t.Error("DocsConfig{RootKinds: ...}.IsZero() = true, want false")
	}
}

func TestDocsConfig_IsZero_TrueWhenSectionAbsent(t *testing.T) {
	var dc DocsConfig
	if !dc.IsZero() {
		t.Error("zero-value DocsConfig.IsZero() = false, want true")
	}
}

func TestDocsConfig_Validate_RejectsUnknownValueDeterministically(t *testing.T) {
	dc := DocsConfig{RootKinds: map[string]string{
		"/z/path": "wiki",
		"/a/path": "notes",
		"/m/path": RootKindVault,
	}}
	for range 5 {
		err := dc.Validate()
		if err == nil {
			t.Fatal("Validate: want an error for an unknown root_kinds value, got nil")
		}
		for _, want := range []string{"/a/path", "notes", RootKindDocs, RootKindVault} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Validate error %q does not mention %q", err, want)
			}
		}
	}
}

func TestValidatePath_SurfacesRootKindsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[docs.root_kinds]\n\"/tmp/x\" = \"wiki\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "root_kinds") {
		t.Errorf("ValidatePath = %v, want a root_kinds error", err)
	}
}
