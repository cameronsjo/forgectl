package docs

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// maxResetStreak caps resetStreak. settleDelay reaches maxResetBackoff
// within a few doublings of any debounce, so a longer streak changes
// nothing but the size of the counter.
const maxResetStreak = 16

// DefaultMaxWait bounds how long events arriving inside the debounce can
// keep postponing a reload: a burst reloads no later than this after its
// first pending event, or after settleDelay if a watch rebuild's backoff is
// longer. Without it, a writer touching a doc more often than once per
// debounce would hold every reload off for as long as it kept writing.
const DefaultMaxWait = 2 * time.Second

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
	maxWait  time.Duration

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

	// pendingSince is when the first event of the burst Run is waiting to
	// settle armed the timer, and zero while no reload is armed. settleIn
	// measures maxWait from it. Touched only by Run.
	pendingSince time.Time

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
	w := &Watcher{fsw: fsw, store: store, broker: broker, debounce: DefaultDebounce, maxWait: DefaultMaxWait}
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
// indexer would descend into; a single-file root gets one watch, on its own
// directory (watchOnlyFileDir). Failures are logged and skipped rather than
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
		if root.OnlyFile != "" {
			w.watchOnlyFileDir(rt, root.Path)
		} else if err := w.watchTree(rt, root.Path); err != nil {
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

// watchOnlyFileDir watches dir, the directory a single-file root's rt holds,
// and nothing below it (forgectl#923). The one file the root serves lives
// directly in dir, so its events arrive on that watch, and a walk of the
// subtree would put watches on directories the user never asked to serve:
// renaming one would rebuild the watches, and writes under it would wake
// the watcher. Run drops the events the watch delivers for the file's
// siblings (onlyFileSibling).
func (w *Watcher) watchOnlyFileDir(rt *os.Root, dir string) {
	want, err := rt.Stat(".")
	if err != nil || !want.IsDir() {
		return
	}
	w.addVerified(dir, want)
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
// not descend into stops the registration. A single-file root watches only
// its own directory (watchOnlyFileDir), so it registers nothing here.
func (w *Watcher) watchCreatedDir(path string) {
	for _, root := range w.store.Current().Roots() {
		if root.OnlyFile != "" || path == root.Path || !withinRoot(root.Path, path) {
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
// watcher closes. It coalesces bursts through w.debounce, bounded by
// w.maxWait (settleIn), and, for each settled burst, rebuilds the watches
// if a rebuild is pending, rebuilds the index, swaps it into the Store, and
// publishes one reload notification when the burst held a doc event or the
// index changed (reload).
func (w *Watcher) Run(ctx context.Context) {
	var (
		timer    *time.Timer
		settledC <-chan time.Time
		// sawDocEvent records that the burst being settled held an event
		// on an in-root doc name (relevant and not stray). Such a burst
		// publishes even when the rebuilt index is equal: the index cannot
		// see a body edit that kept the doc's metadata and its mtime tick.
		sawDocEvent bool
	)
	arm := func() {
		if timer == nil {
			timer = time.NewTimer(w.settleIn(time.Now()))
		} else {
			timer.Reset(w.settleIn(time.Now()))
		}
		settledC = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	// A registration in NewWatcher that met a changing directory has a
	// watch rebuild waiting. Run the reload that performs it.
	if w.resetPending {
		arm()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// A single-file root's directory watch also reports the file's
			// siblings, which the root does not serve. Nothing about them
			// may reach the watch set or the settle (forgectl#923).
			if w.onlyFileSibling(ev.Name) {
				continue
			}
			// Keep the watch set current before deciding relevance: a newly
			// created directory needs watching even when the event that
			// revealed it carries no markdown of its own, and the containing
			// directory may itself have just been replaced.
			wasPending := w.resetPending
			// Decided before refreshWatch, which may change the watch set
			// but never the filesystem the event came through.
			stray := w.strayEvent(ev.Name)
			w.refreshWatch(ev)
			// A Rename of a watched directory rebuilds the watches: the
			// moved tree's descendants keep theirs wherever it went,
			// outside the root or into an excluded directory, and only a
			// fresh watcher drops them (forgectl#796). Move detection
			// never consults stray, since a directory renamed away and
			// replaced by a symlink has a stray Rename (PR #868 review).
			moved := w.dirMoved(ev)
			// A stray event rebuilds the watches too: fsnotify holds a
			// watch bound somewhere its name does not lead, and on kqueue a
			// watch opened through a symlink keeps that binding even after
			// the name is a real file or directory again, since a re-Add
			// of a watched path reuses the watch (forgectl#865). Only a
			// fresh fsnotify watcher drops it.
			if moved || stray {
				w.resetPending = true
			}
			// stray gates only the doc-event decision. A stray event can
			// still publish, but only through the index its settle rebuilds
			// changing (a doc replaced by a symlink drops out of it), and
			// the index holds only root-confined state.
			docEvent := !stray && w.relevant(ev.Name)
			if docEvent {
				sawDocEvent = true
			}
			// An attachment added, removed or renamed arms a settle but is
			// not a doc event: it publishes only through the attachment set
			// the rebuilt index compares (sameIndex), so a name that did not
			// change what resolves stays silent (forgectl#904).
			attachmentEvent := !stray && w.attachmentRelevant(ev)
			// Any in-tree Create arms a settle too, and publishes only if
			// the rebuilt index changed (forgectl#895). On kqueue a new
			// entry reaches us only as the Create fsnotify's dirChange sends
			// while listing the directory, and that listing stops at the
			// first entry it cannot open (EACCES, EPERM, ENOENT), which it
			// never marks seen: every later change to the directory sends
			// that entry's Create again and nothing for the docs sorting
			// after it. A dangling link's repeat Create is stray and
			// rebuilds already; an unopenable plain file's lands here, and
			// the rebuild's walk finds the docs the listing missed. It can
			// postpone a pending rebuild no further than settleIn allows.
			createEvent := !stray && ev.Has(fsnotify.Create) && w.inTree(ev.Name)

			// A reset already pending with its reload armed is not re-armed
			// by an event that is not otherwise relevant, so churn on other
			// files cannot keep postponing it.
			resetNeedsArming := w.resetPending && (!wasPending || settledC == nil)
			if !docEvent && !attachmentEvent && !createEvent && !moved && !resetNeedsArming {
				continue
			}
			arm()

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("docs: live-reload watcher reported an error.", "error", err)

		case <-settledC:
			settledC = nil
			w.pendingSince = time.Time{}
			if w.resetPending {
				w.noteReset(time.Now())
			}
			w.reload(sawDocEvent)
			sawDocEvent = false
			// A rebuild that itself met a changing directory waits again
			// rather than looping, backed off by settleDelay.
			if w.resetPending {
				arm()
			}
		}
	}
}

// noteReset records a watch rebuild starting at now for settleDelay's
// backoff: one within resetQuiet of the last extends the streak, up to
// maxResetStreak, and any other starts it over.
func (w *Watcher) noteReset(now time.Time) {
	if !w.lastReset.IsZero() && now.Sub(w.lastReset) < resetQuiet {
		w.resetStreak = min(w.resetStreak+1, maxResetStreak)
	} else {
		w.resetStreak = 0
	}
	w.lastReset = now
}

// settleIn is how long from now Run arms its timer for, on an event or a
// rebuild waiting to run. It is settleDelay, cut short so the reload lands
// no later than maxWait after the burst's first pending event, or
// settleDelay after it when a rebuild's backoff is the longer: a watch
// rebuild's backoff is never shortened, and events inside it cannot extend
// it. The first call of a burst starts that clock; Run clears it on reload.
func (w *Watcher) settleIn(now time.Time) time.Duration {
	d := w.settleDelay()
	if w.pendingSince.IsZero() {
		w.pendingSince = now
	}
	maxWait := w.maxWait
	if maxWait <= 0 {
		maxWait = DefaultMaxWait // a struct-literal Watcher must not mean "never extend"
	}
	limit := max(maxWait, d) - now.Sub(w.pendingSince)
	return max(min(d, limit), 0)
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

// strayEvent reports whether an event reached the watcher through a watch
// that is now bound outside the watched tree (forgectl#865). fsnotify names
// an event by the path its watch was registered under, and on kqueue
// (macOS, BSD) it also watches every entry of a watched directory itself,
// listing and opening each by path, so every symlink on the way is
// followed: an intermediate one (a directory swapped for a symlink to an
// outside directory is listed through it, and each outside entry gets a
// watch named root/a/<entry> without any Add of ours) and a leaf one (an
// in-root leak.md linking to an outside file, or a directory named *.md
// swapped for such a link, is watched at its target). addVerified's checks
// cannot see those watches, and relevant() is lexical, so a write to the
// outside file would drive a reload.
//
// So the event's full name is resolved as the filesystem stands now
// (resolveNow), and when it resolves through any symlink the resolved path
// must pass the same predicate the name did: relevant() for a doc name,
// which keeps an OnlyFile root to its one file and refuses excluded
// directories, or inTree() for any other name (a directory's own Rename).
// An event whose resolved path fails is stray: it is never a doc event,
// so it publishes only if the index its settle rebuilds changed, and it
// schedules a rebuild of the watches (Run). A stray Rename of a watched
// directory still counts as a move. A compat symlink that leads to a doc
// elsewhere inside the root is not stray, and a name with no symlink on
// its path is decided exactly as before. The check fails closed: a path
// that cannot be resolved for any reason but its own absence (a dangling
// symlink, a loop) is stray. A deleted path resolves through its deepest
// existing ancestor, so its events still reload.
//
// It is a delivery-time check, so a path swapped back between the event and
// the check passes it. The stray watches themselves last until the next
// full rebuild (replaceWatcher) or Close; fsnotify has no by-handle Remove.
func (w *Watcher) strayEvent(name string) bool {
	resolved, err := resolveNow(name)
	if err != nil {
		return true
	}
	if resolved == name {
		return false
	}
	if AllowedExt(name) {
		return !w.relevant(resolved)
	}
	return !w.inTree(resolved)
}

// resolveNow resolves every symlink in path, the leaf included, as the
// filesystem stands now. A path that does not exist resolves through its
// deepest existing ancestor, with the missing tail joined back on. Any
// other failure is an error, including a component that exists but
// resolves nowhere (a dangling symlink).
func resolveNow(path string) (string, error) {
	tail := ""
	for p := path; ; {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if _, lerr := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) || !errors.Is(lerr, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
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
	return AllowedExt(path) && w.inTree(path)
}

// attachmentRelevant reports whether ev can change a vault root's attachment
// set (walkRoot): a Create, Remove or Rename of a non-markdown name that is
// not a dot-file, which some vault root accepts (rootAccepts). A write to an
// attachment's contents is not relevant, since resolution reads names only.
// Like relevant() it is lexical, because a removed or renamed-away path
// cannot be stat'ed; a Create of a directory or a symlink with such a name
// arms a settle too, and the rebuilt index, which lists neither, leaves it
// unpublished.
func (w *Watcher) attachmentRelevant(ev fsnotify.Event) bool {
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
		return false
	}
	if AllowedExt(ev.Name) || strings.HasPrefix(filepath.Base(ev.Name), ".") {
		return false
	}
	// The kind comes from a root that accepts the path, not merely the
	// first that holds it: roots can overlap (forgectl#917).
	for _, root := range w.store.Current().Roots() {
		if root.Kind == RootVault && rootAccepts(root, ev.Name) {
			return true
		}
	}
	return false
}

// onlyFileSibling reports whether name lies in some root but every root
// holding it is a single-file root that name is neither the file of nor
// the directory of: a sibling of the file, or something below one, which
// the root's directory watch (or, on kqueue, fsnotify's own watch of each
// entry of that directory) reports although no root serves it
// (forgectl#923). Lexical, like relevant(). A name no root holds is not a
// sibling, and goes through Run's usual checks.
func (w *Watcher) onlyFileSibling(name string) bool {
	held := false
	for _, root := range w.store.Current().Roots() {
		if !withinRoot(root.Path, name) {
			continue
		}
		if root.OnlyFile == "" || name == root.OnlyFile || name == root.Path {
			return false
		}
		held = true
	}
	return held
}

// inTree is relevant() without the extension rule: whether some root
// accepts path (rootAccepts). Every containing root is consulted, not just
// the first: with overlapping roots (docs serve ~/v/n.md ~/v) the first may
// be a single-file root that refuses what the enclosing root accepts
// (forgectl#917).
func (w *Watcher) inTree(path string) bool {
	for _, root := range w.store.Current().Roots() {
		if rootAccepts(root, path) {
			return true
		}
	}
	return false // no configured root accepts it
}

// rootAccepts reports whether path lies in root, is root's one file when it
// is an OnlyFile root, has no excluded directory component below it, and
// sits no deeper than the index walk descends (maxHeldDirs directories
// below the root, walkHeld).
//
// It is lexical, so a deleted or renamed-away path is still judged, and so
// it cannot see a symlink on the path: a name through an in-root link to a
// directory elsewhere passes. The callers own that rule. Run resolves every
// event's name first and refuses one that resolves somewhere rootAccepts
// would not (strayEvent), and the watch set is built by the held walk,
// which never descends a link (watchTree, addVerified).
func rootAccepts(root Root, path string) bool {
	if !withinRoot(root.Path, path) {
		return false
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
	if len(segments)-1 > maxHeldDirs {
		return false // the walk skips a directory this deep, so nothing in it is indexed
	}
	for _, dir := range segments[:len(segments)-1] {
		if excludedDir(dir) {
			return false
		}
	}
	return true
}

// reload rebuilds the index and, on success, installs it. It notifies
// subscribers when docEvent is set (the settled burst held an in-root doc
// event) or the fresh index differs from the one it replaces (sameIndex),
// and otherwise stays silent. A failed rebuild keeps the previous index in
// service: a root that is temporarily gone should degrade live reload, not
// blank the reader.
//
// Every settle takes this one path, a watch rebuild included, so a change
// the rebuild could not see as an event (a doc edited before its directory
// was watched again) still reaches the index and, by changing it,
// publishes. A settle only a stray event armed cannot publish on that
// event's account, but the rebuild it runs can still find a change. The
// docs, attachments and skipped paths come from a walk confined to the
// roots. Each root's kind and vault path do not: detectRootKind looks for a
// .obsidian directory in every ancestor of the root below $HOME, so a
// .obsidian created or removed above a root changes the index from outside
// it, and a settle that happens to run then publishes that change.
//
// A pending watch rebuild registers every root once. The fresh watcher is
// registered against the current index before the rebuild, so a change
// during the rebuild still wakes Run, and after it only the roots that pass
// could not open are registered from the fresh index (a root replaced since
// the last index, whose new directory only the fresh index pins).
func (w *Watcher) reload(docEvent bool) {
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

	if !docEvent && sameIndex(current, fresh) {
		slog.Debug("docs: index rebuilt for live reload; nothing changed.", "docCount", len(fresh.List()))
		return
	}
	slog.Debug("docs: index rebuilt for live reload.", "docCount", len(fresh.List()))
	w.broker.Publish(reloadMessage)
}

// sameIndex reports whether b holds what a does as far as a reader can
// tell: the same roots (by label, path, single file, kind and vault), the
// same docs in the same order with every scanned field equal, the same
// attachments per root (what a vault wikilink can resolve to,
// forgectl#904), and the same skipped paths. A root's pinned directory
// identity is not compared: a root replaced by an identical tree serves
// the same pages.
//
// Attachments compare by basename table, one entry per file, rather than by
// the case-folded path set: removing a/Dup.png beside a/dup.png leaves the
// folded set unchanged but turns an ambiguous [[dup.png]] into a hit
// (forgectl#917). buildRootIndexes sorts each entry's paths, so a change of
// walk order alone (a directory renamed Z to z) compares equal
// (forgectl#923).
func sameIndex(a, b *Index) bool {
	if len(a.roots) != len(b.roots) || len(a.docs) != len(b.docs) || !slices.Equal(a.skipped, b.skipped) {
		return false
	}
	for i := range a.roots {
		x, y := a.roots[i], b.roots[i]
		if x.Label != y.Label || x.Path != y.Path || x.OnlyFile != y.OnlyFile || x.Kind != y.Kind || x.VaultPath != y.VaultPath {
			return false
		}
		if !maps.EqualFunc(a.attachmentsByName(x.Label), b.attachmentsByName(y.Label), slices.Equal[[]string]) {
			return false
		}
	}
	for i := range a.docs {
		x, y := a.docs[i], b.docs[i]
		if !x.ModTime.Equal(y.ModTime) {
			return false
		}
		x.ModTime, y.ModTime = time.Time{}, time.Time{}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}
