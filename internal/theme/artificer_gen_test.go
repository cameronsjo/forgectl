// Package theme_test (external) so this file can import palettegen, which
// itself imports theme — an internal (package theme) test file importing
// palettegen would be a real import cycle; the external test package is a
// separate compilation unit and has none.
package theme_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/theme/palettegen"
)

// TestArtificerGen_IsCurrent re-renders artificer_gen.go from the vendored
// palette and byte-compares it against the committed file. A mismatch means
// _palette.json changed (or Render's output shape did) without regenerating.
func TestArtificerGen_IsCurrent(t *testing.T) {
	raw, err := os.ReadFile(theme.PalettePath)
	if err != nil {
		t.Fatalf("read %s: %v", theme.PalettePath, err)
	}
	want, err := palettegen.Render(raw)
	if err != nil {
		t.Fatalf("palettegen.Render: %v", err)
	}
	got, err := os.ReadFile("artificer_gen.go")
	if err != nil {
		t.Fatalf("read artificer_gen.go: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("artificer_gen.go is stale — run: go generate ./internal/theme")
	}
}
