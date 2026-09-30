package docs

// Test plan for the watcher's registration walk (forgectl#769)
//
// Watcher registration (Classification: security boundary, low severity —
// a stray watch can only drive extra rebuilds)
//   [x] Unhappy: a directory swapped for a symlink to an outside directory
//              just before its watch is added keeps no watch (addVerified)
//   [x] Unhappy: a directory swapped for a symlink after its watch is added
//              is not descended, so nothing outside the root is watched
//              through it (the held walk)
//   [x] Unhappy: an event's containing directory, swapped for a symlink, is
//              not re-watched through it (readdVerified)
//   [x] Unhappy: a created directory reached through a swapped ancestor is
//              not watched (watchCreatedDir opens every component held)
//   [x] Happy: a created directory subtree is watched, excluded names aside

import (
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
func setWatchHook(t *testing.T, fn func(path string, added bool)) {
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

// Mutation that turns it red: drop addVerified's post-Add SameFile check
// (root/a stays watched, bound to the outside directory, so a write there
// raises an event).
func TestWatcherRegister_DirSwappedBeforeAdd_KeepsNoWatch(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	setWatchHook(t, func(path string, added bool) {
		if path == a && !added {
			swapForSymlink(t, a, outside)
		}
	})

	w := newUnstartedWatcher(t, root)

	requireWatched(t, w, root, filepath.Join(root, "b"))
	requireNoEventFrom(t, w, filepath.Join(outside, "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
}

// Mutation that turns it red: walk with filepath.WalkDir again, which reads
// root/a by path after its watch is added and so lists and watches
// root/a/c, a directory outside the root.
func TestWatcherRegister_DirSwappedAfterAdd_IsNotDescended(t *testing.T) {
	root, outside := swapFixture(t)
	a := filepath.Join(root, "a")
	setWatchHook(t, func(path string, added bool) {
		if path == a && added {
			swapForSymlink(t, a, outside)
		}
	})

	w := newUnstartedWatcher(t, root)

	requireWatched(t, w, root, filepath.Join(root, "b"))
	requireNoEventFrom(t, w, filepath.Join(outside, "c", "probe-quiet.md"), filepath.Join(root, "b", "probe-control.md"))
}

// Mutation that turns it red: drop readdVerified's EvalSymlinks check, so
// the re-Add of an event's directory keeps a watch bound through a symlink.
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
