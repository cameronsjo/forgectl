package docs

// Test plan for store.go
//
// Store (Classification: concurrency primitive — run under -race)
//   [x] Happy: Current returns the index the store was built with
//   [x] Happy: Swap makes subsequent Current calls return the new index
//   [x] Happy: a reader holding the OLD pointer still sees the old contents
//              after a Swap (the property that makes lock-free reads safe)
//   [x] Happy: concurrent Current/Swap is race-free
//
// Index.Rebuild (Classification: core logic)
//   [x] Happy: Rebuild picks up a file created since the original build
//   [x] Happy: Rebuild drops a file deleted since the original build
//   [x] Unhappy (security): Rebuild does NOT index a file created under an
//              excluded directory — the exclusion must survive a rebuild, since
//              pathIndex membership is what makes a path servable at all
//   [x] Unhappy (security): a single-file root stays single-file across a
//              Rebuild — it must not widen into its parent directory
//   [x] Unhappy: Rebuild on a root that has since been removed returns an error
//              (callers keep serving the previous index)

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStore_Current_ReturnsInitialIndex(t *testing.T) {
	idx, _ := testIndex(t)
	s := NewStore(idx)

	if got := s.Current(); got != idx {
		t.Errorf("Current() = %p, want the index the store was built with (%p)", got, idx)
	}
}

func TestStore_Swap_CurrentReturnsNewIndex(t *testing.T) {
	idx, _ := testIndex(t)
	s := NewStore(idx)

	replacement, _ := testIndex(t)
	s.Swap(replacement)

	if got := s.Current(); got != replacement {
		t.Errorf("Current() = %p after Swap, want %p", got, replacement)
	}
}

func TestStore_SwapDuringRead_OldPointerKeepsOldContents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one.md"), "# One\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	s := NewStore(idx)

	// Simulate a request that has loaded the index and is mid-render.
	held := s.Current()
	before := len(held.List())

	writeFile(t, filepath.Join(dir, "two.md"), "# Two\n")
	rebuilt, err := idx.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	s.Swap(rebuilt)

	if got := len(held.List()); got != before {
		t.Errorf("held index doc count = %d after a Swap, want %d unchanged — an in-flight request must not observe the swap", got, before)
	}
	if got := len(s.Current().List()); got != before+1 {
		t.Errorf("Current() doc count = %d, want %d", got, before+1)
	}
}

func TestStore_ConcurrentCurrentAndSwap_IsRaceFree(t *testing.T) {
	idx, _ := testIndex(t)
	s := NewStore(idx)
	replacement, _ := testIndex(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Current().List()
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Swap(replacement)
		}()
	}
	wg.Wait()
}

func TestIndexRebuild_NewFile_IsIndexed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one.md"), "# One\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	writeFile(t, filepath.Join(dir, "two.md"), "# Two\n")
	rebuilt, err := idx.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if got := len(rebuilt.List()); got != 2 {
		t.Errorf("rebuilt doc count = %d, want 2; docs: %+v", got, rebuilt.List())
	}
	label := rebuilt.Roots()[0].Label
	if _, err := rebuilt.Resolve(label, "two.md"); err != nil {
		t.Errorf("Resolve(%q, \"two.md\") after Rebuild = %v, want nil", label, err)
	}
}

func TestIndexRebuild_DeletedFile_IsDropped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one.md"), "# One\n")
	writeFile(t, filepath.Join(dir, "two.md"), "# Two\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	if err := os.Remove(filepath.Join(dir, "two.md")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	rebuilt, err := idx.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if got := len(rebuilt.List()); got != 1 {
		t.Errorf("rebuilt doc count = %d, want 1; docs: %+v", got, rebuilt.List())
	}
	if _, err := rebuilt.Resolve(label, "two.md"); err == nil {
		t.Error("Resolve on a deleted doc succeeded after Rebuild, want an error")
	}
}

func TestIndexRebuild_FileUnderExcludedDir_StaysUnservable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "visible.md"), "# Visible\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	// Create the excluded content only AFTER the first build, so the rebuild is
	// what has to keep it out. This is the regression that matters for live
	// reload: a rebuild that re-derived servability differently from the
	// original walk would reopen the excluded-directory leak.
	for _, excluded := range []string{".trash", "node_modules", "vendor", ".git", ".obsidian"} {
		sub := filepath.Join(dir, excluded)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
		writeFile(t, filepath.Join(sub, "secret.md"), "# Secret\n\nleaked\n")
	}

	rebuilt, err := idx.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	for _, excluded := range []string{".trash", "node_modules", "vendor", ".git", ".obsidian"} {
		rel := excluded + "/secret.md"
		if _, err := rebuilt.Resolve(label, rel); err == nil {
			t.Errorf("Resolve(%q, %q) succeeded after Rebuild, want an error — the exclusion must survive a rebuild", label, rel)
		}
	}
	if got := len(rebuilt.List()); got != 1 {
		t.Errorf("rebuilt doc count = %d, want 1 (only visible.md); docs: %+v", got, rebuilt.List())
	}
}

func TestIndexRebuild_SingleFileRoot_DoesNotWidenToItsDirectory(t *testing.T) {
	dir := t.TempDir()
	only := filepath.Join(dir, "only.md")
	writeFile(t, only, "# Only\n")
	writeFile(t, filepath.Join(dir, "sibling.md"), "# Sibling\n")

	idx, err := NewIndex([]string{only})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	rebuilt, err := idx.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if got := len(rebuilt.List()); got != 1 {
		t.Errorf("rebuilt doc count = %d, want 1 — a single-file root must not widen into its parent directory; docs: %+v", got, rebuilt.List())
	}
	if _, err := rebuilt.Resolve(label, "sibling.md"); err == nil {
		t.Error("Resolve on a sibling of a single-file root succeeded after Rebuild, want an error")
	}
	if _, err := rebuilt.Resolve(label, "only.md"); err != nil {
		t.Errorf("Resolve on the single-file root's own file = %v, want nil", err)
	}
}

func TestIndexRebuild_RemovedRoot_ReturnsError(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "docs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(dir, "one.md"), "# One\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if _, err := idx.Rebuild(); err == nil {
		t.Error("Rebuild on a removed root returned nil error, want an error so the caller keeps serving the previous index")
	}
}
