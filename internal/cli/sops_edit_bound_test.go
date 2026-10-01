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
// a map decode accepts passes, merge keys included, since nothing here
// decodes the document; null, a scalar, a sequence, a repeated key at any
// depth and input over the cap are refused, the last with
// yamlsafe.ErrTooLarge so the caller can name the limit.
//
// Mutations that turn it red: drop the top-level kind check (null, the
// scalar and the sequence pass); drop the CheckTree call (the repeated keys
// pass); drop AllowMerge (the Helm-style merge rows are refused); pass a
// huge cap to yamlsafe.Parse (the over-cap input passes).
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
		{"merge key", "x: &x {b: 1}\ny:\n  <<: *x\n", true},
		{"nested merge key", "defaults: &defaults\n  replicas: 1\napp:\n  web:\n    <<: *defaults\n    image: x\n", true},
		{"repeated merge key", "x: &x {b: 1}\ny:\n  <<: *x\n  <<: *x\n", false},
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
// larger than MaxRewrittenBytes is refused as too large, not as "not a YAML
// mapping".
//
// Mutation that turns it red: drop the ErrTooLarge branch in
// readEditorTarget.
func TestReadEditorTargetNamesTheSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.yaml")
	big := "a: " + strings.Repeat("x", sopspkg.MaxRewrittenBytes) + "\n"
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEditorTarget(path); err == nil || !strings.Contains(err.Error(), "larger than 5 MiB") {
		t.Fatalf("readEditorTarget on an oversized document: %v, want the size limit named", err)
	}
}

// TestSopsEdit_EditsADocumentWithMergeKeys is the #1001 Gate 2 regression:
// a decrypted values file using `<<: *defaults` deep in its values is edited
// as before the #959 bounds, not refused after sops has started. Merges are
// never applied here, so the key is added beside the merge line.
//
// Mutation that turns it red: drop AllowMerge from checkYAMLMapping.
func TestSopsEdit_EditsADocumentWithMergeKeys(t *testing.T) {
	workdir := sopsWorkdirFixture(t)
	const nonce = "a-test-nonce"
	if err := os.WriteFile(filepath.Join(workdir, sopsNonceFile), []byte(nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, sopsValueFile), []byte("a-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sopsWorkdirEnv, workdir)
	t.Setenv(sopsNonceEnv, nonce)
	t.Setenv(sopsPathEnv, "app.web.token")

	const before = "defaults: &defaults\n    replicas: 1\napp:\n    web:\n        <<: *defaults\n        image: x\n"
	doc := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(doc, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSopsEdit(doc); err != nil {
		t.Fatalf("runSopsEdit on a document with a merge key: %v", err)
	}
	edited, err := os.ReadFile(doc) //nolint:gosec // G304: a path this test created
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(edited), "token: 'a-value'") || !strings.Contains(string(edited), "<<: *defaults") {
		t.Errorf("edited document = %q, want the new key beside the merge line", edited)
	}
}

// TestReadEditorTargetNamesAWalkRefusal: a document refused for its shape
// (here a repeated key) says so with the line, not "not a YAML mapping".
//
// Mutation that turns it red: return errNotYAMLMapping for every
// checkYAMLMapping failure.
func TestReadEditorTargetNamesAWalkRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(path, []byte("a:\n  b: 1\n  b: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readEditorTarget(path)
	if err == nil || !strings.Contains(err.Error(), "line 3: a mapping key is repeated") || strings.Contains(err.Error(), "not a YAML mapping") {
		t.Fatalf("readEditorTarget on a repeated key: %v, want the walk's refusal", err)
	}
}
