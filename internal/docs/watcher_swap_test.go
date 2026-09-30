package docs

// Test plan for the watcher's registration walk (forgectl#769)
//
// Watcher registration (Classification: security boundary, low severity:
// a stray watch can only drive extra rebuilds)
//   [x] Unhappy: a directory swapped for a symlink before the pre-Add check
//              is never added
//   [x] Unhappy: a directory swapped between that check and the Add is left
//              alone and the watches are rebuilt from scratch on the next
//              reload (never Removed: fsnotify's bookkeeping can alias it)
//   [x] Unhappy: a directory swapped after its watch is added is not
//              descended (the held walk)
//   [x] Unhappy: an event's containing directory, swapped for a symlink, is
//              not re-watched through it (readdVerified)
//   [x] Unhappy: an event on a root itself does not watch the root's parent
//   [x] Unhappy: a created directory reached through a swapped ancestor is
//              not watched (watchCreatedDir opens every component held)
//   [x] Happy: a created directory subtree is watched, excluded names aside
//   [x] Regression: a versioned layout re-pointed by a compat symlink does
//              not panic fsnotify (no Remove of an aliased path)
//   [x] Regression: a directory renamed behind a compat symlink keeps its
//              watch, and writes in it keep reloading
//   [x] Run carries out a rebuild left pending by NewWatcher
//   [x] Unhappy: a watched subtree moved out of the root loses its
//              descendants' watches (forgectl#796)
//   [x] Repeated rebuilds back off (settleDelay, and Run under a swap that
//              wins every pass)
//   [x] A pending rebuild walks every root once, and still watches a root
//              replaced since the last index
//   [x] Churn on non-markdown files does not postpone a pending rebuild
//   [x] Unhappy: an event named inside the root whose full name now
//              resolves, through a symlink anywhere on its path, to a path
//              relevance refuses (kqueue's own entry watches after a swap,
//              forgectl#865: outside, excluded, an OnlyFile root's sibling,
//              a leaf symlink, a *.md directory link) does not reload for
//              relevance; fails closed on a dangling symlink
//   [x] Regression: a watched directory moved out and replaced by a symlink
//              still rebuilds the watches though its Rename is stray, so a
//              later retarget into the tree cannot revive the moved-out
//              subtree's watches (PR #868 review); a directory named dir.md
//              counts as a directory
//   [x] A stray event on a doc the index lists (the doc replaced by a
//              symlink) reloads to drop it
//   [x] Happy: an event through an in-root compat or leaf symlink, to an
//              OnlyFile root's own file, or under a directory deleted
//              since, still reloads

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// swapFixture builds root/{a,b} and an outside directory holding out/c, and
// returns the canonical root and outside paths.
func swapFixture(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "b"),
		filepath.Join(outside, "c"),
	} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	writeFile(t, filepath.Join(root, "top.md"), "# Top\n")
	return root, outside
}

