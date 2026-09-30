package docs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Any in-tree Create arms a settle (forgectl#895). On kqueue, fsnotify's
// dirChange lists a changed directory and sends a Create for each entry it
// has not seen, but it stops at the first entry it cannot open (EACCES,
// EPERM, ENOENT) and never marks that entry seen. Every later change to the
// directory then re-sends that one Create and nothing for the entries
// sorting after it. These tests stand in for kqueue by injecting that
// repeated Create through the event seam (injectEvent), over a doc that
// reached the tree with no event of its own. The workaround runs only on
// kqueue (forgectl#936), so each test turns on createArmsSettle, the field
// NewWatcher sets from kqueueListing, to run it on any platform.

// kqueueListing is true exactly where fsnotify v1.10.1 builds its kqueue
// backend (backend_kqueue.go's tags), and NewWatcher turns the #895
// workaround on from it.
//
// Mutations that turn it red: flip either build-tagged constant, drop a
// GOOS from watcher_kqueue.go's tag line (that platform builds neither file
// and the package fails to compile), or set createArmsSettle to a literal
// in NewWatcher.
func TestKqueueListing_MatchesFsnotifyKqueueBackend(t *testing.T) {
	want := slices.Contains([]string{"darwin", "dragonfly", "freebsd", "netbsd", "openbsd", "ios"}, runtime.GOOS)
	if kqueueListing != want {
		t.Errorf("kqueueListing = %v on %s, want %v", kqueueListing, runtime.GOOS, want)
	}
	w := newUnstartedWatcher(t, t.TempDir())
	if w.createArmsSettle != kqueueListing {
		t.Errorf("NewWatcher set createArmsSettle = %v, want kqueueListing (%v)", w.createArmsSettle, kqueueListing)
	}
}

// Off kqueue, an in-tree Create of a non-doc arms nothing: the index is
// not rebuilt, so build output landing in a served tree costs no reload
// (forgectl#936). With the workaround on, the same Create rebuilds the
// index. Neither publishes, since the index does not change; the control
// doc write proves the watcher is live.
//
// Mutations that turn it red: drop the createArmsSettle check from Run's
// createEvent (the gate-off case rebuilds); drop createEvent from Run's
// arming (the gate-on case does not).
func TestWatcherRun_InTreeCreate_ArmsSettleOnlyWithTheKqueueGate(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("createArmsSettle=%v", on), func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "top.md"), "# Top\n")
			writeFile(t, filepath.Join(dir, "build.log"), "built\n")
			w := newUnstartedWatcher(t, dir)
			w.createArmsSettle = on
			root := w.store.Current().Roots()[0].Path
			before := w.store.Current()
			sub := startWatcher(t, w)

			injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "build.log"), Op: fsnotify.Create})
			requireNoReload(t, sub, "a non-doc Create that changes nothing the index holds")
			if rebuilt := w.store.Current() != before; rebuilt != on {
				t.Fatalf("index rebuilt = %v after an in-tree non-doc Create with createArmsSettle = %v, want %v", rebuilt, on, on)
			}

			injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "top.md"), Op: fsnotify.Write})
			awaitReload(t, sub, "control: a write to top.md")
		})
	}
}

// A Create for an unopenable non-markdown entry in a docs root (no vault,
// so it is no attachment) settles, and the rebuild's walk finds the doc
// fsnotify never announced. An outside-root Create settles nothing, so the
// unannounced doc stays unindexed through it.
//
// Mutations that turn it red: drop createEvent from Run's arming (the
// in-tree Create arms nothing, so new.md is never indexed); drop its inTree
// check (the outside-root Create settles and publishes new.md).
func TestWatcherRun_InTreeCreate_IndexesDocWithNoEventOfItsOwn(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "docs", "top.md"), "# Top\n")
	locked := filepath.Join(base, "docs", "locked.bin")
	writeFile(t, locked, "unopenable on kqueue")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex([]string{filepath.Join(base, "docs")})
	if err != nil {
		t.Fatal(err)
	}
	root := idx.Roots()[0]
	if root.Kind == RootVault {
		t.Fatal("the fixture needs a docs root, so the Create is no attachment event")
	}
	// Written after the index and before the watches, so no event names it.
	writeFile(t, filepath.Join(root.Path, "new.md"), "# New\n")
	w, err := NewWatcher(NewStore(idx), NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	w.createArmsSettle = true
	sub := startWatcher(t, w)

	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(filepath.Dir(root.Path), "outside.bin"), Op: fsnotify.Create})
	requireNoReload(t, sub, "a Create outside every root")
	if _, ok := w.store.Current().Find(root.Label, "new.md"); ok {
		t.Fatal("new.md was indexed after a Create outside every root; that Create settled")
	}

	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root.Path, "locked.bin"), Op: fsnotify.Create})
	awaitReload(t, sub, "the repeated Create of an unopenable entry")
	if _, ok := w.store.Current().Find(root.Label, "new.md"); !ok {
		t.Error("new.md is not indexed after the settle an in-tree Create armed")
	}
}

