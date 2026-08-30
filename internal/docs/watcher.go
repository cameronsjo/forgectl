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
		err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // unreadable subtree: skip it, keep the rest of the walk
			}
			if !d.IsDir() {
				return nil
			}
			// Identical exemption to walkRoot's: a root the user named
			// explicitly is watched even if its own base name looks excluded.
			if path != root.Path && excludedDir(d.Name()) {
				return filepath.SkipDir
			}
			if addErr := w.fsw.Add(path); addErr != nil {
				slog.Debug("docs: could not watch directory for live reload.", "path", path, "error", addErr)
			}
			return nil
		})
		if err != nil {
			slog.Debug("docs: live-reload registration walk failed for a root.", "root", root.Label, "error", err)
		}
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
		_ = w.fsw.Add(dir) //nolint:errcheck // best-effort; a removed dir legitimately fails here
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
	_ = filepath.WalkDir(ev.Name, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		if path != ev.Name && excludedDir(d.Name()) {
			return filepath.SkipDir
		}
		_ = w.fsw.Add(path) //nolint:errcheck // best-effort
		return nil
	})
}

// relevant reports whether an event path should trigger a reload.
//
// It reuses AllowedExt, AllowedMediaExt, and excludedDir rather than restating
// them, so the watcher cannot disagree with the server about what can affect a
// rendered doc. The
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
	if !AllowedExt(path) && !AllowedMediaExt(path) {
		return false
	}

	idx := w.store.Current()
	for _, root := range idx.Roots() {
		if !withinRoot(root.Path, path) {
			continue
		}
		// A single-file root may serve an image only after proving that its sole
		// doc references that image. The watcher does not parse every event's
		// source to repeat that proof, so it conservatively reloads only the doc
		// itself; a changed sibling image is visible after a manual refresh.
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