// swapForSymlink moves dir aside and puts a symlink to target in its place.
func swapForSymlink(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

// setWatchHook installs fn as testHookWatch for the rest of the test.
func setWatchHook(t *testing.T, fn func(path string, stage watchStage)) {
	t.Helper()
	prev := testHookWatch
	testHookWatch = fn
	t.Cleanup(func() { testHookWatch = prev })
}

// newUnstartedWatcher indexes root and builds a watcher over it without
// running it, so the test reads its watch list directly.
func newUnstartedWatcher(t *testing.T, root string, more ...string) *Watcher {
	t.Helper()
	idx, err := NewIndex(append([]string{root}, more...))
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	w, err := NewWatcher(NewStore(idx), NewBroker())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func requireWatched(t *testing.T, w *Watcher, paths ...string) {
	t.Helper()
	list := w.fsw.WatchList()
	for _, p := range paths {
		if !slices.Contains(list, p) {
			t.Errorf("%s is not watched, want it watched; watch list %q", p, list)
		}
	}
}

// requireNoEventFrom writes quiet, a new file that no watch may observe,
// then control, a new file under a watched directory, and fails if any event
// names quiet. It reads the watcher's raw event channel, so the watcher must
// not be running. The check is behavioral rather than a WatchList lookup
// because kqueue (macOS) records a watch added through a symlink under the
// link's name while binding it to the target, so the list alone cannot say
// what is watched there. The control write proves the watches were live, so
// silence about quiet is evidence and not a dead watcher.
//
// It first syncs with fsnotify's reaction to the swap (syncWithWatch), so
// the probe is written only after that reaction is over. On kqueue that
// reaction lists the swapped name through its new symlink and watches the
// outside entries it finds under in-root names; a probe created while it
// ran could be watched that way and report a write (forgectl#865). The
// watcher drops such events at delivery (strayEvent); this check is about
// the watch set, so it keeps the probe out of that race rather than
// tolerating the event.
func requireNoEventFrom(t *testing.T, w *Watcher, quiet, control string) {
	t.Helper()
	syncWithWatch(t, w)
	writeFile(t, quiet, "# Quiet\n")
	writeFile(t, control, "# Control\n")
	var sawControl bool
	deadline := time.After(recvTimeout)
	var settle <-chan time.Time
	for {
		select {
		case ev := <-w.fsw.Events:
			if filepath.Base(ev.Name) == filepath.Base(quiet) {
				t.Fatalf("got event %v for %s, a directory outside the root", ev, quiet)
			}
			if !sawControl && filepath.Base(ev.Name) == filepath.Base(control) {
				sawControl = true
				settle = time.After(quietWindow / 3)
			}
		case <-settle:
			return
		case <-deadline:
			if !sawControl {
				t.Fatalf("no event for the control write %s within %s; the watches were not live, so no silence proves anything", control, recvTimeout)
			}
			return
		}
	}
}

// syncWithWatch rewrites, in place, one doc each root indexed directly in
// it, and reads the watcher's raw event channel until a write event names
// each, failing after recvTimeout. The watcher must not be running.
//
// It syncs on a file that existed when the watches were registered, never
// on a new one: on kqueue a new file shows up only as its directory's
// NOTE_WRITE, which fsnotify turns into a Create by listing the directory,
// and that listing stops at the first entry it cannot open. A dangling
// symlink sorting before the new name is enough, so the Create never comes
// (forgectl#865, the macOS CI failure). A file present at registration has
// a watch of its own on kqueue (and is covered by its directory's watch on
// inotify), so rewriting it always raises a write named for it.
//
// Every swap in these tests changes an entry of a root, which fsnotify
// handles on the root's own watch. That watch's event was raised before
// the sync write, and both inotify's queue and kqueue's active list hand
// events back in the order they were raised, so once the sync event
// arrives fsnotify has finished reacting to the swap.
func syncWithWatch(t *testing.T, w *Watcher) {
	t.Helper()
	idx := w.store.Current()
	pending := map[string]bool{}
	for _, root := range idx.Roots() {
		doc := root.OnlyFile
		// Prefer a doc directly in the root: a doc below it could sit in a
		// directory the test swapped.
		for _, d := range idx.List() {
			if doc == "" && d.RootLabel == root.Label && filepath.Dir(d.AbsPath) == root.Path {
				doc = d.AbsPath
			}
		}
		if doc == "" {
			t.Fatalf("root %s indexed no doc to sync on", root.Path)
		}
		doc = filepath.Clean(doc)
		body, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", doc, err)
		}
		if err := os.WriteFile(doc, body, 0o600); err != nil { //nolint:gosec // G703: doc is an indexed doc under the test's own TempDir root
			t.Fatalf("WriteFile %s: %v", doc, err)
		}
		pending[doc] = true
	}
	deadline := time.After(recvTimeout)
	for len(pending) > 0 {
		select {
		case ev := <-w.fsw.Events:
			if ev.Has(fsnotify.Write) {
				delete(pending, ev.Name)
			}
		case <-deadline:
			t.Fatalf("no write event for the sync rewrite of %v within %s", pending, recvTimeout)
		}
	}
}

func requireNotWatched(t *testing.T, w *Watcher, paths ...string) {
	t.Helper()
	list := w.fsw.WatchList()
	for _, p := range paths {
		if slices.Contains(list, p) {
			t.Errorf("%s is watched, want no watch there; watch list %q", p, list)
		}
	}
}

// A directory swapped for a symlink after the walk listed it, but before
// its watch is added, is never added: the pre-Add identity check refuses it.
//
// Mutation that turns it red: drop addVerified's pre-Add namesDir check (the
// Add follows the symlink and root/a's watch binds the outside directory).
func TestWatcherRegister_DirSwappedBeforeCheck_IsNotAdded(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	setWatchHook(t, func(path string, stage watchStage) {
		if path == a && stage == stageBeforeCheck {
			swapForSymlink(t, a, outside)
		}
	})

	w := newUnstartedWatcher(t, root)

	if w.resetPending {
		t.Error("resetPending is set, want the swap refused before any Add")
	}
	requireWatched(t, w, root, filepath.Join(root, "b"))
	requireNoEventFrom(t, w, filepath.Join(outside, "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
}

// A directory swapped for a symlink between the pre-Add check and the Add
// binds a watch outside the root. The post-Add check notices, removes
// nothing (see replaceWatcher), and schedules a full rebuild, after which
// nothing outside the root is watched.
//
// Mutation that turns it red: drop addVerified's post-Add namesDir check
// (no rebuild is scheduled), or make replaceWatcher keep the old fsnotify
// watcher (the outside watch survives the reload).
func TestWatcherRegister_DirSwappedBeforeAdd_RebuildsWatches(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	setWatchHook(t, func(path string, stage watchStage) {
		if path == a && stage == stageBeforeAdd {
			swapForSymlink(t, a, outside)
		}
	})

	w := newUnstartedWatcher(t, root)
	if !w.resetPending {
		t.Fatal("resetPending is not set after the watched path changed during its Add")
	}
	w.reload() // what Run does once the debounce settles

	if w.resetPending {
		t.Error("resetPending still set after the reload's rebuild")
	}
	requireWatched(t, w, root, filepath.Join(root, "b"))
	requireNoEventFrom(t, w, filepath.Join(outside, "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
}

// Mutation that turns it red: walk with filepath.WalkDir again, which reads
// root/a by path after its watch is added and so lists and watches
// root/a/c, a directory outside the root.
func TestWatcherRegister_DirSwappedAfterAdd_IsNotDescended(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	setWatchHook(t, func(path string, stage watchStage) {
		if path == a && stage == stageAdded {
			swapForSymlink(t, a, outside)
		}
	})

	w := newUnstartedWatcher(t, root)

	requireWatched(t, w, root, filepath.Join(root, "b"))
	requireNoEventFrom(t, w, filepath.Join(outside, "c", "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
}

// Mutation that turns it red: drop readdVerified's pre-Add resolvesToItself
// check, so the re-Add of an event's directory binds a watch through the
// symlink.
func TestWatcherRefresh_SwappedEventDir_IsNotRewatched(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	w := newUnstartedWatcher(t, root)
	requireWatched(t, w, a)

	swapForSymlink(t, a, outside)
	w.refreshWatch(fsnotify.Event{Name: filepath.Join(a, "x.md"), Op: fsnotify.Write})

	requireNoEventFrom(t, w, filepath.Join(outside, "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
}

// Mutation that turns it red: walk a created directory with
// filepath.WalkDir(ev.Name) again, which resolves root/a by path through the
// symlink and watches root/a/c and root/a/c/deep outside the root.
func TestWatcherRefresh_CreatedDirUnderSwappedAncestor_IsNotWatched(t *testing.T) {
	root, outside := swapFixture(t)
	if err := os.MkdirAll(filepath.Join(outside, "c", "deep"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	a := filepath.Join(root, "a")
	w := newUnstartedWatcher(t, root)

	swapForSymlink(t, a, outside)
	created := filepath.Join(a, "c")
	w.refreshWatch(fsnotify.Event{Name: created, Op: fsnotify.Create})

	requireNoEventFrom(t, w, filepath.Join(outside, "c", "deep", "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
	requireNoEventFrom(t, w, filepath.Join(outside, "c", "probe-quiet2.md"), filepath.Join(root, "b", "probe-control2.md"))
}

// Mutation that turns it red: make watchCreatedDir a no-op, or stop its
// held open at the first component (fresh/deep goes unwatched). Watching
// node_modules under it again fails the excluded-name row.
func TestWatcherRefresh_CreatedDirSubtree_IsWatched(t *testing.T) {
	root, _ := swapFixture(t)
	w := newUnstartedWatcher(t, root)

	fresh := filepath.Join(root, "b", "fresh")
	for _, d := range []string{
		filepath.Join(fresh, "deep"),
		filepath.Join(fresh, "node_modules", "pkg"),
	} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	w.refreshWatch(fsnotify.Event{Name: fresh, Op: fsnotify.Create})

	requireWatched(t, w, fresh, filepath.Join(fresh, "deep"))
	requireNotWatched(t, w, filepath.Join(fresh, "node_modules"), filepath.Join(fresh, "node_modules", "pkg"))
}

// awaitReload waits up to recvTimeout for one reload notification.
func awaitReload(t *testing.T, sub <-chan string, what string) {
	t.Helper()
	select {
	case _, ok := <-sub:
		if !ok {
			t.Fatalf("broker channel closed, want a reload after %s", what)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("no reload within %s after %s", recvTimeout, what)
	}
}

// drainReloads swallows reload notifications until none has arrived for
// quietWindow/3, so the next awaitReload sees only what follows.
func drainReloads(sub <-chan string) {
	for {
		select {
		case <-sub:
		case <-time.After(quietWindow / 3):
			return
		}
	}
}

// A versioned layout re-pointed through a compat symlink (root/v1 moved to
// v1.old and replaced by a symlink to v2) makes the watch still named
// root/v1/guide resolve, by path, to root/v2/guide, which is already watched.
// An Add there aliases the second path onto the first watch in fsnotify's
// bookkeeping, and a Remove of either path then panics fsnotify (a nil
// watch in removePath), killing the Run goroutine and the server with it.
//
// Mutation that turns it red: remove the path's watch in readdVerified when
// it no longer resolves to itself, as the first fix did (the test binary
// panics in fsnotify's removePath).
func TestWatcher_VersionedCompatSymlink_DoesNotPanic(t *testing.T) {
	root, _ := swapFixture(t)
	for _, d := range []string{"v1/guide", "v2/guide"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		writeFile(t, filepath.Join(root, d, "page.md"), "# Page\n")
	}
	_, sub, _ := newTestWatcher(t, root)

	if err := os.Rename(filepath.Join(root, "v1"), filepath.Join(root, "v1.old")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Symlink("v2", filepath.Join(root, "v1")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	drainReloads(sub)

	writeFile(t, filepath.Join(root, "v1.old", "guide", "page.md"), "# Page\n\nedited\n")
	awaitReload(t, sub, "a write under the renamed-away v1.old/guide")
	drainReloads(sub)

	// The watcher survived: a later write elsewhere still reloads.
	writeFile(t, filepath.Join(root, "v2", "guide", "page.md"), "# Page\n\nv2 edited\n")
	awaitReload(t, sub, "a write under v2/guide")
}

// A directory renamed and given a compat symlink under its old name (mv
// root/a root/c; ln -s c root/a) keeps one watch, still named root/a but
// bound to root/c. An event under it must not make the watcher remove that
// watch: writes in root/c have to keep reloading.
//
// Mutation that turns it red: remove the path's watch in readdVerified when
// it no longer resolves to itself, as the first fix did (the Remove takes
// the one watch on root/c, and a write there goes silent).
func TestWatcher_RenameWithCompatSymlink_KeepsDelivering(t *testing.T) {
	root, _ := swapFixture(t)
	writeFile(t, filepath.Join(root, "a", "doc.md"), "# Doc\n")
	_, sub, _ := newTestWatcher(t, root)

	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "c")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Symlink("c", filepath.Join(root, "a")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	drainReloads(sub)

	for i, name := range []string{"doc.md", "doc.md", "new.md"} {
		writeFile(t, filepath.Join(root, "c", name), "# Doc\n\nedit\n")
		awaitReload(t, sub, "write "+string(rune('1'+i))+" in root/c")
		drainReloads(sub)
	}
}

// An event on a root itself (the root moved or replaced) names the root, so
// its containing directory is the root's parent, outside every root. It
// must not be watched (forgectl#796).
//
// Mutation that turns it red: drop readdVerified's insideSomeRoot gate.
func TestWatcherRefresh_RootSelfEvent_DoesNotWatchParent(t *testing.T) {
	root, _ := swapFixture(t)
	w := newUnstartedWatcher(t, root)

	w.refreshWatch(fsnotify.Event{Name: root, Op: fsnotify.Chmod})

	requireNotWatched(t, w, filepath.Dir(root))
	requireWatched(t, w, root)
}

// A rebuild scheduled while NewWatcher registered (before Run started) is
// carried out by Run itself, with no filesystem event needed to wake it.
//
// Mutation that turns it red: drop Run's start-up check of resetPending (no
// reload ever comes).
func TestWatcherRun_PendingResetFromNewWatcher_Reloads(t *testing.T) {
	root, _ := swapFixture(t)
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	broker := NewBroker()
	w, err := NewWatcher(NewStore(idx), broker)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.debounce = testDebounce
	// What addVerified leaves when a directory changes during an Add in
	// NewWatcher's registration; set directly so no filesystem event exists
	// to wake Run some other way.
	w.resetPending = true
	sub, unsubscribe := broker.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		unsubscribe()
		broker.Close()
		_ = w.Close()
	})
	go w.Run(ctx)

	awaitReload(t, sub, "a registration that left a watch rebuild pending")
}

// A renamed directory given a compat symlink under its old name keeps its
// one watch, still named for the old path. An event naming that old path
// must not make the watcher remove it: the path now resolves elsewhere, so
// the watcher neither re-adds nor removes anything. The watcher is not
// running, so fsnotify has not yet acted on the rename itself.
//
// Mutation that turns it red: remove the path's watch in readdVerified when
// it no longer resolves to itself, as the first fix did.
func TestWatcherRefresh_CompatSymlinkedDir_KeepsItsWatch(t *testing.T) {
	root, _ := swapFixture(t)
	a := filepath.Join(root, "a")
	w := newUnstartedWatcher(t, root)
	requireWatched(t, w, a)

	if err := os.Rename(a, filepath.Join(root, "c")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Symlink("c", a); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	w.refreshWatch(fsnotify.Event{Name: filepath.Join(a, "doc.md"), Op: fsnotify.Write})

	requireWatched(t, w, a)
}

// A watched subtree moved out of the root keeps its descendants' watches
// unless the watcher rebuilds them: inotify drops only the moved directory's
// own watch (IN_MOVE_SELF), and the watches below it still report under
// their old in-root names. A write there would then drive a reload, leaking
// reload timing for a file outside every root (forgectl#796). The move marks
// a watch rebuild pending, so the reload that follows starts from a fresh
// fsnotify watcher.
//
// Mutation that turns it red: drop the resetPending assignment on a
// dirMoved event in Run (the write under outside/a/sub reloads).
func TestWatcher_SubtreeMovedOutOfRoot_DropsDescendantWatches(t *testing.T) {
	root, outside := swapFixture(t)
	sub := filepath.Join(root, "a", "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(sub, "doc.md"), "# Doc\n")
	_, reloads, _ := newTestWatcher(t, root)

	moved := filepath.Join(outside, "a")
	if err := os.Rename(filepath.Join(root, "a"), moved); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	awaitReload(t, reloads, "moving root/a out of the root")
	drainReloads(reloads)

	writeFile(t, filepath.Join(moved, "sub", "secret.md"), "# Secret\n")
	select {
	case <-reloads:
		t.Fatal("a write under a subtree moved out of the root reloaded the reader; its descendant watches outlived the move")
	case <-time.After(quietWindow):
	}

	// CONTROL: the watcher is still live for the root itself.
	writeFile(t, filepath.Join(root, "top.md"), "# Top\n\nedited\n")
	awaitReload(t, reloads, "a write to root/top.md")
}

// settleDelay is the plain debounce unless a watch rebuild is pending
// within resetQuiet of the last one; then it doubles once more than the
// streak so far, and caps.
//
// Mutation that turns it red: return w.debounce unconditionally (no
// backoff), drop the maxResetBackoff cap, or back off with no rebuild
// pending (the plain-edit row).
func TestWatcherSettleDelay_BacksOffAndCaps(t *testing.T) {
	w := &Watcher{debounce: 100 * time.Millisecond}
	if got := w.settleDelay(); got != w.debounce {
		t.Errorf("settleDelay with no rebuild ever run = %v, want the debounce", got)
	}
	w.lastReset = time.Now()
	w.resetStreak = 3
	if got := w.settleDelay(); got != w.debounce {
		t.Errorf("settleDelay with no rebuild pending = %v, want the debounce: a plain edit is never backed off", got)
	}
	w.resetPending = true
	for streak, want := range []time.Duration{
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	} {
		w.resetStreak = streak
		if got := w.settleDelay(); got != want {
			t.Errorf("settleDelay at streak %d = %v, want %v", streak, got, want)
		}
	}
	w.resetStreak = 1000
	if got := w.settleDelay(); got != maxResetBackoff {
		t.Errorf("settleDelay at streak 1000 = %v, want the cap %v", got, maxResetBackoff)
	}
	w.lastReset = time.Now().Add(-resetQuiet)
	if got := w.settleDelay(); got != w.debounce {
		t.Errorf("settleDelay after resetQuiet = %v, want the debounce", got)
	}
}

// noteReset extends the streak for a rebuild within resetQuiet of the last,
// up to maxResetStreak, and starts it over after a quiet spell.
//
// Mutation that turns it red: drop the min against maxResetStreak (the
// streak grows past the cap).
func TestWatcherNoteReset_ClampsStreak(t *testing.T) {
	w := &Watcher{}
	t0 := time.Now()
	w.noteReset(t0)
	if w.resetStreak != 0 {
		t.Errorf("first rebuild's streak = %d, want 0", w.resetStreak)
	}
	w.noteReset(t0.Add(time.Second))
	if w.resetStreak != 1 {
		t.Errorf("streak after a rebuild within resetQuiet = %d, want 1", w.resetStreak)
	}
	w.resetStreak = maxResetStreak
	w.noteReset(t0.Add(2 * time.Second))
	if w.resetStreak != maxResetStreak {
		t.Errorf("streak past the cap = %d, want %d", w.resetStreak, maxResetStreak)
	}
	w.noteReset(t0.Add(2*time.Second + resetQuiet))
	if w.resetStreak != 0 {
		t.Errorf("streak after resetQuiet = %d, want 0", w.resetStreak)
	}
}

// A directory swap that wins the race against every registration pass
// leaves a watch rebuild pending after every reload. Run backs off instead
// of reloading at the debounce rate for as long as that lasts.
//
// Mutation that turns it red: drop Run's resetStreak increment, or reset
// the streak on any reload that ends clean (a reload every debounce or two,
// dozens in the window).
func TestWatcherRun_RepeatedReset_BacksOff(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	moved := a + ".moved"
	// Every registration pass swaps root/a for a symlink during its Add,
	// which marks a rebuild pending, then puts it back when the walk
	// reaches root/b, so the next pass meets it again.
	setWatchHook(t, func(path string, stage watchStage) {
		switch {
		case path == a && stage == stageBeforeAdd:
			_ = os.Rename(a, moved)
			_ = os.Symlink(outside, a)
		case path == filepath.Join(root, "b") && stage == stageBeforeCheck:
			if _, err := os.Stat(moved); err == nil {
				_ = os.Remove(a)
				_ = os.Rename(moved, a)
			}
		}
	})
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	broker := NewBroker()
	w, err := NewWatcher(NewStore(idx), broker)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if !w.resetPending {
		t.Fatal("the swapping hook did not leave a rebuild pending; the fixture exercises nothing")
	}
	w.debounce = testDebounce
	sub, unsubscribe := broker.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	// Run must have returned before setWatchHook's cleanup restores the
	// hook: cancel alone does not wait, and a reload still walking would
	// read the hook as it is restored (a data race under -race).
	runDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-runDone
		unsubscribe()
		broker.Close()
		_ = w.Close()
	})
	go func() {
		defer close(runDone)
		w.Run(ctx)
	}()

	// 20 ms doubling from 40 ms: a reload at about 20, 60, 140, 300, 620 and
	// 1260 ms, so about six in the window, plus any the swap's own events
	// arm. Without backoff it is one per debounce or two.
	window := time.After(1500 * time.Millisecond)
	reloads := 0
	for done := false; !done; {
		select {
		case <-sub:
			reloads++
		case <-window:
			done = true
		}
	}
	t.Logf("%d reloads in the window", reloads)
	if reloads < 2 {
		t.Fatalf("%d reloads in the window; the pending rebuild never repeated, so nothing was measured", reloads)
	}
	if reloads > 12 {
		t.Errorf("%d reloads in 1.5s under a rebuild that stays pending; want backoff (at most about 7)", reloads)
	}
}

// A pending watch rebuild walks every root once: the fresh watcher is
// registered before the index rebuild, and not again after it.
//
// Mutation that turns it red: register the fresh index in full after the
// rebuild whatever the reset pass did (the root is walked twice).
func TestWatcherReload_PendingReset_RegistersOnce(t *testing.T) {
	root, _ := swapFixture(t)
	w := newUnstartedWatcher(t, root)
	walks := 0
	setWatchHook(t, func(path string, stage watchStage) {
		if path == root && stage == stageBeforeCheck {
			walks++
		}
	})
	w.resetPending = true
	w.reload()
	if walks != 1 {
		t.Errorf("a reload with a watch rebuild pending walked the root %d times, want 1", walks)
	}
	requireWatched(t, w, root, filepath.Join(root, "a"), filepath.Join(root, "b"))
}

// A root replaced by a new directory since the last index cannot be opened
// through the current index's pin, so the rebuild's pass before the index
// rebuild skips it. The fresh index pins the new directory, and the reload
// registers that root from it.
//
// Mutation that turns it red: skip the post-rebuild registration of the
// roots the reset pass could not open (the new root is never watched).
func TestWatcherReload_PendingReset_RegistersReplacedRoot(t *testing.T) {
	root, _ := swapFixture(t)
	w := newUnstartedWatcher(t, root)
	if err := os.Rename(root, root+".old"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "fresh"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(root, "doc.md"), "# Doc\n")
	w.resetPending = true
	w.reload()
	requireWatched(t, w, root, filepath.Join(root, "fresh"))
}

// While a watch rebuild is pending and its reload is armed, an event that
// is not otherwise relevant does not re-arm the timer: churn on other files
// cannot keep postponing the rebuild.
//
// Mutation that turns it red: re-arm on every event while resetPending is
// set (the reload waits for the churn to stop).
func TestWatcherRun_IrrelevantChurn_DoesNotPostponeReset(t *testing.T) {
	root, _ := swapFixture(t)
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	broker := NewBroker()
	w, err := NewWatcher(NewStore(idx), broker)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.debounce = 200 * time.Millisecond
	w.resetPending = true
	sub, unsubscribe := broker.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		unsubscribe()
		broker.Close()
		_ = w.Close()
	})
	go w.Run(ctx)

	churn := filepath.Join(root, "notes.txt")
	stop := time.After(2 * time.Second)
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sub:
			return // the rebuild ran while the churn went on
		case <-tick.C:
			writeFile(t, churn, time.Now().String())
		case <-stop:
			t.Fatal("no reload during 2s of churn on a non-markdown file; each event postponed the pending rebuild")
		}
	}
}

// startWatcher runs w with the test debounce and returns a reload
// subscription; the watcher stops with the test.
func startWatcher(t *testing.T, w *Watcher) <-chan string {
	t.Helper()
	w.debounce = testDebounce
	sub, unsubscribe := w.broker.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-runDone
		unsubscribe()
		w.broker.Close()
	})
	go func() {
		defer close(runDone)
		w.Run(ctx)
	}()
	return sub
}

// injectEvent hands ev to the running watcher as though fsnotify had
// delivered it. It stands in for kqueue, which (unlike inotify) names an
// outside file's write under an in-root path after a directory swap
// (forgectl#865), so the delivery-time filter is exercised on every
// platform.
func injectEvent(t *testing.T, w *Watcher, ev fsnotify.Event) {
	t.Helper()
	w.mu.Lock()
	events := w.fsw.Events
	w.mu.Unlock()
	select {
	case events <- ev:
	case <-time.After(recvTimeout):
		t.Fatalf("the watcher did not take the injected event %v within %s", ev, recvTimeout)
	}
}

// requireNoReload fails if a reload arrives within quietWindow.
func requireNoReload(t *testing.T, sub <-chan string, what string) {
	t.Helper()
	select {
	case <-sub:
		t.Fatalf("a reload followed %s", what)
	case <-time.After(quietWindow):
	}
}

// An event named inside the root whose full name now resolves, through a
// symlink anywhere on its path, to a path relevance refuses is what kqueue
// delivers for an outside file after a swap (forgectl#865). It must not
// reload: through a swapped directory to outside, into an excluded
// directory, to an OnlyFile root's sibling file, through a leaf symlink to
// an outside file, through a directory named *.md that links outside, or
// through a dangling symlink. An event through a compat symlink or a leaf
// symlink to a doc inside the root, to an OnlyFile root's own file, and
// one under a directory deleted since, still reload.
//
// Mutation that turns it red: skip Run's stray branch of the relevance
// decision (the outside rows reload); make strayEvent true for any symlinked
// path (the compat, alias and only-file controls go silent); make
// resolveNow fail on a missing path (the deleted control goes silent);
// make strayEvent return false on a resolution error (the dangling row
// reloads); resolve only the event's directory and join the base back on
// (the leaf and *.md-directory rows reload); or accept any resolved path
// inside some root (the OnlyFile-sibling and vendored rows reload).
func TestWatcherRun_EventThroughSwappedDir_DoesNotReload(t *testing.T) {
	root, outside := swapFixture(t)
	for _, d := range []string{"c", "node_modules/pkg"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	only := filepath.Join(filepath.Dir(root), "only")
	writeFile(t, filepath.Join(only, "x.md"), "# X\n")
	writeFile(t, filepath.Join(only, "other.md"), "# Other\n")
	w := newUnstartedWatcher(t, root, filepath.Join(only, "x.md"))
	a := filepath.Join(root, "a")
	swapForSymlink(t, a, outside)
	for link, target := range map[string]string{
		"compat":   filepath.Join(root, "c"),
		"vendored": filepath.Join(root, "node_modules", "pkg"),
		"dangling": filepath.Join(outside, "gone"),
		"onlylink": only,
		"leak.md":  filepath.Join(outside, "secret.md"),
		"dir.md":   outside,
		"alias.md": filepath.Join(root, "c", "doc.md"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}
	writeFile(t, filepath.Join(outside, "secret.md"), "# Secret\n")
	writeFile(t, filepath.Join(root, "c", "doc.md"), "# Doc\n")
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "doc.md"), "# Doc\n")
	syncWithWatch(t, w)
	sub := startWatcher(t, w)
	drainReloads(sub)

	for _, name := range []string{
		filepath.Join(a, "secret.md"),
		filepath.Join(root, "vendored", "doc.md"),
		filepath.Join(root, "dangling", "doc.md"),
		filepath.Join(root, "onlylink", "other.md"),
		filepath.Join(root, "leak.md"),
		filepath.Join(root, "dir.md"),
	} {
		injectEvent(t, w, fsnotify.Event{Name: name, Op: fsnotify.Write})
		requireNoReload(t, sub, "a write event named "+name)
	}

	// CONTROLS: the gate still passes events it must.
	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "compat", "doc.md"), Op: fsnotify.Write})
	awaitReload(t, sub, "a write through a compat symlink inside the root")
	drainReloads(sub)
	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "alias.md"), Op: fsnotify.Write})
	awaitReload(t, sub, "a write on a leaf symlink to a doc inside the root")
	drainReloads(sub)
	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "onlylink", "x.md"), Op: fsnotify.Write})
	awaitReload(t, sub, "a write through a symlink to an OnlyFile root's own file")
	drainReloads(sub)
	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(root, "gone", "deeper", "doc.md"), Op: fsnotify.Remove})
	awaitReload(t, sub, "a remove under a directory deleted since")
}

