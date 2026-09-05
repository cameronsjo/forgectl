package config

// Test plan for DocsConfig.RootKinds (config.go)
//
// DocsConfig.RootKinds (Classification: TOML decode + IsZero coverage)
//   [x] Happy: [docs.root_kinds] decodes into a map keyed by root path
//   [x] Happy: IsZero is false when only root_kinds is set (roots/addr empty)
//   [x] Happy: IsZero is true when the [docs] section is fully absent

import (
	"reflect"
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
