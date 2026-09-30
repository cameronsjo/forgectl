package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// A single-file root's watch surface (forgectl#923), and the rules
// rootAccepts states for every root.

// onlyFileFixture builds only/{x.md, other.md, sub/y.md} and a watcher (not
// started) over the single-file root only/x.md, and returns the watcher and
// the canonical only/ path.
func onlyFileFixture(t *testing.T) (*Watcher, string) {
	t.Helper()
	only := filepath.Join(t.TempDir(), "only")
	writeFile(t, filepath.Join(only, "x.md"), "# X\n")
	writeFile(t, filepath.Join(only, "other.md"), "# Other\n")
	writeFile(t, filepath.Join(only, "sub", "y.md"), "# Y\n")
	w := newUnstartedWatcher(t, filepath.Join(only, "x.md"))
	root := w.store.Current().Roots()[0]
	if root.OnlyFile == "" {
		t.Fatal("the fixture needs a single-file root")
	}
	return w, root.Path
}

// A single-file root watches its own directory and nothing below it, at
// registration and when a directory is created there later.
//
// Mutations that turn it red: register a single-file root with watchTree
// (only/sub is watched); drop watchCreatedDir's single-file skip (only/new
// is watched after its Create).
func TestWatcherRegister_OnlyFileRoot_WatchesItsDirectoryOnly(t *testing.T) {
	w, dir := onlyFileFixture(t)
	requireWatched(t, w, dir)
	requireNotWatched(t, w, filepath.Join(dir, "sub"))

	created := filepath.Join(dir, "new")
	if err := os.MkdirAll(filepath.Join(created, "deeper"), 0o750); err != nil {
		t.Fatal(err)
	}
	w.refreshWatch(fsnotify.Event{Name: created, Op: fsnotify.Create})
	requireNotWatched(t, w, created, filepath.Join(created, "deeper"))
	requireWatched(t, w, dir)
}

// Events for a single-file root's siblings never reach the watch set or the
// settle. A stray sibling (a dangling link, which on kqueue fsnotify
// re-sends on every change to the directory) would otherwise schedule a
// watch rebuild. The control write to the root's own file proves the
// watcher runs, and its settle is where a scheduled rebuild would run.
//
// Mutation that turns it red: drop Run's onlyFileSibling check (the stray
// sibling Create rebuilds the watches).
func TestWatcherRun_OnlyFileSiblingEvents_AreDropped(t *testing.T) {
	w, dir := onlyFileFixture(t)
	dangling := filepath.Join(dir, "dangling.md")
	if err := os.Symlink(filepath.Join(dir, "gone.md"), dangling); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if !w.strayEvent(dangling) {
		t.Fatal("the fixture needs a stray sibling")
	}
	before := currentFSW(w)
	sub := startWatcher(t, w)

	injectEvent(t, w, fsnotify.Event{Name: dangling, Op: fsnotify.Create})
	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(dir, "sub"), Op: fsnotify.Rename})
	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(dir, "x.md"), Op: fsnotify.Write})
	awaitReload(t, sub, "control: a write to the single-file root's own file")
	if currentFSW(w) != before {
		t.Error("the watches were rebuilt after events on a single-file root's siblings")
	}
}

// onlyFileSibling holds a name to the single-file roots that hold it, and
// lets a directory root holding the same name take it back.
//
// Mutations that turn it red: return true for the root's own file or its
// directory; ignore directory roots (the overlapping row goes true); treat
// a name no root holds as a sibling (the outside row goes true).
func TestWatcherOnlyFileSibling(t *testing.T) {
	_, vault := attachmentWatchVault(t)
	idx, err := NewIndex([]string{filepath.Join(vault, "n.md")})
	if err != nil {
		t.Fatal(err)
	}
	w := &Watcher{store: NewStore(idx), broker: NewBroker(), debounce: testDebounce}
	root := idx.Roots()[0]
	both, err := NewIndex([]string{filepath.Join(vault, "n.md"), vault})
	if err != nil {
		t.Fatal(err)
	}
	wb := &Watcher{store: NewStore(both), broker: NewBroker(), debounce: testDebounce}

	for _, c := range []struct {
		w    *Watcher
		name string
		want bool
	}{
		{w, filepath.Join(root.Path, "other.md"), true},
		{w, filepath.Join(root.Path, "img.png"), true},
		{w, filepath.Join(root.Path, "sub", "y.md"), true},
		{w, root.OnlyFile, false},
		{w, root.Path, false},
		{w, filepath.Join(filepath.Dir(root.Path), "outside.md"), false},
		{wb, filepath.Join(root.Path, "other.md"), false},
	} {
		if got := c.w.onlyFileSibling(c.name); got != c.want {
			t.Errorf("onlyFileSibling(%s) under %d root(s) = %v, want %v", c.name, len(c.w.store.Current().Roots()), got, c.want)
		}
	}
}

// rootAccepts refuses exactly what the index walk will not reach for
// depth: a doc maxHeldDirs directories below the root is indexed and
// accepted, and one a level deeper is neither.
//
// Mutations that turn it red: drop rootAccepts' depth check (the deeper doc
// is accepted); compare with >= (the doc at the cap is refused).
func TestRootAccepts_DepthMatchesTheWalk(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	atCap := "ok/" + nestedDoc(t, filepath.Join(root, "ok"), maxHeldDirs-1)
	past := "deep/" + nestedDoc(t, filepath.Join(root, "deep"), maxHeldDirs)
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	r := idx.Roots()[0]
	for _, rel := range []string{atCap, past} {
		_, indexed := idx.Find(r.Label, rel)
		if got := rootAccepts(r, filepath.Join(r.Path, filepath.FromSlash(rel))); got != indexed {
			t.Errorf("rootAccepts for a doc %d directories deep = %v, but indexed = %v", strings.Count(rel, "/"), got, indexed)
		}
	}
	if _, ok := idx.Find(r.Label, atCap); !ok {
		t.Fatal("the doc at the cap was not indexed; the fixture exercises nothing")
	}
}

// rootAccepts is lexical: it judges a path that no longer exists, which a
// Remove or a Rename away must still reload for, and it cannot see a
// symlinked directory on the way. That second rule belongs to its callers,
// and Run's is strayEvent: a name through an in-root link to a directory
// outside is accepted lexically and refused as stray.
//
// Mutations that turn it red: make rootAccepts Lstat or resolve the path
// (the deleted row is refused); make strayEvent trust rootAccepts on the
// event's own name (the linked row is not stray).
func TestRootAccepts_IsLexical_StrayEventOwnsSymlinks(t *testing.T) {
	root, outside := swapFixture(t)
	w := newUnstartedWatcher(t, root)
	r := w.store.Current().Roots()[0]
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	writeFile(t, filepath.Join(outside, "secret.md"), "# Secret\n")

	deleted := filepath.Join(root, "gone", "doc.md")
	if !rootAccepts(r, deleted) {
		t.Errorf("rootAccepts(%s) = false for a path that does not exist, want true", deleted)
	}
	linked := filepath.Join(root, "linked", "secret.md")
	if !rootAccepts(r, linked) {
		t.Errorf("rootAccepts(%s) = false, want true: it is lexical", linked)
	}
	if !w.strayEvent(linked) {
		t.Errorf("strayEvent(%s) = false for a name through a link out of the root, want true", linked)
	}
}
