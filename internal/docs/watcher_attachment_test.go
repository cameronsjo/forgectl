package docs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Live reload of a vault's attachment set (forgectl#904). The set is what a
// vault wikilink can resolve to besides a note (resolveAttachment), so adding
// or removing an attachment must reach the reader, while a content write to
// one, or anything outside the root, must not.

// attachmentWatchVault builds a vault at base/vault holding n.md, which links
// [[img.png]], and returns base and the vault path.
func attachmentWatchVault(t *testing.T) (base, vault string) {
	t.Helper()
	base = t.TempDir()
	vault = filepath.Join(base, "vault")
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(vault, "n.md"), "# N\n\n[[img.png]]\n")
	return base, vault
}

// imgVerdict is how [[img.png]] from n.md resolves in the store's current
// index.
func imgVerdict(t *testing.T, store *Store, label string) Miss {
	t.Helper()
	idx := store.Current()
	from, ok := idx.Find(label, "n.md")
	if !ok {
		t.Fatalf("n.md not indexed under %q", label)
	}
	_, _, miss := idx.resolveWikilink(&from, LinkRef{Path: "img.png", Form: FormPlain}, nil)
	return miss
}

func wantReload(t *testing.T, sub <-chan string, what string) {
	t.Helper()
	select {
	case msg, ok := <-sub:
		if !ok {
			t.Fatalf("%s: broker channel closed, want a reload notification", what)
		}
		if msg != reloadMessage {
			t.Errorf("%s: msg = %q, want %q", what, msg, reloadMessage)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("%s: no reload notification within %s", what, recvTimeout)
	}
}

func wantNoReload(t *testing.T, sub <-chan string, what string) {
	t.Helper()
	select {
	case msg, ok := <-sub:
		if ok {
			t.Fatalf("%s: got reload notification %q, want none", what, msg)
		}
		t.Fatalf("%s: broker channel closed unexpectedly", what)
	case <-time.After(quietWindow):
	}
}

// Adding an attachment makes a link to it resolve and publishes; deleting it
// makes the link broken again and publishes.
//
// Mutations that turn it red: attachmentRelevant returns false (nothing arms
// the settle), or sameIndex skips the attachment sets (the settle runs but
// finds nothing to publish).
func TestWatcher_AttachmentAddAndRemove_Publishes(t *testing.T) {
	_, vault := attachmentWatchVault(t)
	store, sub, label := newTestWatcher(t, vault)
	if got := imgVerdict(t, store, label); got != MissNoTarget {
		t.Fatalf("[[img.png]] before the add = %v, want MissNoTarget", got)
	}

	img := filepath.Join(vault, "img.png")
	writeFile(t, img, "png")
	wantReload(t, sub, "adding img.png")
	if got := imgVerdict(t, store, label); got != MissAttachment {
		t.Errorf("[[img.png]] after the add = %v, want MissAttachment", got)
	}

	if err := os.Remove(img); err != nil {
		t.Fatal(err)
	}
	wantReload(t, sub, "removing img.png")
	if got := imgVerdict(t, store, label); got != MissNoTarget {
		t.Errorf("[[img.png]] after the remove = %v, want MissNoTarget", got)
	}
}

// Writing to an existing attachment's contents changes no name, so it does
// not publish. The control add proves the watcher was live.
//
// Mutation that turns it red: count any in-tree name as a doc event
// (docEvent := !stray && w.inTree(ev.Name)), so the write publishes.
func TestWatcher_AttachmentContentWrite_PublishesNoReload(t *testing.T) {
	_, vault := attachmentWatchVault(t)
	img := filepath.Join(vault, "img.png")
	writeFile(t, img, "png")
	store, sub, label := newTestWatcher(t, vault)

	writeFile(t, img, "png, edited")
	wantNoReload(t, sub, "writing img.png's contents")

	writeFile(t, filepath.Join(vault, "other.png"), "png")
	wantReload(t, sub, "control: adding other.png")
	if got := imgVerdict(t, store, label); got != MissAttachment {
		t.Errorf("[[img.png]] = %v, want MissAttachment", got)
	}
}

// An attachment-shaped name that leads outside the root never publishes:
// an in-root symlink to an outside file, and writes to that outside file.
// The walk lists neither, so the attachment set cannot change. The control
// add proves the watcher was live.
//
// Mutation that turns it red: let walkRoot list a symlink as an attachment.
func TestWatcher_OutsideRootAttachment_PublishesNoReload(t *testing.T) {
	base, vault := attachmentWatchVault(t)
	outside := filepath.Join(base, "outside.png")
	writeFile(t, outside, "secret")
	store, sub, label := newTestWatcher(t, vault)

	if err := os.Symlink(outside, filepath.Join(vault, "img.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeFile(t, outside, "secret, edited")
	writeFile(t, filepath.Join(base, "sibling.png"), "outside the root")
	wantNoReload(t, sub, "a symlink out of the root and outside writes")
	if got := imgVerdict(t, store, label); got != MissNoTarget {
		t.Errorf("[[img.png]] through a symlink out of the root = %v, want MissNoTarget", got)
	}

	writeFile(t, filepath.Join(vault, "real.png"), "png")
	wantReload(t, sub, "control: adding real.png")
}

// attachmentRelevant accepts a name change of a non-dot, non-markdown file in
// a vault root's tree, and nothing else.
//
// Mutations that turn it red: drop the event-op check (a Write is accepted),
// the dot-file check, the inTree check (the outside path and .trash are
// accepted), or the vault-kind check (the docs root is accepted).
func TestWatcherAttachmentRelevant(t *testing.T) {
	base, vault := attachmentWatchVault(t)
	docsDir := filepath.Join(base, "docs")
	writeFile(t, filepath.Join(docsDir, "README.md"), "# R\n")
	idx, err := NewIndex([]string{vault, docsDir})
	if err != nil {
		t.Fatal(err)
	}
	w := &Watcher{store: NewStore(idx), broker: NewBroker(), debounce: testDebounce}
	v, d := idx.Roots()[0].Path, idx.Roots()[1].Path

	for _, c := range []struct {
		name string
		op   fsnotify.Op
		want bool
	}{
		{filepath.Join(v, "img.png"), fsnotify.Create, true},
		{filepath.Join(v, "img.png"), fsnotify.Remove, true},
		{filepath.Join(v, "img.png"), fsnotify.Rename, true},
		{filepath.Join(v, "assets", "doc.pdf"), fsnotify.Create, true},
		{filepath.Join(v, "img.png"), fsnotify.Write, false},
		{filepath.Join(v, "img.png"), fsnotify.Chmod, false},
		{filepath.Join(v, ".hidden.png"), fsnotify.Create, false},
		{filepath.Join(v, "note.md"), fsnotify.Create, false},
		{filepath.Join(v, ".trash", "img.png"), fsnotify.Create, false},
		{filepath.Join(base, "outside.png"), fsnotify.Create, false},
		{filepath.Join(d, "img.png"), fsnotify.Create, false},
	} {
		if got := w.attachmentRelevant(fsnotify.Event{Name: c.name, Op: c.op}); got != c.want {
			t.Errorf("attachmentRelevant(%s %s) = %v, want %v", c.op, c.name, got, c.want)
		}
	}
}

// sameIndex tells two indexes apart by their attachment sets alone.
//
// Mutation that turns it red: drop the attachment-set comparison.
func TestSameIndex_AttachmentSetDiffers(t *testing.T) {
	_, vault := attachmentWatchVault(t)
	before, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(vault, "img.png"), "png")
	after, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}
	if sameIndex(before, after) {
		t.Error("sameIndex = true across an added attachment, want false")
	}
	again, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}
	if !sameIndex(after, again) {
		t.Error("sameIndex = false for two builds of an unchanged vault, want true")
	}
}
