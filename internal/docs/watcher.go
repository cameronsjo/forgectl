package docs

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce is how long the watcher waits for the filesystem to go quiet
// before rebuilding the index and notifying browsers.
//
// A debounce is not a nicety here. A single logical "save" is routinely several
// filesystem events: editors that write-then-rename produce a Create plus a
// Rename plus a Chmod, and an agent writing a document may touch it repeatedly
// within a few hundred milliseconds. Without coalescing, each event would
// trigger a full index rebuild (a walk of every root) and a browser reload,
// so a burst of writes would show up as a flickering page that reloads out
// from under the reader mid-sentence.
const DefaultDebounce = 150 * time.Millisecond

// maxResetBackoff caps settleDelay. Watch rebuilds that keep coming (a swap
// racing every registration pass, or directories moved over and over)
// double the wait before each further one up to this, instead of reloading
// at the debounce rate for as long as they keep coming.
const maxResetBackoff = 5 * time.Second

// resetQuiet is how long after a watch rebuild the next one still counts as
// part of the same run for settleDelay's backoff. It exceeds
// maxResetBackoff, so rebuilds arriving at the capped rate stay capped.
const resetQuiet = 2 * maxResetBackoff

// Watcher watches every indexed root for markdown changes, rebuilds the Index
// when one lands, installs it in a Store, and notifies connected browsers
// through a Broker.
//
// Watches are registered per DIRECTORY, never per file. Per-file watches are
// the obvious-looking design and the wrong one: the common editor and agent
// write pattern is write-to-temp-then-rename, which replaces the file's inode,
// and a watch bound to the old inode goes silently deaf — the page simply stops
// updating with no error anywhere. Watching the containing directory instead
// observes the rename as an event in its own right. On every event the
// containing directory is re-Add'ed for the same reason one level up: a
// directory can itself be replaced.
//
// Note this is NOT a fix for the "atomic replace breaks file watching" folklore
// bug — that specific claim was investigated and found false. Directory-level
// watching is here because it is the correct shape for rename-based writes, not
// as a workaround for a defect.
type Watcher struct {
	// mu guards fsw and closed between Close and replaceWatcher. Only the Run
	// goroutine (or NewWatcher, before Run starts) replaces fsw, so Run reads
	// it without the lock.
	mu       sync.Mutex
	fsw      *fsnotify.Watcher
	closed   bool
	store    *Store
	broker   *Broker
	debounce time.Duration

	// resetPending records that a watch was added through a path that
	// stopped naming its directory, or that a watched directory moved.
	// fsnotify's bookkeeping for such a watch cannot be trusted, and a moved
	// directory's descendants keep their watches wherever it went, so the
	// next reload rebuilds every watch in a fresh fsnotify watcher
	// (replaceWatcher). Touched only by the goroutine that registers watches.
	resetPending bool

	// resetStreak counts the watch rebuilds in the current run, each within
	// resetQuiet of the one before, and lastReset is when the latest one
	// ran. settleDelay backs off on them. Touched only by Run.
	resetStreak int
	lastReset   time.Time

	// dirs is every directory path addVerified watched since the last
	// replaceWatcher. A Rename naming one of them means a watched directory
	// moved, which a reload must pick up (dirMoved). Touched only by the
	// goroutine that registers watches.
	dirs map[string]struct{}
}

// NewWatcher creates a watcher over the roots of store's current index and
// registers watches immediately, so a change between construction and Run
// cannot be missed. Call Run to start processing, and Close to release the
// underlying OS handles.
func NewWatcher(store *Store, broker *Broker) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fsw: fsw, store: store, broker: broker, debounce: DefaultDebounce}
	w.register(store.Current())
	return w, nil
}

// Close releases the watcher's OS resources.
func (w *Watcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return w.fsw.Close()
}