// An in-tree Create whose settle finds the index unchanged rebuilds it but
// publishes nothing. The index swap proves the Create armed a settle; the
// control doc write proves the watcher publishes.
//
// Mutations that turn it red: count createEvent as a doc event (the Create
// publishes); drop createEvent from Run's arming (no rebuild runs).
func TestWatcherRun_InTreeCreateWithUnchangedIndex_PublishesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top.md"), "# Top\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "notes\n")
	w := newUnstartedWatcher(t, dir)
	w.createArmsSettle = true
	root := w.store.Current().Roots()[0].Path
	before := w.store.Current()
	sub := startWatcher(t, w)

	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "notes.txt"), Op: fsnotify.Create})
	requireNoReload(t, sub, "a Create that changes nothing the index holds")
	if w.store.Current() == before {
		t.Fatal("the index was not rebuilt after an in-tree Create; it armed no settle")
	}

	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "top.md"), Op: fsnotify.Write})
	awaitReload(t, sub, "control: a write to top.md")
}

// A stray Create (its name resolves nowhere: a dangling symlink) keeps its
// own path: it schedules a watch rebuild and is no create event, so churn
// of them cannot keep postponing a rebuild that is already pending and
// armed. The docs root makes the names no attachment either. maxWait is
// lifted out of reach so only the gate can let the rebuild run. The churn
// is real Creates, since the rebuild closes the channel an injection sends
// on.
//
// Mutation that turns it red: drop the !stray gate on createEvent (each
// stray Create re-arms the settle).
func TestWatcherRun_StrayCreateChurn_DoesNotPostponeReset(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "docs", "top.md"), "# Top\n")
	idx, err := NewIndex([]string{filepath.Join(base, "docs")})
	if err != nil {
		t.Fatal(err)
	}
	root := idx.Roots()[0].Path
	gone := filepath.Join(base, "gone.bin")
	dangling := filepath.Join(root, "dangling.bin")
	if err := os.Symlink(gone, dangling); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	w, err := NewWatcher(NewStore(idx), NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if !w.strayEvent(dangling) || !w.inTree(dangling) || w.attachmentRelevant(fsnotify.Event{Name: dangling, Op: fsnotify.Create}) {
		t.Fatal("the fixture needs a stray, in-tree Create that is no attachment event")
	}
	w.createArmsSettle = true
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
			if err := os.Symlink(gone, filepath.Join(root, fmt.Sprintf("dangling-%d.bin", n))); err != nil {
				t.Fatal(err)
			}
		case <-stop:
			t.Fatal("no rebuild during 2s of stray Creates; each one postponed the pending rebuild")
		}
	}
}

// In-tree Creates that are not stray do re-arm the settle, but a pending
// watch rebuild still runs within maxWait of the burst's first event, not
// once the churn stops: the Creates stay inside settleIn's bound.
//
// Mutation that turns it red: make settleIn return settleDelay whole,
// without the maxWait limit (each Create postpones the rebuild again).
func TestWatcherRun_CreateChurn_RebuildsWithinMaxWait(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top.md"), "# Top\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	root := idx.Roots()[0].Path
	w, err := NewWatcher(NewStore(idx), NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	w.createArmsSettle = true
	w.debounce = 200 * time.Millisecond
	w.maxWait = 300 * time.Millisecond
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
			writeFile(t, filepath.Join(root, fmt.Sprintf("churn-%d.txt", n)), "churn\n")
		case <-stop:
			t.Fatal("no rebuild during 2s of in-tree Creates; each one postponed the pending rebuild past maxWait")
		}
	}
}
