package config

// Test plan for DocsConfig.SearchBackend (config.go)
//
//   [x] Happy: search_backend decodes, and IsZero is false when only it is set
//   [x] Happy: Validate accepts "", "ripgrep" and "qmd"
//   [x] Unhappy: Validate rejects any other value, naming the key and value

import (
	"strings"
	"testing"
)

func TestDocsConfig_SearchBackend_Decodes(t *testing.T) {
	got, err := DecodeStrict([]byte("[docs]\nsearch_backend = \"qmd\"\n"))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if got.Docs.SearchBackend != SearchBackendQMD {
		t.Errorf("SearchBackend = %q, want %q", got.Docs.SearchBackend, SearchBackendQMD)
	}
	if got.Docs.IsZero() {
		t.Error("IsZero() = true with only search_backend set")
	}
}

func TestDocsConfig_SearchBackend_Validate(t *testing.T) {
	for _, v := range []string{"", SearchBackendRipgrep, SearchBackendQMD} {
		if err := (DocsConfig{SearchBackend: v}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", v, err)
		}
	}
	err := DocsConfig{SearchBackend: "auto"}.Validate()
	if err == nil || !strings.Contains(err.Error(), "search_backend") || !strings.Contains(err.Error(), `"auto"`) {
		t.Errorf("Validate(auto) = %v, want an error naming search_backend and the value", err)
	}
}