// replaceWatcher swaps the fsnotify watcher for a fresh one holding no
// watches, and reports whether it did; the caller registers the roots in
// it. It is the recovery for a watch whose path stopped naming the
// directory it was added for, and for a moved directory whose descendants
// kept their watches. Removing those watches one by one is not safe: when a
// path comes to resolve to a directory already watched under another path,
// inotify hands back the other path's watch descriptor, and fsnotify
// v1.10.1 then records the two paths inconsistently. A later Remove of
// either path can remove the other path's only watch, or dereference a
// watch fsnotify already deleted and panic (forgectl#769 review). Closing
// the whole watcher releases every descriptor at once and needs none of that
// bookkeeping to be right.
func (w *Watcher) replaceWatcher() bool {
	w.resetPending = false
	fresh, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("docs: could not rebuild the live-reload watcher; keeping the current one.", "error", err)
		return false
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		_ = fresh.Close()
		return false
	}
	old := w.fsw
	w.fsw = fresh
	w.mu.Unlock()
	_ = old.Close()
	return true
}

// register walks every root in idx and adds a watch for each directory the
// indexer would descend into. Failures are logged and skipped rather than
// returned: a single unreadable subdirectory should cost live reload for that
// subtree, not refuse to start the reader at all.
//
// Safe to call repeatedly — fsnotify treats a duplicate Add as an update, and
// watches on deleted directories are dropped by the kernel — so this doubles
// as the re-registration pass after a rebuild picks up new directories.
func (w *Watcher) register(idx *Index) {
	w.dirs = nil // this pass re-adds every directory still watched
	w.registerRoots(idx.Roots())
}

// registerRoots watches every root in roots, as register does, and returns
// the paths of the roots it could open and walk.
func (w *Watcher) registerRoots(roots []Root) map[string]bool {
	done := make(map[string]bool, len(roots))
	for _, root := range roots {
		rt, err := openPinnedRoot(root)
		if err != nil {
			slog.Debug("docs: live-reload registration could not open a root.", "root", root.Label, "error", err)
			continue
		}
		if err := w.watchTree(rt, root.Path); err != nil {
			slog.Debug("docs: live-reload registration walk failed for a root.", "root", root.Label, "error", err)
		}
		_ = rt.Close()
		done[root.Path] = true
	}
	return done
}

// watchTree adds a watch for top, the directory dir holds, and for every
// directory below it that the indexer would descend into (forgectl#769).
//
// The walk is the index's own held walk (walkHeld). Every directory is listed
// through an os.Root opened in the one above it, and it must be the directory
// its Lstat saw. So a directory swapped for a symlink mid-walk is neither
// listed nor descended, and the walk cannot wander outside the root.
// fsnotify only adds a watch by path, though, so each Add goes through
// addVerified, which drops a watch whose path no longer names the directory
// the walk listed. top is exempt from the excluded-name rule, as a root is
// in walkRoot.
func (w *Watcher) watchTree(dir *os.Root, top string) error {
	return walkHeld(dir, top, func(path string, _ *os.Root, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // unreadable subtree: skip it, keep the rest of the walk
		}
		if path != top && excludedDir(d.Name()) {
			return filepath.SkipDir
		}
		want, err := d.Info()
		if err != nil || !want.IsDir() || !w.addVerified(path, want) {
			return filepath.SkipDir
		}
		return nil
	})
}

// watchStage names the points in addVerified that testHookWatch runs at.
type watchStage int

const (
	stageBeforeCheck watchStage = iota // before the pre-Add identity check
	stageBeforeAdd                     // after that check, before the Add
	stageAdded                         // after the watch passed the post-Add check
)

// testHookWatch, when set, runs at each watchStage of addVerified. Tests use
// it to swap a directory at exactly those points.
var testHookWatch func(path string, stage watchStage)

