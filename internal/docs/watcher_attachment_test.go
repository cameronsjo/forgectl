package docs

import (
	"context"
	"fmt"
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

// Overlapping roots (forgectl#917): a path counts when any root holding it
// accepts it, not just the first. A single-file root listed before the
// vault that holds it refuses its siblings, and a docs root listed before a
// vault inside it is the wrong kind for attachments; neither may hide the
// vault's events. Excluded directories stay refused under every root.
//
// Mutations that turn it red: stop inTree at the first containing root (the
// sibling doc and attachment rows go false), or take attachmentRelevant's
// kind from the first containing root (the nested-vault row goes false).
func TestWatcherInTree_OverlappingRoots_AnyAcceptingRootCounts(t *testing.T) {
	base, vault := attachmentWatchVault(t)
	idx, err := NewIndex([]string{filepath.Join(vault, "n.md"), vault})
	if err != nil {
		t.Fatal(err)
	}
	w := &Watcher{store: NewStore(idx), broker: NewBroker(), debounce: testDebounce}
	v := idx.Roots()[1].Path

	if !w.relevant(filepath.Join(v, "other.md")) {
		t.Error("relevant(vault/other.md) = false under [vault/n.md, vault], want true")
	}
	if !w.attachmentRelevant(fsnotify.Event{Name: filepath.Join(v, "img.png"), Op: fsnotify.Create}) {
		t.Error("attachmentRelevant(Create vault/img.png) = false under [vault/n.md, vault], want true")
	}
	if w.relevant(filepath.Join(v, ".trash", "x.md")) {
		t.Error("relevant(vault/.trash/x.md) = true, want false: every root excludes it")
	}

	nested, err := NewIndex([]string{base, vault})
	if err != nil {
		t.Fatal(err)
	}
	if nested.Roots()[0].Kind == RootVault || nested.Roots()[1].Kind != RootVault {
		t.Fatalf("root kinds = %v, %v; the fixture needs a docs root holding a vault", nested.Roots()[0].Kind, nested.Roots()[1].Kind)
	}
	wn := &Watcher{store: NewStore(nested), broker: NewBroker(), debounce: testDebounce}
	if !wn.attachmentRelevant(fsnotify.Event{Name: filepath.Join(nested.Roots()[1].Path, "img.png"), Op: fsnotify.Create}) {
		t.Error("attachmentRelevant(Create vault/img.png) = false under [base, vault], want true: the vault root accepts it")
	}
	if wn.attachmentRelevant(fsnotify.Event{Name: filepath.Join(nested.Roots()[0].Path, "img.png"), Op: fsnotify.Create}) {
		t.Error("attachmentRelevant(Create base/img.png) = true, want false: only a docs root holds it")
	}
}

// End to end under `docs serve vault/n.md vault`: a new doc and a new
// attachment in the vault both publish.
//
// Mutations that turn it red: stop inTree at the first containing root (the
// doc add goes silent), or take attachmentRelevant's kind from the first
// containing root (the attachment add goes silent).
func TestWatcher_OverlappingRoots_VaultEventsPublish(t *testing.T) {
	_, vault := attachmentWatchVault(t)
	store, sub, _ := newTestWatcher(t, filepath.Join(vault, "n.md"), vault)
	label := store.Current().Roots()[1].Label

	writeFile(t, filepath.Join(vault, "other.md"), "# Other\n")
	wantReload(t, sub, "adding vault/other.md")

	writeFile(t, filepath.Join(vault, "img.png"), "png")
	wantReload(t, sub, "adding vault/img.png")
	if got := imgVerdict(t, store, label); got != MissAttachment {
		t.Errorf("[[img.png]] after the add = %v, want MissAttachment", got)
	}
}

// A stray attachment event (its name resolves nowhere: a dangling symlink)
// is not an attachment event, so churn of them cannot keep postponing a
// watch rebuild that is already pending and armed. The churn is real
// filesystem Creates of fresh dangling links rather than injected events,
// since the rebuild closes the channel an injection would send on. maxWait
// is lifted out of reach so only the gate can let the rebuild run.
//
// Mutation that turns it red: drop the !stray gate on attachmentEvent in
// Run (each stray Create re-arms the settle).
func TestWatcherRun_StrayAttachmentChurn_DoesNotPostponeReset(t *testing.T) {
	base, vault := attachmentWatchVault(t)
	idx, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}
	v := idx.Roots()[0].Path
	dangling := filepath.Join(v, "dangling.png")
	if err := os.Symlink(filepath.Join(base, "gone.png"), dangling); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	w, err := NewWatcher(NewStore(idx), NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if !w.strayEvent(dangling) || !w.attachmentRelevant(fsnotify.Event{Name: dangling, Op: fsnotify.Create}) {
		t.Fatal("the fixture needs a stray, attachment-shaped Create")
	}
	w.debounce = 200 * time.Millisecond
	w.maxWait = time.Minute
	w.resetPending = true
	before := currentFSW(w)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-runDone
	})
	go func() {
		defer close(runDone)
		w.Run(ctx)
	}()

	stop := time.After(2 * time.Second)
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	for n := 0; currentFSW(w) == before; {
		select {
		case <-tick.C:
			n++
			if err := os.Symlink(filepath.Join(base, "gone.png"), filepath.Join(v, fmt.Sprintf("dangling-%d.png", n))); err != nil {
				t.Fatal(err)
			}
		case <-stop:
			t.Fatal("no rebuild during 2s of stray attachment Creates; each one postponed the pending rebuild")
		}
	}
}

// Two attachments whose names differ only in case fold to one attRel key,
// but removing one changes resolution: [[dup.png]] goes from ambiguous to a
// hit. sameIndex must see that (forgectl#917).
//
// Mutation that turns it red: compare the folded attRel sets instead of
// the per-name tables.
func TestSameIndex_CaseVariantAttachmentRemoved_Differs(t *testing.T) {
	_, vault := attachmentWatchVault(t)
	writeFile(t, filepath.Join(vault, "a", "Dup.png"), "png")
	writeFile(t, filepath.Join(vault, "a", "dup.png"), "png")
	entries, err := os.ReadDir(filepath.Join(vault, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Skip("case-insensitive filesystem: Dup.png and dup.png are one file")
	}
	before, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(vault, "a", "Dup.png")); err != nil {
		t.Fatal(err)
	}
	after, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}

	verdict := func(idx *Index) Miss {
		from, ok := idx.Find(idx.Roots()[0].Label, "n.md")
		if !ok {
			t.Fatal("n.md not indexed")
		}
		_, _, miss := idx.resolveWikilink(&from, LinkRef{Path: "dup.png", Form: FormPlain}, nil)
		return miss
	}
	if b, a := verdict(before), verdict(after); b != MissAmbiguous || a != MissAttachment {
		t.Fatalf("[[dup.png]] = %v then %v, want MissAmbiguous then MissAttachment; the fixture exercises nothing", b, a)
	}
	if sameIndex(before, after) {
		t.Error("sameIndex = true across removing one of two case-variant attachments, want false")
	}
}
