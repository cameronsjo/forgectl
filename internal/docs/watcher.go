package docs

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	fsw      *fsnotify.Watcher
	store    *Store
	broker   *Broker
	debounce time.Duration
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
	return w.fsw.Close()
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
	for _, root := range idx.Roots() {
		rt, err := openPinnedRoot(root)
		if err != nil {
			slog.Debug("docs: live-reload registration could not open a root.", "root", root.Label, "error", err)
			continue
		}
		if err := w.watchTree(rt, root.Path); err != nil {
			slog.Debug("docs: live-reload registration walk failed for a root.", "root", root.Label, "error", err)
		}
		_ = rt.Close()
	}
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

// testHookWatch, when set, runs around each watch addVerified adds: before
// the Add (added false) and after the watch passed its identity check (added
// true). Tests use it to swap a directory at exactly those points.
var testHookWatch func(path string, added bool)

// addVerified adds a watch on path and keeps it only if path still names
// want, the directory the held walk listed. fsnotify.Add resolves path
// itself and follows a symlink, so a directory swapped for a symlink to an
// outside directory between the walk and the Add would otherwise leave a
// watch outside the root, and changes there would drive rebuilds.
//
// The check follows the Add, so it tests what the path named when the watch
// was bound, give or take a swap and swap back between the two calls. That
// window stays open because fsnotify has no by-handle Add, but a watch can
// only ever cause a rebuild, never a read: the rebuild walks through the
// pinned roots, and every serve re-verifies the path.
func (w *Watcher) addVerified(path string, want fs.FileInfo) bool {
	if testHookWatch != nil {
		testHookWatch(path, false)
	}
	if err := w.fsw.Add(path); err != nil {
		slog.Debug("docs: could not watch directory for live reload.", "path", path, "error", err)
		return false
	}
	got, err := os.Stat(path)
	if err != nil || !os.SameFile(want, got) {
		w.dropWatch(path)
		slog.Debug("docs: dropped a live-reload watch whose directory changed during registration.", "path", path)
		return false
	}
	if testHookWatch != nil {
		testHookWatch(path, true)
	}
	return true
}

// readdVerified re-adds a watch on dir, an event's containing directory,
// and keeps it only if dir still resolves to itself. The root paths are
// canonical and every watched directory was reached by real names below
// one, so a watched path that no longer resolves to itself has had a
// component swapped for a symlink, and the re-Add may have bound a
// directory outside the root.
func (w *Watcher) readdVerified(dir string) {
	if err := w.fsw.Add(dir); err != nil {
		return // best-effort; a removed dir legitimately fails here
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != dir {
		w.dropWatch(dir)
	}
}

// dropWatch removes the watch Add bound for path. The inotify backend
// (Linux) keys that watch by path. The kqueue backend (macOS) keys a watch
// added through a symlink by the link's target instead, so the target is
// removed too when it lies outside every root. A target inside a root is a
// directory the walk watches in its own right, and removing it would cost
// that directory its live reload.
func (w *Watcher) dropWatch(path string) {
	_ = w.fsw.Remove(path) //nolint:errcheck // best-effort; kqueue may not know the watch by this name
	target, err := filepath.EvalSymlinks(path)
	if err != nil || target == path {
		return
	}
	for _, root := range w.store.Current().Roots() {
		if withinRoot(root.Path, target) {
			return
		}
	}
	_ = w.fsw.Remove(target) //nolint:errcheck // best-effort; inotify keyed the watch by path and it is gone already
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
			w.refreshWatch(ev)

			if !w.relevant(ev.Name) {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(w.debounce)
			} else {
				timer.Reset(w.debounce)
			}
			settledC = timer.C

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("docs: live-reload watcher reported an error.", "error", err)

		case <-settledC:
			settledC = nil
			w.reload()
		}
	}
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
func (w *Watcher) reload() {
	current := w.store.Current()
	fresh, err := current.Rebuild()
	if err != nil {
		slog.Warn("docs: index rebuild failed; continuing to serve the previous index.", "error", err)
		return
	}

	w.store.Swap(fresh)
	// Re-register after the swap so directories created since the last pass are
	// watched, and so relevance is evaluated against the new root set.
	w.register(fresh)

	slog.Debug("docs: index rebuilt for live reload.", "docCount", len(fresh.List()))
	w.broker.Publish(reloadMessage)
}