// addVerified adds a watch on path only if path names want, the directory
// the held walk listed, and reports whether it did. fsnotify.Add resolves
// path itself and follows a symlink, so a directory swapped for a symlink
// to an outside directory between the walk and the Add would otherwise
// leave a watch outside the root, and changes there would drive rebuilds.
//
// The identity is checked before the Add, which skips the swapped case
// outright, and again after it. A mismatch after the Add means the path
// changed in between. The watch is then left alone rather than removed
// (see replaceWatcher for why a Remove is unsafe), and the next reload
// rebuilds every watch from scratch. A swap and swap back around the Add
// passes both checks, so nothing marks it: that watch stays bound outside
// the root until some later reset replaces the whole fsnotify watcher (a
// mismatch like the one above, or a watched directory moving) or the
// reader closes. A reload's ordinary register pass does not clear it: it
// re-Adds the path, which now names the real directory, and the stray
// descriptor stays with the kernel. fsnotify has no by-handle Add, but a
// watch can only ever cause a rebuild, never a read: the rebuild walks
// through the pinned roots, and every serve re-verifies the path.
func (w *Watcher) addVerified(path string, want fs.FileInfo) bool {
	if testHookWatch != nil {
		testHookWatch(path, stageBeforeCheck)
	}
	if !namesDir(path, want) {
		return false
	}
	if testHookWatch != nil {
		testHookWatch(path, stageBeforeAdd)
	}
	if err := w.fsw.Add(path); err != nil {
		slog.Debug("docs: could not watch directory for live reload.", "path", path, "error", err)
		return false
	}
	if !namesDir(path, want) {
		w.resetPending = true
		slog.Debug("docs: a directory changed while its live-reload watch was added; rebuilding the watches on the next reload.", "path", path)
		return false
	}
	if testHookWatch != nil {
		testHookWatch(path, stageAdded)
	}
	if w.dirs == nil {
		w.dirs = map[string]struct{}{}
	}
	w.dirs[path] = struct{}{}
	return true
}

// namesDir reports whether path, looked up now, is want itself and not a
// symlink to it. The Lstat resolves every component but the last, so an
// ancestor swapped for a symlink that leads anywhere else fails the
// SameFile check too.
func namesDir(path string, want fs.FileInfo) bool {
	got, err := os.Lstat(path)
	return err == nil && got.IsDir() && os.SameFile(want, got)
}

// readdVerified re-adds a watch on dir, an event's containing directory,
// when dir lies inside a root and still resolves to itself. The root paths
// are canonical and every watched directory was reached by real names below
// one, so a watched path that no longer resolves to itself has had a
// component swapped for a symlink, and an Add would follow it. The root
// gate keeps an event on a root itself (a root moved or replaced) from
// watching the root's parent, which lies outside every root.
//
// Like addVerified, it never removes a watch. A path that stops resolving
// to itself between the check and the Add is left for the next reload's
// full rebuild.
func (w *Watcher) readdVerified(dir string) {
	if !w.insideSomeRoot(dir) || !resolvesToItself(dir) {
		return
	}
	if err := w.fsw.Add(dir); err != nil {
		return // best-effort; a removed dir legitimately fails here
	}
	if !resolvesToItself(dir) {
		w.resetPending = true
	}
}

// resolvesToItself reports whether dir has no symlink on its path.
func resolvesToItself(dir string) bool {
	resolved, err := filepath.EvalSymlinks(dir)
	return err == nil && resolved == dir
}

// insideSomeRoot reports whether path is a root of the current index or
// lies below one, lexically.
func (w *Watcher) insideSomeRoot(path string) bool {
	for _, root := range w.store.Current().Roots() {
		if withinRoot(root.Path, path) {
			return true
		}
	}
	return false
}

// watchCreatedDir watches a directory created at path, and its subtree,
// under every root that holds it. The directory is opened through the
// pinned root one held component at a time, so a symlink anywhere on the
// way is refused rather than followed, and a component the indexer would
// not descend into stops the registration.
func (w *Watcher) watchCreatedDir(path string) {
	for _, root := range w.store.Current().Roots() {
		if path == root.Path || !withinRoot(root.Path, path) {
			continue
		}
		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			continue
		}
		rt, err := openPinnedRoot(root)
		if err != nil {
			continue
		}
		dir := rt
		for depth, name := range strings.Split(rel, string(filepath.Separator)) {
			if excludedDir(name) {
				if dir != rt {
					_ = dir.Close()
				}
				dir = nil
				break
			}
			sub, err := openHeldSubdir(dir, name, depth)
			if dir != rt {
				_ = dir.Close()
			}
			if err != nil {
				dir = nil
				break
			}
			dir = sub
		}
		if dir != nil {
			_ = w.watchTree(dir, path) //nolint:errcheck // best-effort, as register is
			if dir != rt {
				_ = dir.Close()
			}
		}
		_ = rt.Close()
	}
}

