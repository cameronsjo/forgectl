package docs

import (
	"encoding/json"
	"os"
	"path"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Mermaid is vendored in the binary; its MIT notice must ship beside the
// font and KaTeX licenses, which the goreleaser archive entries glob.
func TestMermaidLicenseShips(t *testing.T) {
	b, err := os.ReadFile("assets/font-licenses/Mermaid-MIT.txt")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.HasPrefix(s, "The MIT License") || !strings.Contains(s, "Sveidqvist") {
		t.Errorf("Mermaid-MIT.txt is not mermaid's MIT notice: %.80q", s)
	}
	y, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Archives []struct {
			ID    string `yaml:"id"`
			Files []any  `yaml:"files"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(y, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Archives) == 0 {
		t.Fatal("no archives in .goreleaser.yaml")
	}
	for _, a := range cfg.Archives {
		found := false
		for _, f := range a.Files {
			if m, ok := f.(map[string]any); ok && m["src"] == "internal/docs/assets/font-licenses/*" {
				found = true
			}
		}
		if !found {
			t.Errorf("archive %q does not ship font-licenses/*", a.ID)
		}
	}
}

// mermaid.min.js inlines dozens of libraries whose own notices the bundle's
// banner mostly omits. scripts/vendor-mermaid.sh generates
// THIRD_PARTY_NOTICES-mermaid.txt from the tarball's dependency tree; the
// direct dependency list recorded in provenance-mermaid.json is the floor the
// file must name, and the file must ride both archives through the
// font-licenses glob.
func TestMermaidThirdPartyNoticesShip(t *testing.T) {
	pb, err := os.ReadFile("assets/provenance-mermaid.json")
	if err != nil {
		t.Fatal(err)
	}
	var prov struct {
		Files []struct {
			Notices      string            `json:"notices"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"files"`
	}
	if err := json.Unmarshal(pb, &prov); err != nil {
		t.Fatal(err)
	}
	if len(prov.Files) != 1 || len(prov.Files[0].Dependencies) == 0 {
		t.Fatalf("provenance-mermaid.json records no dependency list: %s", pb)
	}
	nb, err := os.ReadFile("assets/font-licenses/THIRD_PARTY_NOTICES-mermaid.txt")
	if err != nil {
		t.Fatal(err)
	}
	notices := string(nb)
	if !strings.Contains(prov.Files[0].Notices, "THIRD_PARTY_NOTICES-mermaid.txt") {
		t.Errorf("provenance does not point at the notices file: %q", prov.Files[0].Notices)
	}
	for dep := range prov.Files[0].Dependencies {
		// The generated index lists "  name@version (license)".
		if !strings.Contains(notices, "\n  "+dep+"@") {
			t.Errorf("notices do not name direct dependency %s", dep)
		}
	}
	// The libraries the bundle banner leaves out, plus DOMPurify's dual license.
	for _, want := range []string{"\nmarked@", "\nd3-shape@", "\ndayjs@", "Apache License", "Mozilla Public License"} {
		if !strings.Contains(notices, want) {
			t.Errorf("notices missing %q", want)
		}
	}

	y, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Archives []struct {
			ID    string `yaml:"id"`
			Files []any  `yaml:"files"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(y, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Archives) < 2 {
		t.Fatalf("want both archives in .goreleaser.yaml, got %d", len(cfg.Archives))
	}
	for _, a := range cfg.Archives {
		ships := false
		for _, f := range a.Files {
			m, ok := f.(map[string]any)
			if !ok {
				continue
			}
			if src, _ := m["src"].(string); src != "" {
				if ok, _ := path.Match(src, "internal/docs/assets/font-licenses/THIRD_PARTY_NOTICES-mermaid.txt"); ok {
					ships = true
				}
			}
		}
		if !ships {
			t.Errorf("archive %q does not ship THIRD_PARTY_NOTICES-mermaid.txt", a.ID)
		}
	}
}
