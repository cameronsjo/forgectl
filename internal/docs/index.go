// Package docs is the ops layer for `forgectl docs` (#93): a pure-Go,
// server-side-rendered local markdown reader. It indexes a closed set of
// root directories, renders markdown to sanitized HTML, and serves both over
// loopback HTTP. It knows nothing of Cobra — that decoupling is the house
// pattern (see internal/tmux, internal/net).
//
// Current scope: render, index, and live reload (a filesystem Watcher rebuilds
// the Index and notifies browsers over SSE). Mermaid and pan/zoom SVG are still
// outstanding — forgectl#93 stages those separately.
package docs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Root is one canonicalized root directory the server is willing to index
// and serve files from. Canonicalization happens once, at construction
// (NewIndex → CanonicalizeRoot) — every later request resolves against this
// already-EvalSymlinks'd value (security.go).
type Root struct {
	// Label identifies this root in URLs and the sidenav. Derived from the
	// directory's (or, for OnlyFile roots, the file's) base name,
	// disambiguated with a numeric suffix if two roots share one.
	Label string
	// Path is the canonical, symlink-resolved, trailing-separator-free
	// absolute directory — the traversal boundary ResolveInRoot enforces.
	// For an OnlyFile root this is the file's PARENT directory, never the
	// file itself (ResolveInRoot needs a directory to walk Join/EvalSymlinks
	// against).
	Path string
	// OnlyFile, when non-empty, is the canonical absolute path of the SOLE
	// file this root may ever serve — set when the user named a single
	// markdown file on the command line rather than a directory. Path is
	// still that file's parent directory (so traversal-checking machinery
	// is shared with directory roots), but Resolve additionally rejects
	// any resolved path other than OnlyFile: naming one file must not
	// silently grant access to every other file in its directory.
	OnlyFile string
	// dirInfo is the Stat of the os.Root the index opened on Path when the
	// root was indexed (openRootDir). Index.Open refuses to read through Path
	// once it names a different directory (openPinnedRoot).
	dirInfo fs.FileInfo
	// Kind classifies this root's link syntax and anchor semantics —
	// RootDocs (ordinary relative markdown links) or RootVault (Obsidian
	// wikilinks). Detected by detectRootKind (vault.go) at index-build
	// time; Task 4's IndexOptions lets a caller override the detection.
	Kind RootKind
	// VaultPath is the directory containing the ".obsidian" folder that
	// classified this root as RootVault — set only when Kind == RootVault.
	// It may differ from Path when the configured root sits somewhere
	// inside the vault rather than at the vault's own top.
	VaultPath string
}

// Doc is one indexed markdown file.
type Doc struct {
	// RootLabel is the Root.Label this doc was found under.
	RootLabel string
	// RelPath is the doc's path relative to its root, always forward-slash
	// separated regardless of host OS — it's used as a URL path segment.
	RelPath string
	// AbsPath is the doc's canonical, symlink-resolved absolute path — the
	// same value ResolveInRoot would produce for this doc's RelPath. It is
	// the join key Index.Resolve uses to confirm a resolved request path was
	// actually indexed (see Index.pathIndex); never sent to the client.
	AbsPath string
	// Title is the doc's first level-1 ("# ") heading, or its filename
	// (without extension) if none is found.
	Title string
	// Aliases is the doc's frontmatter `aliases` value (a YAML list or a
	// bare scalar, folded to a list), scanned for every root. Only a
	// RootVault root consults it during resolution.
	Aliases []string
	// Headings is every heading scanDoc found in the doc, in document
	// order, each carrying goldmark's auto-generated anchor slug.
	Headings []Heading
	// BlockIDs is the sorted, de-duplicated set of Obsidian "^block-id"
	// markers found in the doc.
	BlockIDs []string
	// Links is every outbound link scanDoc found in the doc — wikilinks
	// and plain markdown links whose destination carries no URL scheme —
	// already split into the form ResolveLink (Task 3) consumes.
	Links []LinkRef
	// Status and StaleAfter are the raw OKF trust fields from YAML
	// frontmatter (see trust.go). They are stored unevaluated: whether a doc
	// is stale depends on the clock, so readers call evalTrust at read time.
	Status     string
	StaleAfter string
	// OrphanOK is true when the frontmatter says orphan_ok: true, the page's
	// own statement that nothing is expected to link to it. Check counts it
	// in Summary.IgnoredOrphans instead of reporting an orphan.
	OrphanOK bool
	// ModTime is the file's last-modified time, used to order "recents".
	ModTime time.Time
}