// Run processes filesystem events until ctx is canceled or the underlying
// watcher closes. It coalesces bursts through w.debounce and, for each settled
// burst, rebuilds the index, swaps it into the Store, and publishes one reload
// notification.
func (w *Watcher) Run(ctx context.Context) {
	var (
		timer    *time.Timer
		settledC <-chan time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	// A registration in NewWatcher that met a changing directory has a
	// watch rebuild waiting. Run the reload that performs it.
	if w.resetPending {
		timer = time.NewTimer(w.debounce)
		settledC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// Keep the watch set current before deciding relevance: a newly
			// created directory needs watching even when the event that
			// revealed it carries no markdown of its own, and the containing
			// directory may itself have just been replaced.
			wasPending := w.resetPending
			w.refreshWatch(ev)
			moved := w.dirMoved(ev)
			if moved {
				// The moved tree's descendants keep their watches wherever
				// it went, outside the root or into an excluded directory;
				// only a fresh watcher drops them (forgectl#796).
				w.resetPending = true
			}

			// A reset already pending with its reload armed is not re-armed
			// by an event that is not otherwise relevant, so churn on other
			// files cannot keep postponing it.
			resetNeedsArming := w.resetPending && (!wasPending || settledC == nil)
			if !w.relevant(ev.Name) && !moved && !resetNeedsArming {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(w.settleDelay())
			} else {
				timer.Reset(w.settleDelay())
			}
			settledC = timer.C

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("docs: live-reload watcher reported an error.", "error", err)

		case <-settledC:
			settledC = nil
			if w.resetPending {
				now := time.Now()
				if !w.lastReset.IsZero() && now.Sub(w.lastReset) < resetQuiet {
					w.resetStreak++
				} else {
					w.resetStreak = 0
				}
				w.lastReset = now
			}
			w.reload()
			// A rebuild that itself met a changing directory waits again
			// rather than looping, backed off by settleDelay.
			if w.resetPending {
				timer.Reset(w.settleDelay())
				settledC = timer.C
			}
		}
	}
}

// settleDelay is how long Run waits for quiet before a reload. With no
// watch rebuild pending, or none run within resetQuiet, it is the debounce.
// A pending rebuild that follows a recent one waits the debounce doubled
// once more than the last, up to maxResetBackoff: a directory swap that wins
// the race against every registration pass would otherwise drive a reload
// per debounce for as long as it keeps winning. The streak spans reloads
// that happen to end clean in between, so alternating does not reset it.
func (w *Watcher) settleDelay() time.Duration {
	d := w.debounce
	if !w.resetPending || w.lastReset.IsZero() || time.Since(w.lastReset) >= resetQuiet {
		return d
	}
	for i := 0; i <= w.resetStreak && d < maxResetBackoff; i++ {
		d = min(2*d, maxResetBackoff)
	}
	return d
}

// refreshWatch keeps the watch set aligned with the tree: it re-Adds the
// event's containing directory (cheap, and it re-establishes a watch on a
// directory that was itself replaced) and registers a newly created directory
// subtree. A created directory that the indexer would refuse to descend into is
// deliberately NOT watched — see relevant() for why that matters.
func (w *Watcher) refreshWatch(ev fsnotify.Event) {
	if dir := filepath.Dir(ev.Name); dir != "" {
		w.readdVerified(dir)
	}

	if !ev.Has(fsnotify.Create) {
		return
	}
	info, err := os.Lstat(ev.Name)
	if err != nil || !info.IsDir() {
		return
	}
	if excludedDir(filepath.Base(ev.Name)) {
		return
	}
	// A directory arriving whole (an mv of a populated tree) can surface as a
	// single Create with no per-file events, so walk and watch it now.
	w.watchCreatedDir(ev.Name)
}

