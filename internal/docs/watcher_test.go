package docs

// Test plan for watcher.go
//
// Watcher.relevant (Classification: security predicate — deterministic, no timing)
//   [x] Happy: a .md write directly under a root is relevant
//   [x] Happy: a .md write in a nested, non-excluded subdirectory is relevant
//   [x] Happy: a dot-FILE with a .md extension is relevant (only DIRECTORIES
//              are excluded by name, and walkRoot indexes such a file)
//   [x] Unhappy (security): a write under .trash/ is NOT relevant
//   [x] Unhappy (security): a write under node_modules/, vendor/, .git/, and an
//              arbitrary dot-directory is NOT relevant
//   [x] Unhappy (security): a write under a DEEPLY NESTED excluded directory is
//              NOT relevant (the check must scan every path segment, not just
//              the immediate parent)
//   [x] Unhappy (security): a write outside every configured root is NOT relevant
//   [x] Unhappy (security): a sibling of a single-file root is NOT relevant
//   [x] Unhappy: a non-markdown extension is NOT relevant
//
// Watcher end-to-end (Classification: integration — filesystem timing)
//   [x] Happy: editing an indexed doc publishes a reload and the swapped index
//              reflects the change
//   [x] Happy: creating a new doc publishes a reload and the new doc becomes
//              servable through the store's index
//   [x] Unhappy (security): a write under an excluded directory publishes NO
//              reload event — paired with a control write to a non-excluded doc
//              in the same fixture, so the silence is evidence rather than the
//              symptom of a watcher that never started
//
// excludedDir (Classification: shared predicate)
//   [x] Happy/Unhappy: the indexer and the watcher agree, by construction —
//              both call this one function (see its doc comment)

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testDebounce is short enough to keep the suite fast but still long enough to
// coalesce the multi-event burst a single write produces.
const testDebounce = 20 * time.Millisecond

// quietWindow is how long a negative test waits to be confident no reload is
// coming. Comfortably longer than testDebounce so a genuine (but slow) event
// would have landed.
const quietWindow = 750 * time.Millisecond

// newTestWatcher builds a Store/Broker/Watcher trio over dir and starts the
// watcher, returning the store, a reload subscription, and the root's label.
func newTestWatcher(t *testing.T, paths ...string) (*Store, <-chan string, string) {
	t.Helper()

	idx, err := NewIndex(paths)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	store := NewStore(idx)
	broker := NewBroker()

	w, err := NewWatcher(store, broker)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.debounce = testDebounce

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	t.Cleanup(func() {
		cancel()
		broker.Close()
		if err := w.Close(); err != nil {
			t.Logf("watcher close: %v", err)
		}
	})

	sub, unsubscribe := broker.Subscribe()
	t.Cleanup(unsubscribe)

	return store, sub, idx.Roots()[0].Label
}

// relevanceFixture builds a watcher (not started) over a tree containing every
// excluded-directory shape, for the deterministic predicate tests.
func relevanceFixture(t *testing.T) (*Watcher, string) {
	t.Helper()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top.md"), "# Top\n")
	if err := os.MkdirAll(filepath.Join(dir, "nested", "deeper"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(dir, "nested", "deeper", "buried.md"), "# Buried\n")

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	w := &Watcher{store: NewStore(idx), broker: NewBroker(), debounce: testDebounce}

	// Canonical root path, because relevant() compares lexically against it and
	// t.TempDir on macOS hands back a /var symlink to /private/var.
	return w, idx.Roots()[0].Path
}

func TestWatcherRelevant_MarkdownUnderRoot_IsRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	if !w.relevant(filepath.Join(root, "top.md")) {
		t.Error("relevant(<root>/top.md) = false, want true")
	}
}

func TestWatcherRelevant_MarkdownInNestedDir_IsRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	path := filepath.Join(root, "nested", "deeper", "buried.md")
	if !w.relevant(path) {
		t.Errorf("relevant(%q) = false, want true", path)
	}
}

func TestWatcherRelevant_MediaUnderDirectoryRootIsRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	path := filepath.Join(root, "images", "architecture.svg")
	if !w.relevant(path) {
		t.Errorf("relevant(%q) = false, want true", path)
	}
}

func TestWatcherRelevant_DotFileWithMarkdownExt_IsRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	// walkRoot excludes hidden DIRECTORIES, not hidden files — a file named
	// ".notes.md" is indexed, so a write to it must trigger reload. If this
	// ever flips, the exclusion rule and this predicate have drifted.
	path := filepath.Join(root, ".notes.md")
	if !w.relevant(path) {
		t.Errorf("relevant(%q) = false, want true — only directories are excluded by name", path)
	}
}

func TestWatcherRelevant_WriteUnderTrash_IsNotRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	path := filepath.Join(root, ".trash", "deleted.md")
	if w.relevant(path) {
		t.Errorf("relevant(%q) = true, want false — a write under .trash must not wake the reader", path)
	}
}

func TestWatcherRelevant_WriteUnderExcludedDirs_IsNotRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	for _, excluded := range []string{"node_modules", "vendor", ".git", ".obsidian", ".hidden"} {
		path := filepath.Join(root, excluded, "doc.md")
		if w.relevant(path) {
			t.Errorf("relevant(%q) = true, want false — %s is an excluded directory", path, excluded)
		}
	}
}

func TestWatcherRelevant_WriteUnderDeeplyNestedExcludedDir_IsNotRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	// The excluded segment is neither the first nor the immediate parent, so a
	// check that only inspected one of those would wrongly allow this.
	path := filepath.Join(root, "nested", "node_modules", "pkg", "readme.md")
	if w.relevant(path) {
		t.Errorf("relevant(%q) = true, want false — every path segment must be checked, not just the immediate parent", path)
	}
}