// A watched directory moved out of the root and replaced at once by a
// symlink to an outside decoy delivers its Rename as a stray event (its
// name now resolves outside). It is still a watched directory moving, and
// must rebuild the watches: otherwise its descendants' watches survive,
// still named inside the root, and once the link is retargeted into the
// tree a write under the moved-out subtree resolves in-tree and reloads
// (forgectl#796, reopened by coupling move detection to stray in PR #868).
// A real directory named dir.md is a directory all the same.
//
// Mutation that turns it red: compute moved as !stray && w.dirMoved(ev)
// in Run (the stray Rename then gets only the silent watch rebuild, so no
// reload comes and the index keeps the moved-out doc).
func TestWatcher_MovedOutThenRelinked_DropsDescendantWatches(t *testing.T) {
	for _, name := range []string{"a", "dir.md"} {
		t.Run(name, func(t *testing.T) {
			root, outside := swapFixture(t)
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			writeFile(t, filepath.Join(dir, "sub", "doc.md"), "# Doc\n")
			decoy := filepath.Join(outside, "decoy")
			if err := os.MkdirAll(decoy, 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			w := newUnstartedWatcher(t, root)
			requireWatched(t, w, dir, filepath.Join(dir, "sub"))

			// Move out and link to the decoy before Run sees the Rename, so
			// the Rename is judged against the symlink.
			moved := filepath.Join(outside, "moved")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatalf("Rename: %v", err)
			}
			if err := os.Symlink(decoy, dir); err != nil {
				t.Skipf("symlinks unavailable here: %v", err)
			}
			reloads := startWatcher(t, w)
			// The move is a move though its Rename is stray: it reloads, and
			// the reload drops the moved-out doc from the index.
			awaitReload(t, reloads, "a watched directory moved out and replaced by a symlink")
			drainReloads(reloads)
			if _, ok := w.store.Current().FindByAbsPath(filepath.Join(dir, "sub", "doc.md")); ok {
				t.Error("the index still lists a doc moved out of the root")
			}

			// Retarget the link into the tree, where the moved-out subtree's
			// names would now resolve.
			if err := os.Remove(dir); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if err := os.Symlink(filepath.Join(root, "b"), dir); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			drainReloads(reloads)

			writeFile(t, filepath.Join(moved, "sub", "secret.md"), "# Secret\n")
			requireNoReload(t, reloads, "a write under a subtree moved out of the root")

			// CONTROL: the watcher is still live for the root.
			writeFile(t, filepath.Join(root, "top.md"), "# Top\n\nedited\n")
			awaitReload(t, reloads, "a write to root/top.md")
		})
	}
}