// dirMoved reports whether ev is a Rename of a directory addVerified
// watched. Moving a watched directory changes the doc set under it, and it
// can cost the moved tree its watch: inotify reports IN_MOVE_SELF on the
// moved directory's own watch, and fsnotify then removes that watch, even
// when a Create for the directory's new name has already re-added the same
// descriptor. The watches below it stay, still named for their old paths
// inside the root, wherever the tree went. So Run marks a watch rebuild
// pending, and the reload that follows registers every root in a fresh
// watcher: the moved tree is watched again if it is still inside a root,
// and its descendants' old watches are gone if it is not. The directory was
// watched, so the indexer descends into it, and a reload leaks nothing
// relevant would withhold.
func (w *Watcher) dirMoved(ev fsnotify.Event) bool {
	if !ev.Has(fsnotify.Rename) {
		return false
	}
	_, ok := w.dirs[ev.Name]
	return ok
}

// relevant reports whether an event path should trigger a reload.
//
// It reuses AllowedExt and excludedDir rather than restating either, so the
// watcher cannot disagree with the indexer about what counts as a doc. The
// exclusion half is a security check, not a performance one: without it, a
// write under .trash/ or node_modules/ would wake the reader up and rebuild the
// index on behalf of a file the reader will then correctly refuse to serve —
// leaking, through reload timing alone, the fact that something changed in a
// directory the user asked to be excluded.
//
// The check is lexical on purpose. A deleted or renamed-away path cannot be
// stat'ed or symlink-resolved, and those are exactly the events that must still
// trigger a rebuild, so relevance is decided from the path string against the
// already-canonical root paths.
func (w *Watcher) relevant(path string) bool {
	if !AllowedExt(path) {
		return false
	}

	idx := w.store.Current()
	for _, root := range idx.Roots() {
		if !withinRoot(root.Path, path) {
			continue
		}
		// Naming a single file must not make its siblings live-reloadable any
		// more than it makes them servable.
		if root.OnlyFile != "" {
			return path == root.OnlyFile
		}
		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return false
		}
		// Directory components only — walkRoot excludes hidden DIRECTORIES,
		// not a file that merely happens to start with a dot, so the final
		// segment (the filename) is not subject to the rule.
		segments := strings.Split(filepath.ToSlash(rel), "/")
		for _, dir := range segments[:len(segments)-1] {
			if excludedDir(dir) {
				return false
			}
		}
		return true
	}
	return false // outside every configured root
}

// reload rebuilds the index and, on success, installs it and notifies
// subscribers. A failed rebuild keeps the previous index in service: a root
// that is temporarily gone should degrade live reload, not blank the reader.
//
// A pending watch rebuild registers every root once. The fresh watcher is
// registered against the current index before the rebuild, so a change
// during the rebuild still wakes Run, and after it only the roots that pass
// could not open are registered from the fresh index (a root replaced since
// the last index, whose new directory only the fresh index pins).
func (w *Watcher) reload() {
	current := w.store.Current()
	var reset map[string]bool
	if w.resetPending && w.replaceWatcher() {
		w.dirs = nil
		reset = w.registerRoots(current.Roots())
	}
	fresh, err := current.Rebuild()
	if err != nil {
		slog.Warn("docs: index rebuild failed; continuing to serve the previous index.", "error", err)
		return
	}

	w.store.Swap(fresh)
	// Re-register after the swap so directories created since the last pass are
	// watched, and so relevance is evaluated against the new root set.
	if reset == nil {
		w.register(fresh)
	} else {
		var rest []Root
		for _, root := range fresh.Roots() {
			if !reset[root.Path] {
				rest = append(rest, root)
			}
		}
		w.registerRoots(rest)
	}

	slog.Debug("docs: index rebuilt for live reload.", "docCount", len(fresh.List()))
	w.broker.Publish(reloadMessage)
}