// Index holds a closed set of Roots and the Docs discovered under them at
// construction time. An Index is never mutated in place: a changed tree
// produces a whole new Index (Rebuild), which the live-reload Watcher installs
// by pointer swap (Store). That immutability is what lets handlers read one
// without synchronization.
type Index struct {
	// paths is the caller's original, pre-canonicalization argument list, kept
	// so Rebuild can reproduce this index from the same request the caller
	// actually made. Re-deriving it from roots would be subtly wrong: a Root's
	// Path is canonical and, for a single-file root, is the file's PARENT
	// directory — rebuilding from that would silently widen a "serve this one
	// file" root into "serve its whole directory".
	paths []string
	roots []Root
	docs  []Doc
	// pathIndex is the set of every indexed (root label, absolute path) pair,
	// built once in NewIndex. Resolve consults it so the SAME predicate that
	// decided what's in the sidenav (walkRoot's hidden/vendor-dir exclusions,
	// symlinked-file exclusion) also decides what's servable — a directory
	// excluded from the walk must not remain reachable by a direct URL guess.
	//
	// Membership is keyed by ROOT as well as path, not by path alone. Roots may
	// legitimately overlap: naming a normally-excluded directory (say a
	// vault's .trash) as its own root is explicit consent to index it, but that
	// consent belongs to that root's URL namespace only. A single global set of
	// absolute paths would let the child root's consent leak sideways into the
	// parent root's namespace, where the same file is still deliberately hidden
	// from the sidenav — reopening the excluded-directory leak through
	// configuration instead of through code.
	pathIndex map[docKey]bool
	// byRoot holds one rootIndex per Root.Label, built by buildRootIndexes
	// (links.go) right after pathIndex. ResolveLink (links.go) reads only
	// idx.byRoot[from.RootLabel] — resolution never crosses roots (Global
	// Constraint).
	byRoot map[string]*rootIndex
	// backlinks maps a doc's docKey to the indices (into docs) of every
	// OTHER doc whose scanned Links resolve to it — built once, right after
	// byRoot, by running ResolveLink over every outbound link. Because this
	// reuses ResolveLink itself rather than a second lookup path, the
	// forward and reverse answers can never disagree. Backlinks reads it.
	backlinks map[docKey][]int
	// opts is the IndexOptions this Index was built with. Rebuild reproduces
	// it (NewIndexWithOptions(idx.paths, idx.opts)) so a live-reload rebuild
	// carries forward the same root-kind overrides the caller configured —
	// without this, Rebuild would silently drop them on the next filesystem
	// change.
	opts IndexOptions
	// skipped records every path the walk could not index (an unreadable
	// subdirectory, an entry that vanished mid-walk). It is the only channel
	// that survives the default log_level "off": `docs check` reads it to
	// refuse a verdict on a partial tree, and the other verbs print a note.
	skipped []SkippedPath
}

// SkippedPath is one path the index walk skipped instead of failing the build.
type SkippedPath struct {
	// Root is the root label; Rel is the slash-separated path under it. The
	// root itself is never skipped: an unreadable root is fatal.
	Root   string `json:"root"`
	Rel    string `json:"path"`
	Reason string `json:"reason"`
}

// Skipped returns the paths the walk skipped, in walk order. The result is a
// copy.
func (idx *Index) Skipped() []SkippedPath {
	return append([]SkippedPath(nil), idx.skipped...)
}

// skipReason is the recorded reason for a skipped path: the bare cause
// ("permission denied"), not err.Error(), whose *fs.PathError form repeats
// the absolute path. The skip entry already names the path as root label
// plus relative path, and that is all `docs check` output may carry.
func skipReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		return pe.Err.Error()
	}
	return err.Error()
}

// walkFunc is the callback walkDir calls for each entry. path is the
// entry's absolute path, root.Path joined with its root-relative name. dir
// is the held os.Root of the directory the entry was listed in, nil for the
// root itself. A non-nil err is a failure to read that entry, as in
// fs.WalkDirFunc, and returning filepath.SkipDir from a directory's call
// skips it.
type walkFunc func(path string, dir *os.Root, d fs.DirEntry, err error) error

// walkDir is the directory walk walkRoot runs. It is a seam so tests can
// inject a walk error without relying on file modes, which root ignores.
var walkDir = walkHeld

// walkHeld walks the directory rt holds, whose path is rootPath, in lexical
// order, the order filepath.WalkDir visits. Every directory is opened as its
// own os.Root in the one above it and must be the directory its Lstat saw,
// as in resolveIn, and each entry is handed to fn with the Root it was
// listed in. So the walk never follows a symlink, and a directory swapped
// for one mid-walk is skipped rather than descended (forgectl#743). It holds
// at most maxHeldDirs directories below rt at once; a deeper directory is
// reported to fn as an error rather than descended, which keeps every
// indexed doc within what resolveIn will serve.
func walkHeld(rt *os.Root, rootPath string, fn walkFunc) error {
	info, err := rt.Stat(".")
	if err != nil {
		return fn(rootPath, nil, nil, err)
	}
	self := fs.FileInfoToDirEntry(info)
	if err := fn(rootPath, nil, self, nil); err != nil {
		if errors.Is(err, filepath.SkipDir) {
			return nil
		}
		return err
	}
	return walkHeldDir(rt, rootPath, nil, self, 0, fn)
}

// errTooDeep is the reason walkHeld reports for a directory it does not
// descend because it lies more than maxHeldDirs levels below the root.
var errTooDeep = fmt.Errorf("directory is nested more than %d levels below the root", maxHeldDirs)

// errDirChanged is the reason walkHeld reports for a directory that was not
// the directory its Lstat saw by the time the walk opened it.
var errDirChanged = errors.New("directory changed while it was being walked")