// A doc replaced by a symlink to an outside file delivers stray events
// only, but the index still lists it: the watcher reloads once to drop it.
//
// Mutation that turns it red: drop the FindByAbsPath branch in Run (no
// reload comes, and the index keeps listing top.md).
func TestWatcherRun_DocReplacedBySymlink_Reloads(t *testing.T) {
	root, outside := swapFixture(t)
	writeFile(t, filepath.Join(outside, "secret.md"), "# Secret\n")
	w := newUnstartedWatcher(t, root)
	top := filepath.Join(root, "top.md")
	if _, ok := w.store.Current().FindByAbsPath(top); !ok {
		t.Fatal("top.md is not indexed; the fixture exercises nothing")
	}
	if err := os.Remove(top); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), top); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	reloads := startWatcher(t, w)

	awaitReload(t, reloads, "an indexed doc replaced by a symlink")
}

// A stray event means fsnotify holds a watch bound somewhere its name does
// not lead. On kqueue, a doc swapped for a symlink to an outside file and
// swapped back keeps a watch on the outside file under the doc's own name,
// which a re-Add reuses and nothing else drops. So any stray event
// schedules a rebuild of the watches in a fresh fsnotify watcher, and the
// rebuild is silent: it neither rebuilds the index nor publishes a reload.
//
// Mutation that turns it red: drop the resetPending assignment in Run's
// stray branch (the fsnotify watcher is never replaced), or publish that
// rebuild (a reload follows the stray event).
func TestWatcherRun_StrayEvent_RebuildsWatchesSilently(t *testing.T) {
	root, outside := swapFixture(t)
	w := newUnstartedWatcher(t, root)
	a := filepath.Join(root, "a")
	swapForSymlink(t, a, outside)
	// Consume the swap's own events, so Run meets no real move.
	syncWithWatch(t, w)
	w.mu.Lock()
	before := w.fsw
	w.mu.Unlock()
	reloads := startWatcher(t, w)

	injectEvent(t, w, fsnotify.Event{Name: filepath.Join(a, "secret.md"), Op: fsnotify.Write})
	deadline := time.Now().Add(recvTimeout)
	for {
		w.mu.Lock()
		replaced := w.fsw != before
		w.mu.Unlock()
		if replaced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fsnotify watcher was not replaced within %s of a stray event", recvTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	requireNoReload(t, reloads, "the watch rebuild a stray event scheduled")
	requireWatched(t, w, root, filepath.Join(root, "b"))
}