func TestWatcherRelevant_PathOutsideEveryRoot_IsNotRelevant(t *testing.T) {
	w, _ := relevanceFixture(t)

	elsewhere := filepath.Join(t.TempDir(), "stranger.md")
	if w.relevant(elsewhere) {
		t.Errorf("relevant(%q) = true, want false — path is outside every configured root", elsewhere)
	}
}

func TestWatcherRelevant_SiblingOfSingleFileRoot_IsNotRelevant(t *testing.T) {
	dir := t.TempDir()
	only := filepath.Join(dir, "only.md")
	writeFile(t, only, "# Only\n")
	writeFile(t, filepath.Join(dir, "sibling.md"), "# Sibling\n")

	idx, err := NewIndex([]string{only})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	w := &Watcher{store: NewStore(idx), broker: NewBroker(), debounce: testDebounce}

	root := idx.Roots()[0]
	if !w.relevant(root.OnlyFile) {
		t.Errorf("relevant(%q) = false, want true for the single-file root's own file", root.OnlyFile)
	}
	sibling := filepath.Join(root.Path, "sibling.md")
	if w.relevant(sibling) {
		t.Errorf("relevant(%q) = true, want false — naming one file must not make its siblings live-reloadable", sibling)
	}
}

func TestWatcherRelevant_NonMarkdownExtension_IsNotRelevant(t *testing.T) {
	w, root := relevanceFixture(t)

	for _, name := range []string{"notes.txt", "secret.env", "archive.zip", "noext"} {
		path := filepath.Join(root, name)
		if w.relevant(path) {
			t.Errorf("relevant(%q) = true, want false — extension is not in the docs allowlist", path)
		}
	}
}

func TestWatcher_EditedDoc_PublishesReloadAndSwapsIndex(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "plan.md")
	writeFile(t, doc, "# Plan\n\noriginal\n")

	store, sub, label := newTestWatcher(t, dir)

	writeFile(t, doc, "# Plan Revised\n\nedited\n")

	select {
	case msg, ok := <-sub:
		if !ok {
			t.Fatal("broker channel closed, want a reload notification")
		}
		if msg != reloadMessage {
			t.Errorf("msg = %q, want %q", msg, reloadMessage)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("no reload notification within %s after editing %s", recvTimeout, doc)
	}

	// The swapped-in index must reflect the new title, proving the watcher
	// rebuilt rather than just notifying.
	got, ok := store.Current().Find(label, "plan.md")
	if !ok {
		t.Fatalf("Find(%q, \"plan.md\") not found in the swapped index", label)
	}
	if got.Title != "Plan Revised" {
		t.Errorf("title = %q after reload, want %q — the index was not rebuilt", got.Title, "Plan Revised")
	}
}

func TestWatcher_NewDoc_BecomesServable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "existing.md"), "# Existing\n")

	store, sub, label := newTestWatcher(t, dir)

	if _, err := store.Current().Resolve(label, "fresh.md"); err == nil {
		t.Fatal("Resolve on a not-yet-created doc succeeded, want an error before the write")
	}

	writeFile(t, filepath.Join(dir, "fresh.md"), "# Fresh\n")

	select {
	case _, ok := <-sub:
		if !ok {
			t.Fatal("broker channel closed, want a reload notification")
		}
	case <-time.After(recvTimeout):
		t.Fatalf("no reload notification within %s after creating a new doc", recvTimeout)
	}

	if _, err := store.Current().Resolve(label, "fresh.md"); err != nil {
		t.Errorf("Resolve(%q, \"fresh.md\") after reload = %v, want nil", label, err)
	}
}

func TestWatcher_WriteUnderExcludedDir_PublishesNoReload(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "visible.md"), "# Visible\n")
	trash := filepath.Join(dir, ".trash")
	modules := filepath.Join(dir, "node_modules")
	for _, sub := range []string{trash, modules} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
	}

	store, sub, label := newTestWatcher(t, dir)

	writeFile(t, filepath.Join(trash, "deleted.md"), "# Deleted\n\nshould stay invisible\n")
	writeFile(t, filepath.Join(modules, "dep.md"), "# Dep\n")

	select {
	case msg, ok := <-sub:
		if ok {
			t.Fatalf("got reload notification %q for a write under an excluded directory; reload timing alone would reveal that excluded content changed", msg)
		}
		t.Fatal("broker channel closed unexpectedly")
	case <-time.After(quietWindow):
		// No notification: correct.
	}

	// CONTROL. Silence above is only evidence if this watcher was actually
	// alive and watching this tree. A watcher that failed to register, or whose
	// goroutine had already exited, would produce the same silence and this test
	// would pass while asserting nothing. So prove the positive path works in
	// the very same fixture: a write to a NON-excluded doc must still notify.
	writeFile(t, filepath.Join(dir, "visible.md"), "# Visible\n\nedited\n")
	select {
	case msg, ok := <-sub:
		if !ok {
			t.Fatal("broker channel closed, want a reload notification for the control write")
		}
		if msg != reloadMessage {
			t.Errorf("control msg = %q, want %q", msg, reloadMessage)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("control write produced no reload within %s — the watcher was not live, so the silence above proves nothing about the exclusion", recvTimeout)
	}

	// And the excluded files must still be unservable.
	for _, rel := range []string{".trash/deleted.md", "node_modules/dep.md"} {
		if _, err := store.Current().Resolve(label, rel); err == nil {
			t.Errorf("Resolve(%q, %q) succeeded, want an error", label, rel)
		}
	}
}