// walkHeldDir lists dir, which holds path and was listed as self in parent,
// and walks its entries. depth counts the directories held below the root.
func walkHeldDir(dir *os.Root, path string, parent *os.Root, self fs.DirEntry, depth int, fn walkFunc) error {
	entries, err := readHeldDir(dir)
	if err != nil {
		if err := fn(path, parent, self, err); err != nil && !errors.Is(err, filepath.SkipDir) {
			return err
		}
		return nil
	}
	for _, e := range entries {
		p := filepath.Join(path, e.name)
		if err := fn(p, dir, e, nil); err != nil {
			if !errors.Is(err, filepath.SkipDir) {
				return err
			}
			if !e.IsDir() {
				return nil // filepath.WalkDir's rule: SkipDir on a file skips the rest of its directory
			}
			continue
		}
		if !e.IsDir() {
			continue
		}
		sub, err := openHeldSubdir(dir, e.name, depth)
		if err != nil {
			if err := fn(p, dir, e, err); err != nil && !errors.Is(err, filepath.SkipDir) {
				return err
			}
			continue
		}
		err = walkHeldDir(sub, p, dir, e, depth+1, fn)
		_ = sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// heldOpenErr is the walk's reading of an openChildDirRoot failure: a child
// that is no longer a directory, or moved under the open, is errDirChanged;
// anything else (a permission denial, say) is kept as the skip reason.
func heldOpenErr(err error) error {
	if errors.Is(err, errNotADirectory) || errors.Is(err, errDirRootMoved) {
		return errDirChanged
	}
	return err
}

// openHeldSubdir opens the directory name in dir as its own Root, refusing
// one past maxHeldDirs or one that is no longer the directory its Lstat
// sees. Unlike openDirVerified it keeps the open's own error (a permission
// denial, say), which the walk records as the skip reason. The open is
// openChildDirRoot, so a FIFO swapped in after the Lstat is refused as
// errDirChanged rather than waited on.
func openHeldSubdir(dir *os.Root, name string, depth int) (*os.Root, error) {
	if depth >= maxHeldDirs {
		return nil, errTooDeep
	}
	want, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !want.IsDir() {
		return nil, errDirChanged
	}
	sub, err := openChildDirRoot(dir, name)
	if err != nil {
		return nil, heldOpenErr(err)
	}
	got, err := sub.Stat(".")
	if err != nil || !got.IsDir() || !os.SameFile(want, got) {
		_ = sub.Close()
		return nil, errDirChanged
	}
	return sub, nil
}

// readHeldDir lists the directory dir holds, sorted by name. Each entry's
// type is the directory entry's own, and its Info is an Lstat through dir,
// never a lookup by path. On a filesystem that reports DT_UNKNOWN, Go fills
// Type() from an lstat by path instead. That type only steers the walk. A
// directory is descended only after openHeldSubdir re-checks it through
// dir, and a file is read only after its Info and openRegularIn re-check it,
// so a wrong type can skip an entry but never read through a symlink.
func readHeldDir(dir *os.Root) ([]heldEntry, error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	list, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]heldEntry, 0, len(list))
	for _, e := range list {
		out = append(out, heldEntry{dir: dir, name: e.Name(), typ: e.Type()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// heldEntry is one fs.DirEntry of a directory walkHeld holds open.
type heldEntry struct {
	dir  *os.Root
	name string
	typ  fs.FileMode
}

func (e heldEntry) Name() string               { return e.name }
func (e heldEntry) IsDir() bool                { return e.typ.IsDir() }
func (e heldEntry) Type() fs.FileMode          { return e.typ }
func (e heldEntry) Info() (fs.FileInfo, error) { return e.dir.Lstat(e.name) }

// faultEntry is the synthetic fs.DirEntry InjectWalkFaultForTest reports.
type faultEntry struct {
	dir  bool
	path string
}

func (e faultEntry) Name() string { return "fault" }
func (e faultEntry) IsDir() bool  { return e.dir }
func (e faultEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e faultEntry) Info() (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "lstat", Path: e.path, Err: fs.ErrNotExist}
}

// InjectWalkFaultForTest makes every index walk additionally report a
// permission error for the subdirectory "locked" and a vanished entry
// "gone.md" under each walked root, on top of the real walk. It exists so
// tests in other packages can exercise the skip path as root, where chmod
// cannot produce an unreadable directory. The returned func restores the
// real walk. Not safe for parallel tests.
func InjectWalkFaultForTest() (restore func()) {
	prev := walkDir
	walkDir = func(rt *os.Root, root string, fn walkFunc) error {
		if err := prev(rt, root, fn); err != nil {
			return err
		}
		// The errors carry the absolute path, as the real walk's do, so a test
		// can prove the recorded reason drops it.
		locked := filepath.Join(root, "locked")
		_ = fn(locked, rt, faultEntry{dir: true, path: locked}, &fs.PathError{Op: "open", Path: locked, Err: fs.ErrPermission})
		gone := filepath.Join(root, "gone.md")
		_ = fn(gone, rt, faultEntry{path: gone}, nil)
		return nil
	}
	return func() { walkDir = prev }
}

// IndexOptions carries construction-time overrides for NewIndexWithOptions.
type IndexOptions struct {
	// RootKinds overrides detectRootKind's filesystem-based inference for a
	// root, keyed by the root path as the caller wrote it. Keys and the
	// paths argument are compared by absolute, cleaned form (rootKindFor),
	// so "." / "./docs" / "docs/" in a config file match the absolute root
	// the CLI derives for the same directory. Symlinks are not resolved on
	// either side. A key that matches no entry in paths matches nothing; it
	// is not an error, since a caller's config may list roots this
	// particular invocation was not given.
	RootKinds map[string]RootKind
}

// rootKindFor returns the override for root path p, matching RootKinds
// keys by absolute cleaned path rather than by the literal string. A key
// or path that cannot be made absolute falls back to literal comparison.
// Two keys that name the same root with different kinds are an error, not
// a coin toss: map order would otherwise pick the winner per process.
func (o IndexOptions) rootKindFor(p string) (RootKind, bool, error) {
	if len(o.RootKinds) == 0 {
		return RootDocs, false, nil
	}
	want := comparablePath(p)
	keys := make([]string, 0, len(o.RootKinds))
	for key := range o.RootKinds {
		if comparablePath(key) == want {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return RootDocs, false, nil
	}
	sort.Strings(keys)
	kind := o.RootKinds[keys[0]]
	for _, key := range keys[1:] {
		if o.RootKinds[key] != kind {
			return RootDocs, false, fmt.Errorf("root_kinds: %q and %q name the same root with different kinds", keys[0], key)
		}
	}
	return kind, true, nil
}

// comparablePath is the form two root paths are compared in: absolute and
// cleaned, so relative spellings, "." segments, and trailing separators do
// not defeat a match. An unresolvable path compares as written.
func comparablePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// docKey identifies one indexed document by the root it was indexed under plus
// its canonical absolute path. Both halves are required: see Index.pathIndex.
type docKey struct {
	rootLabel string
	absPath   string
}

// NewIndex builds an Index over paths, each of which is either a directory
// (canonicalized and walked for markdown files, AllowedExt) or a single
// markdown file (canonicalized and indexed alone, without granting access to
// its sibling files — see Root.OnlyFile). A path that fails to canonicalize
// (doesn't exist, permission denied) or names a file with a disallowed
// extension is a hard error — a docs server should never silently start
// with fewer roots than the caller asked for.
func NewIndex(paths []string) (*Index, error) {
	return NewIndexWithOptions(paths, IndexOptions{})
}

// NewIndexWithOptions builds an Index over paths exactly as NewIndex does,
// except a root named in opts.RootKinds skips detectRootKind's filesystem
// probe and uses the given RootKind instead (see resolveRootKind for the
// VaultPath interaction). NewIndex(paths) is a thin call to
// NewIndexWithOptions(paths, IndexOptions{}), which is itself a thin call to
// NewIndexContext(context.Background(), paths, opts) — neither existing
// entry point changes behavior; context.Background() never cancels, so
// ctx.Err() never fires on that path.
func NewIndexWithOptions(paths []string, opts IndexOptions) (*Index, error) {
	return NewIndexContext(context.Background(), paths, opts)
}

// NewIndexContext builds an Index exactly as NewIndexWithOptions does, except
// each directory walk (walkRoot) checks ctx.Err() on every directory entry it
// visits and stops early when the caller's deadline or cancellation fires —
// see docs list's --timeout (forgectl#483). ctx.Err() firing while walking
// one root fails the WHOLE build: NewIndexContext returns early with a
// *WalkDeadlineError naming that root and wrapping ctx.Err(), discarding any
// roots already walked along with it — there is no partial Index to return.
func NewIndexContext(ctx context.Context, paths []string, opts IndexOptions) (*Index, error) {
	idx := &Index{paths: append([]string(nil), paths...), opts: opts}
	labels := map[string]bool{}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("docs root %q: %w", p, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("docs root %q: %w", p, err)
		}

		override, hasOverride, err := opts.rootKindFor(p)
		if err != nil {
			return nil, fmt.Errorf("docs root %q: %w", p, err)
		}

		if info.IsDir() {
			root, docs, skipped, err := indexDirRoot(ctx, labels, p, override, hasOverride)
			if err != nil {
				return nil, err
			}
			idx.roots = append(idx.roots, root)
			idx.docs = append(idx.docs, docs...)
			idx.skipped = append(idx.skipped, skipped...)
			continue
		}

		root, doc, err := indexFileRoot(labels, p, override, hasOverride)
		if err != nil {
			return nil, err
		}
		idx.roots = append(idx.roots, root)
		idx.docs = append(idx.docs, doc)
	}

	sortByRecency(idx.docs)

	idx.pathIndex = make(map[docKey]bool, len(idx.docs))
	for _, d := range idx.docs {
		idx.pathIndex[docKey{rootLabel: d.RootLabel, absPath: d.AbsPath}] = true
	}

	idx.byRoot = buildRootIndexes(idx.roots, idx.docs)
	idx.backlinks = idx.buildBacklinks()
	return idx, nil
}

// sortByRecency orders docs most-recently-modified first, breaking mtime
// ties on (RootLabel, RelPath). The tie-break makes the order total: root
// labels are unique per Index and a RelPath is unique within its root, so
// no two docs compare equal and the result does not depend on the order
// the docs arrived in. Without it, equal mtimes (a fresh git checkout, a tar
// extraction) left sort.Slice's pdqsort free to scramble them, so the
// "recent" lists showed them in an order no rule described.
func sortByRecency(docs []Doc) {
	sort.Slice(docs, func(i, j int) bool {
		a, b := &docs[i], &docs[j]
		if !a.ModTime.Equal(b.ModTime) {
			return a.ModTime.After(b.ModTime)
		}
		if a.RootLabel != b.RootLabel {
			return a.RootLabel < b.RootLabel
		}
		return a.RelPath < b.RelPath
	})
}

// resolveRootKind decides one root's Kind: an override in opts.RootKinds
// (NewIndexWithOptions) always wins over detectRootKind's filesystem probe,
// but detectRootKind still runs unconditionally so a real ".obsidian"
// ancestor's VaultPath is available to report even under an override.
//
//   - No override: detectRootKind's own answer, unchanged.
//   - Override to RootDocs: RootDocs with VaultPath cleared — a docs-kind
//     root has no vault to report, even if one happens to sit above it.
//   - Override to RootVault: RootVault. If detectRootKind found a real
//     ".obsidian" ancestor, its VaultPath is kept; otherwise VaultPath falls
//     back to canonical itself — the override grants Obsidian-style
//     wikilink/anchor semantics without requiring an actual ".obsidian"
//     directory on disk.
func resolveRootKind(canonical string, override RootKind, hasOverride bool) (RootKind, string) {
	kind, vaultPath := detectRootKind(canonical)
	if !hasOverride {
		return kind, vaultPath
	}
	if override == RootDocs {
		return RootDocs, ""
	}
	if kind == RootVault {
		return RootVault, vaultPath
	}
	return RootVault, canonical
}

// indexDirRoot canonicalizes dir and walks it for markdown files.
//
// A *WalkDeadlineError from walkRoot is returned as-is, not re-wrapped with
// "docs root %q": walkRoot already names the root that stopped
// (NewIndexContext), and a second "docs root %q" layer would name it twice
// without adding information. The check is on the ERROR's own type
// (errors.As), not on ctx.Err() at the time indexDirRoot happens to look —
// checking the ambient context instead of the concrete error would
// misclassify a real filesystem fault (e.g. permission denied) as a deadline
// error whenever the two race, discarding its "docs root %q" wrap for no
// reason. Every other walkRoot error (a real filesystem fault) keeps the
// existing wrap.
func indexDirRoot(ctx context.Context, labels map[string]bool, dir string, override RootKind, hasOverride bool) (Root, []Doc, []SkippedPath, error) {
	canonical, err := CanonicalizeRoot(dir)
	if err != nil {
		return Root{}, nil, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	label := uniqueLabel(labels, filepath.Base(canonical))
	kind, vaultPath := resolveRootKind(canonical, override, hasOverride)
	rt, dirInfo, err := openRootDir(canonical)
	if err != nil {
		return Root{}, nil, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	defer func() { _ = rt.Close() }()
	root := Root{Label: label, Path: canonical, Kind: kind, VaultPath: vaultPath, dirInfo: dirInfo}
	docs, skipped, err := walkRoot(ctx, root, rt)
	if err != nil {
		var deadline *WalkDeadlineError
		if errors.As(err, &deadline) {
			return Root{}, nil, nil, err
		}
		return Root{}, nil, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	return root, docs, skipped, nil
}

// indexFileRoot canonicalizes a single markdown file and indexes it alone.
// The returned Root's Path is the file's canonical PARENT directory (needed
// so ResolveInRoot has a directory to Join/EvalSymlinks against), but
// Root.OnlyFile pins the one path Resolve will ever hand back for it.
func indexFileRoot(labels map[string]bool, file string, override RootKind, hasOverride bool) (Root, Doc, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	real = filepath.Clean(real)
	if !AllowedExt(real) {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: not a markdown file", file)
	}

	parent, err := CanonicalizeRoot(filepath.Dir(real))
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}

	base := filepath.Base(real)
	label := uniqueLabel(labels, strings.TrimSuffix(base, filepath.Ext(base)))
	kind, vaultPath := resolveRootKind(parent, override, hasOverride)
	rt, dirInfo, err := openRootDir(parent)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	defer func() { _ = rt.Close() }()
	root := Root{Label: label, Path: parent, OnlyFile: real, Kind: kind, VaultPath: vaultPath, dirInfo: dirInfo}

	// The file is opened by its name in the pinned parent, as Index.Open
	// will serve it, and must be a regular file: a FIFO would block the
	// read forever (forgectl#743), and a symlink swapped in after
	// EvalSymlinks is not the file that was named.
	fi, err := rt.Lstat(base)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	if !fi.Mode().IsRegular() {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: not a regular file", file)
	}
	f, err := openRegularIn(rt, base, fi)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	defer func() { _ = f.Close() }()
	// Unlike walkRoot's per-file skip below, a scan failure on a single-file
	// root IS a hard error: there is no "rest of the index" to fall back to
	// serving without it.
	meta, err := scanDocFrom(kind, f, base)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	return root, newDoc(label, base, real, fi.ModTime(), meta), nil
}

// errRootMoved reports that a root's canonical path no longer named the
// directory CanonicalizeRoot resolved by the time the index opened it.
var errRootMoved = errors.New("root directory changed while it was being opened")

// openRootDir opens the canonical root directory as an os.Root and returns
// it with its Stat, which becomes Root.dirInfo, the pin openPinnedRoot
// checks every later open against. openDirRoot refuses a FIFO at the path
// rather than blocking on it (forgectl#798). The Stat is the open Root's own
// rather than a second lookup by path: on Windows os.Stat leaves the file ID to be
// filled by path at the first os.SameFile, which would pin whatever the path
// named at the first request instead of at index time (forgectl#743). The
// path must still name a directory, not a symlink swapped in after
// CanonicalizeRoot, since os.OpenRoot follows one.
func openRootDir(canonical string) (*os.Root, fs.FileInfo, error) {
	rt, err := openDirRoot(canonical)
	if err != nil {
		return nil, nil, err
	}
	info, err := rt.Stat(".")
	if err != nil {
		_ = rt.Close()
		return nil, nil, err
	}
	li, err := os.Lstat(canonical)
	if err != nil || li.Mode()&fs.ModeSymlink != 0 || !os.SameFile(li, info) {
		_ = rt.Close()
		return nil, nil, errRootMoved
	}
	return rt, info, nil
}

// newDoc is the one place a Doc is assembled from its scan result, so the
// directory walk and the single-file root can never populate a different
// subset of docMeta's fields.
func newDoc(rootLabel, relPath, absPath string, modTime time.Time, meta docMeta) Doc {
	return Doc{
		RootLabel:  rootLabel,
		RelPath:    relPath,
		AbsPath:    absPath,
		Title:      meta.Title,
		Aliases:    meta.Aliases,
		Headings:   meta.Headings,
		BlockIDs:   meta.BlockIDs,
		Links:      meta.Links,
		Status:     meta.Status,
		StaleAfter: meta.StaleAfter,
		OrphanOK:   meta.OrphanOK,
		ModTime:    modTime,
	}
}

// uniqueLabel returns base, or base suffixed with an incrementing counter if
// base is already taken — two configured roots sharing a base name (e.g. two
// different "docs" directories) must not collide in the URL/sidenav
// namespace.
func uniqueLabel(taken map[string]bool, base string) string {
	label := base
	for n := 2; taken[label]; n++ {
		label = fmt.Sprintf("%s-%d", base, n)
	}
	taken[label] = true
	return label
}

// hiddenOrVendorDir names directories the indexer never descends into.
var hiddenOrVendorDir = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

// excludedDir reports whether a directory with this base name must never be
// descended into. It is deliberately a single named predicate rather than an
// inline condition, because it has TWO callers that must agree byte-for-byte:
// walkRoot (which decides what lands in the index, and therefore — since
// Resolve gates on pathIndex membership — what is servable at all) and the
// live-reload Watcher (which decides which subtrees it registers and which
// events it acts on). This repo already shipped one security bug from exactly
// this kind of drift: the exclusions were UI-only in walkRoot while Resolve
// re-derived servability from the filesystem, so an excluded file was hidden
// from the sidenav yet served on a direct URL guess. A watcher carrying its
// own copy of the rule would reintroduce the same class of gap — a write
// under .trash/ waking the reader up, or a genuine doc silently not
// triggering reload.
//
// Callers apply this to a directory's BASE name, and must exempt a root's own
// path: a user may legitimately point a root at a dot-directory (or at a
// directory literally named "vendor"), and naming it explicitly is consent to
// index it. Only directories discovered BENEATH a root are subject to it.
func excludedDir(name string) bool {
	return hiddenOrVendorDir[name] || strings.HasPrefix(name, ".")
}

// WalkDeadlineError is returned when a walk stops because ctx.Err() fired
// (NewIndexContext, forgectl#483) — Root is the specific root that was being
// walked at that moment, which the caller needs and cannot otherwise recover:
// NewIndexContext may be building an Index over several paths, and the one
// that stalled is not necessarily the first. A caller reporting the deadline
// (docs list's --json error object) should errors.As for this type rather
// than assuming its own first root argument, which does not track which of
// several roots was actually in progress.
type WalkDeadlineError struct {
	Root string
	Err  error
}

func (e *WalkDeadlineError) Error() string {
	return fmt.Sprintf("docs index: walk of %s stopped: %s", e.Root, e.Err)
}

func (e *WalkDeadlineError) Unwrap() error { return e.Err }

// walkRoot discovers markdown files under root, walking and reading through
// rt, the os.Root openRootDir pinned for it (walkHeld). The walk never
// follows a symlink, for directories or files, and opens each doc by its
// single name in its held directory, verified to be the regular file the
// walk's Lstat saw (openRegularIn), so indexing can never read outside the
// root or read a file other than the one it lists (forgectl#743). A FIFO,
// socket or device named like a doc is never indexed, and the open is
// nonblocking besides, so none can hang the build. (Defense in depth only:
// the request-time resolution in security.go re-verifies every serve
// regardless of what the index contains.)
func walkRoot(ctx context.Context, root Root, rt *os.Root) ([]Doc, []SkippedPath, error) {
	var docs []Doc
	var skipped []SkippedPath
	skip := func(path, reason string) {
		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			rel = path
		}
		skipped = append(skipped, SkippedPath{Root: root.Label, Rel: filepath.ToSlash(rel), Reason: reason})
	}
	err := walkDir(rt, root.Path, func(path string, dir *os.Root, d fs.DirEntry, err error) error {
		if err != nil {
			// The root's own error stays fatal. Anything below it (an
			// unreadable subdirectory, or an entry that vanished mid-walk)
			// is skipped with a warning so one bad path cannot fail the
			// whole build. A skipped subtree is not indexed.
			if path == root.Path {
				return err
			}
			slog.Warn("docs: skipped an unreadable path during the index walk.",
				"root", root.Label, "path", path, "error", err)
			skip(path, skipReason(err))
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &WalkDeadlineError{Root: root.Path, Err: ctxErr}
		}
		if d.IsDir() {
			if path != root.Path && excludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Only a regular file is a doc: never a symlink (see the doc
		// comment above), and never a FIFO, socket or device, whose open or
		// read could block the build.
		if !d.Type().IsRegular() || !AllowedExt(path) {
			return nil
		}

		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			// The file vanished between the readdir and the stat.
			slog.Warn("docs: skipped a file that vanished during the index walk.",
				"root", root.Label, "path", path, "error", err)
			skip(path, skipReason(err))
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // replaced by a non-regular file since the readdir
		}

		// An open or scan failure here (the file became unreadable, or was
		// swapped, between the Lstat and this read) keeps the doc in the
		// index with its filename as the title and no link metadata — the
		// posture the title-only scan always had. Dropping it would make a
		// transient read error unlist a file that Index.Open's
		// request-time read may well succeed on a moment later.
		meta := docMeta{Title: titleFromFilename(relSlash)}
		if f, err := openRegularIn(dir, d.Name(), info); err == nil {
			if scanned, err := scanDocFrom(root.Kind, f, relSlash); err == nil {
				meta = scanned
			}
			_ = f.Close()
		}

		// path is canonical as it stands: the root is, and every directory
		// below it was entered by a real, verified name, never a symlink.
		// So Doc.AbsPath is byte-identical to what resolveIn computes for
		// the same file at request time, which Resolve's pathIndex
		// membership check depends on.
		docs = append(docs, newDoc(root.Label, relSlash, path, info.ModTime(), meta))
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return docs, skipped, nil
}

// Rebuild re-walks this index's original root arguments and returns a fresh
// Index, leaving the receiver untouched. It is how live reload picks up files
// that were created, deleted, renamed, or retitled since the last build.
//
// Rebuilding through NewIndex — rather than patching the existing index in
// place — is deliberate: NewIndex is the only code path that populates
// pathIndex, and pathIndex membership is what Resolve uses to decide whether a
// path is servable at all. An incremental "just add the changed file" update
// would be a second, parallel implementation of the exclusion rules, which is
// exactly the drift that produced the excluded-directory leak this reader
// already had to fix once. One builder, one gate.
//
// A rebuild can legitimately fail (a root was renamed or unmounted while the
// server was running). Callers SHOULD keep serving the previous index in that
// case rather than tearing the server down.
func (idx *Index) Rebuild() (*Index, error) {
	return NewIndexWithOptions(idx.paths, idx.opts)
}

// buildBacklinks maps every doc's docKey to the indices of every OTHER doc
// whose scanned Links resolve to it. Called from NewIndexWithOptions right
// after byRoot is built, since ResolveLink needs it — reusing ResolveLink
// itself (rather than a second lookup path) is what guarantees the forward
// and reverse answers can never disagree.
//
// A link counts once its FILE resolves, even if only its heading/block
// fragment missed (MissNoTarget paired with a non-nil Doc): the target doc
// was found, so it is still something the reader arrived at from this link.
// A link that resolves back to the SAME doc (a self-link, or a fragment-only
// link) is skipped — Backlinks answers "who else links here," and a doc is
// never its own backlink.
//
// Each list is de-duplicated (a doc linking to the target more than once
// appears once) and sorted by RelPath here, at build time, so Backlinks is
// a lookup and a copy — never repeated sorting on a request path.
func (idx *Index) buildBacklinks() map[docKey][]int {
	sets := make(map[docKey]map[int]bool)
	for i := range idx.docs {
		from := &idx.docs[i]
		for _, link := range from.Links {
			// Doc-only: the anchor is discarded and a fragment miss still
			// returns the doc, so matching the fragment (a rendered-text
			// parse for a vault) would be cost with no effect (#645). The
			// file-level miss outcomes do not depend on the fragment.
			target, _ := idx.resolveParts(from, link.Path, "", nil)
			if target == nil {
				continue
			}
			if target.RootLabel == from.RootLabel && target.AbsPath == from.AbsPath {
				continue
			}
			key := docKey{rootLabel: target.RootLabel, absPath: target.AbsPath}
			if sets[key] == nil {
				sets[key] = map[int]bool{}
			}
			sets[key][i] = true
		}
	}
	out := make(map[docKey][]int, len(sets))
	for key, set := range sets {
		indices := make([]int, 0, len(set))
		for i := range set {
			indices = append(indices, i)
		}
		sort.Slice(indices, func(a, b int) bool {
			return idx.docs[indices[a]].RelPath < idx.docs[indices[b]].RelPath
		})
		out[key] = indices
	}
	return out
}

// Backlinks returns every indexed doc whose scanned Links resolve to d,
// sorted by RelPath and de-duplicated (a doc linking to d more than once
// appears once). d is looked up by its own (RootLabel, AbsPath) — a nil d,
// or a Doc this Index never indexed, both return an empty slice rather than
// panicking. The returned pointers alias this Index's own storage and are
// read-only: an Index is never mutated in place (see the type comment), and
// writing through one would race every handler reading the same Index.
func (idx *Index) Backlinks(d *Doc) []*Doc {
	if d == nil {
		return nil
	}
	indices := idx.backlinks[docKey{rootLabel: d.RootLabel, absPath: d.AbsPath}]
	if len(indices) == 0 {
		return nil
	}
	out := make([]*Doc, 0, len(indices))
	for _, i := range indices {
		out = append(out, &idx.docs[i])
	}
	return out
}

// Roots returns the indexed roots in configuration order.
func (idx *Index) Roots() []Root {
	out := make([]Root, len(idx.roots))
	copy(out, idx.roots)
	return out
}

// List returns every indexed doc, most-recently-modified first.
func (idx *Index) List() []Doc {
	out := make([]Doc, len(idx.docs))
	for i := range idx.docs {
		out[i] = idx.docs[i].clone()
	}
	return out
}

// clone returns a Doc whose slice fields are independent copies, so a
// caller holding a value from List or Find cannot write through a shared
// backing array into this Index's own storage — which every handler reads
// unsynchronized on the promise that an Index never changes.
func (d Doc) clone() Doc {
	out := d
	out.Aliases = append([]string(nil), d.Aliases...)
	out.Headings = append([]Heading(nil), d.Headings...)
	out.BlockIDs = append([]string(nil), d.BlockIDs...)
	out.Links = append([]LinkRef(nil), d.Links...)
	return out
}

// Find returns the indexed Doc for (rootLabel, relPath), if any. Used to
// look up display metadata (Title) for a doc the caller already resolved
// through Resolve — Find itself performs no traversal check, so callers
// MUST NOT use it as a substitute for Resolve when deciding whether to serve
// a file.
func (idx *Index) Find(rootLabel, relPath string) (Doc, bool) {
	for _, d := range idx.docs {
		if d.RootLabel == rootLabel && d.RelPath == relPath {
			return d.clone(), true
		}
	}
	return Doc{}, false
}

// FindByAbsPath returns the indexed Doc whose canonical absolute path is
// absPath. It is how `docs open` turns a path the operator typed into the
// (root, relPath) pair a URL needs, WITHOUT reimplementing root matching or the
// exclusion rules on the client side — the index that decided what is servable
// is the thing being asked.
//
// absPath must already be canonical (symlink-resolved); callers get that from
// CanonicalizeRoot or filepath.EvalSymlinks.
func (idx *Index) FindByAbsPath(absPath string) (Doc, bool) {
	for _, d := range idx.docs {
		if d.AbsPath == absPath {
			return d, true
		}
	}
	return Doc{}, false
}

// ErrRootNotFound indicates a request named a root label the index doesn't
// have.
var ErrRootNotFound = errors.New("no such docs root")

// ErrNotIndexed indicates a resolved path is a real, in-root, allowed-
// extension file that nonetheless was never added to the index — e.g. it
// lives under a directory walkRoot excludes (.git, node_modules, vendor, any
// dot-directory). Without this check, Resolve re-derives a path straight
// from the filesystem and would happily serve a file the sidenav deliberately
// hides; membership in the index is what makes "excluded from the walk" and
// "not servable" the same guarantee instead of two claims that can drift
// apart.
var ErrNotIndexed = errors.New("file was not indexed")

// Resolve maps a (rootLabel, relPath) URL pair to a safe, on-disk absolute
// path: it looks up rootLabel among the indexed Roots, then runs relPath
// through the full ResolveInRoot traversal chain (security.go) against that
// root's canonical path, checks the resolved path's extension against
// AllowedExt, and finally requires the (root label, resolved path) PAIR to be a
// member of idx.pathIndex — the exact set walkRoot/indexFileRoot populated at
// index build time. That last check is what closes the gap between "hidden from
// the sidenav" and "not servable": Resolve never re-derives servability from
// the live filesystem independently of what was actually indexed. For a
// single-file root (Root.OnlyFile set), any resolution other than that exact
// file is also rejected — naming one file on the command line must not grant
// access to its siblings. Any failure returns a wrapped error; the HTTP
// layer maps all of them to 404 without distinguishing the cause to the
// client.
//
// Resolve is a check, not a read: the path it returns can be swapped before
// anything opens it. A caller that reads the doc uses Open instead.
func (idx *Index) Resolve(rootLabel, relPath string) (string, error) {
	rt, end, resolved, err := idx.resolveOpen(rootLabel, relPath)
	if err != nil {
		return "", err
	}
	end.close()
	_ = rt.Close()
	return resolved, nil
}

// Open is Resolve followed by opening the doc (forgectl#611). Resolution
// holds every directory on the path open as its own os.Root, each verified
// to be the directory its Lstat saw, and the doc is opened by its single
// name in the last of them. The file opened must then be a regular file
// and the one the walk's own Lstat saw (os.SameFile), or Open closes it and
// denies with ErrOutsideRoot. So a symlink or directory swapped in after
// the check can neither leave the root nor redirect the read to another
// file inside it. The root itself is pinned too: it must still be the
// directory the index was built from (Root.dirInfo), so replacing the root
// path with a symlink after indexing is refused. The caller owns the
// returned file, which stays valid after the Roots close. resolved is the
// path Resolve would have returned, for display and membership only;
// reading it again by path would reopen the race Open closes.
func (idx *Index) Open(rootLabel, relPath string) (f *os.File, resolved string, err error) {
	rt, end, resolved, err := idx.resolveOpen(rootLabel, relPath)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		end.close()
		_ = rt.Close()
	}()
	if end.base == "" {
		return nil, "", ErrOutsideRoot
	}
	f, err = openVerified(end.dir, end.base, end.info)
	if err != nil {
		return nil, "", err
	}
	return f, resolved, nil
}

// openVerified opens name in dir and returns it only if it is still the
// regular file want describes. A non-regular want is refused before any
// open, and the open is nonblocking (openNonblock), so neither a FIFO found
// by the walk nor one swapped in after it can hang the caller.
func openVerified(dir *os.Root, name string, want fs.FileInfo) (*os.File, error) {
	f, err := openRegularIn(dir, name, want)
	if err != nil {
		return nil, ErrOutsideRoot
	}
	return f, nil
}

// errFileChanged reports that the file opened was not the regular file the
// caller's Lstat described.
var errFileChanged = errors.New("file changed between its stat and its open")

// openRegularIn is openVerified keeping the underlying error, for the index
// walk, which records it as a skip reason or a root's error.
func openRegularIn(dir *os.Root, name string, want fs.FileInfo) (*os.File, error) {
	if dir == nil || !want.Mode().IsRegular() {
		return nil, errFileChanged
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, err
	}
	got, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !got.Mode().IsRegular() || !os.SameFile(want, got) {
		_ = f.Close()
		return nil, errFileChanged
	}
	return f, nil
}

// resolveOpen is the shared body of Resolve and Open. On success it returns
// the open top-level Root, which the caller closes after end.close(), where
// the walk ended, and the canonical absolute path.
func (idx *Index) resolveOpen(rootLabel, relPath string) (*os.Root, *walkEnd, string, error) {
	for _, r := range idx.roots {
		if r.Label != rootLabel {
			continue
		}
		rt, err := openPinnedRoot(r)
		if err != nil {
			return nil, nil, "", err
		}
		// r.dirInfo stands in for the root's own Stat: openPinnedRoot just
		// proved rt is that directory, so resolveIn need not stat it again.
		end, resolved, err := idx.checkInRoot(rt, r, relPath)
		if err != nil {
			_ = rt.Close()
			return nil, nil, "", err
		}
		return rt, end, resolved, nil
	}
	return nil, nil, "", ErrRootNotFound
}

// openPinnedRoot opens r's directory and returns it only if it is still the
// directory the index was built from. os.OpenRoot follows a symlink at the
// root path itself, so without this a root moved aside and replaced by a
// symlink after indexing would serve whatever the symlink names. The open
// is openDirRoot's, so a FIFO swapped in at the path fails at once instead
// of blocking the request (forgectl#798).
func openPinnedRoot(r Root) (*os.Root, error) {
	if r.dirInfo == nil {
		return nil, ErrOutsideRoot
	}
	rt, err := openDirRoot(r.Path)
	if err != nil {
		return nil, ErrOutsideRoot
	}
	got, err := rt.Stat(".")
	if err != nil || !os.SameFile(r.dirInfo, got) {
		_ = rt.Close()
		return nil, ErrOutsideRoot
	}
	return rt, nil
}

// checkInRoot runs the resolution chain and the index's own gates for one
// root over its open Root. On success the caller owns end.
func (idx *Index) checkInRoot(rt *os.Root, r Root, relPath string) (*walkEnd, string, error) {
	end, err := resolveIn(rt, r.dirInfo, r.Path, relPath)
	if err != nil {
		return nil, "", err
	}
	resolved := filepath.Join(r.Path, end.name)
	deny := func(err error) (*walkEnd, string, error) {
		end.close()
		return nil, "", err
	}
	if r.OnlyFile != "" && resolved != r.OnlyFile {
		return deny(ErrOutsideRoot)
	}
	if !AllowedExt(resolved) {
		return deny(ErrDisallowedExt)
	}
	// Keyed on r.Label, so a file indexed under a DIFFERENT (possibly
	// overlapping) root does not satisfy membership for this one.
	if !idx.pathIndex[docKey{rootLabel: r.Label, absPath: resolved}] {
		return deny(ErrNotIndexed)
	}
	return end, resolved, nil
}
