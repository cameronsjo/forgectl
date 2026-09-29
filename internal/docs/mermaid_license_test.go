package docs

import (
	"os"
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
