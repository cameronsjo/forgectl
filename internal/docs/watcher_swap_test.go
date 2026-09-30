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
func newUnstartedWatcher(t *testing.T, root string) *Watcher {
	t.Helper()
	idx, err := NewIndex([]string{root})
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
func requireNoEventFrom(t *testing.T, w *Watcher, quiet, control string) {
	t.Helper()
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
// nothing (see resetWatches), and schedules a full rebuild, after which
// nothing outside the root is watched.
//
// Mutation that turns it red: drop addVerified's post-Add namesDir check
// (no rebuild is scheduled), or make resetWatches keep the old fsnotify
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
