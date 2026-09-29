package docs

import (
	"os"
	"strings"
	"testing"
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
	if n := strings.Count(string(y), "internal/docs/assets/font-licenses/*"); n != 2 {
		t.Errorf("font-licenses glob in %d archive entries, want 2 (linux, darwin)", n)
	}
}
