package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sopspkg "github.com/cameronsjo/forgectl/internal/sops"
	"github.com/cameronsjo/forgectl/internal/yamlsafe"
)

// TestCheckYAMLMapping pins __sops-edit's document probe (#959): a mapping
// a map decode accepts passes; null, a scalar, a sequence, a repeated key at
// any depth, a merge key and input over the cap are refused, the last with
// yamlsafe.ErrTooLarge so the caller can name the limit.
//
// Mutations that turn it red: drop the top-level kind check (null, the
// scalar and the sequence pass); drop the CheckTree call (the repeated and
// merge keys pass); pass a huge cap to yamlsafe.Parse (the over-cap input
// passes).
func TestCheckYAMLMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		ok   bool
	}{
		{"mapping", "a:\n  b: c\n", true},
		{"empty mapping", "{}\n", true},
		{"null", "~\n", false},
		{"empty", "", false},
		{"scalar", "text\n", false},
		{"sequence", "- a\n", false},
		{"nested repeated key", "a:\n  b: 1\n  b: 2\n", false},
		{"merge key", "x: &x {b: 1}\ny:\n  <<: *x\n", false},
		{"unparseable", "a: [\n", false},
	} {
		if err := checkYAMLMapping([]byte(tc.doc), 1024); (err == nil) != tc.ok {
			t.Errorf("%s: checkYAMLMapping = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	over := "a: " + strings.Repeat("x", 1024) + "\n"
	if err := checkYAMLMapping([]byte(over), 1024); !errors.Is(err, yamlsafe.ErrTooLarge) {
		t.Errorf("over the cap: checkYAMLMapping = %v, want ErrTooLarge", err)
	}
}

// TestReadEditorTargetNamesTheSizeLimit: a document sops hands over that is
// larger than MaxDocumentBytes is refused as too large, not as "not a YAML
// mapping".
//
// Mutation that turns it red: drop the ErrTooLarge branch in
// readEditorTarget.
func TestReadEditorTargetNamesTheSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.yaml")
	big := "a: " + strings.Repeat("x", sopspkg.MaxDocumentBytes) + "\n"
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEditorTarget(path); err == nil || !strings.Contains(err.Error(), "larger than 4 MiB") {
		t.Fatalf("readEditorTarget on an oversized document: %v, want the size limit named", err)
	}
}
