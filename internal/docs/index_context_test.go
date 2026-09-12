package docs

// Test plan for NewIndexContext (forgectl#483)
//
// NewIndexContext (Classification: ops layer — filesystem walk under a caller
// deadline)
//   [x] Happy: a live context behaves exactly like NewIndexWithOptions
//   [x] Unhappy: a canceled context stops the walk and the returned error
//       names the root and wraps context.Canceled

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewIndexContext_CanceledContext_NamesRootAndWrapsCanceled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "# A")
	writeFile(t, filepath.Join(dir, "b.md"), "# B")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewIndexContext(ctx, []string{dir}, IndexOptions{})
	if err == nil {
		t.Fatal("NewIndexContext with a canceled context: got nil error, want one")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	canonical, canonErr := CanonicalizeRoot(dir)
	if canonErr != nil {
		t.Fatalf("CanonicalizeRoot(%q): %v", dir, canonErr)
	}
	if !strings.Contains(err.Error(), canonical) {
		t.Errorf("error = %q, want it to name the root %q", err.Error(), canonical)
	}
}

func TestNewIndexContext_LiveContext_BehavesLikeNewIndexWithOptions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "# A")

	idx, err := NewIndexContext(context.Background(), []string{dir}, IndexOptions{})
	if err != nil {
		t.Fatalf("NewIndexContext: %v", err)
	}
	docs := idx.List()
	if len(docs) != 1 || docs[0].Title != "A" {
		t.Errorf("docs = %+v, want one doc titled A", docs)
	}
}
