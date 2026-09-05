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
	"errors"
	"fmt"
	"io/fs"
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
// NewIndexWithOptions(paths, IndexOptions{}).
func NewIndexWithOptions(paths []string, opts IndexOptions) (*Index, error) {
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
			root, docs, err := indexDirRoot(labels, p, override, hasOverride)
			if err != nil {
				return nil, err
			}
			idx.roots = append(idx.roots, root)
			idx.docs = append(idx.docs, docs...)
			continue
		}

		root, doc, err := indexFileRoot(labels, p, override, hasOverride)
		if err != nil {
			return nil, err
		}
		idx.roots = append(idx.roots, root)
		idx.docs = append(idx.docs, doc)
	}

	sort.Slice(idx.docs, func(i, j int) bool { return idx.docs[i].ModTime.After(idx.docs[j].ModTime) })

	idx.pathIndex = make(map[docKey]bool, len(idx.docs))
	for _, d := range idx.docs {
		idx.pathIndex[docKey{rootLabel: d.RootLabel, absPath: d.AbsPath}] = true
	}

	idx.byRoot = buildRootIndexes(idx.roots, idx.docs)
	idx.backlinks = idx.buildBacklinks()
	return idx, nil
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
func indexDirRoot(labels map[string]bool, dir string, override RootKind, hasOverride bool) (Root, []Doc, error) {
	canonical, err := CanonicalizeRoot(dir)
	if err != nil {
		return Root{}, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	label := uniqueLabel(labels, filepath.Base(canonical))
	kind, vaultPath := resolveRootKind(canonical, override, hasOverride)
	root := Root{Label: label, Path: canonical, Kind: kind, VaultPath: vaultPath}
	docs, err := walkRoot(root)
	if err != nil {
		return Root{}, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	return root, docs, nil
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
	root := Root{Label: label, Path: parent, OnlyFile: real, Kind: kind, VaultPath: vaultPath}

	fi, err := os.Stat(real)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	// Unlike walkRoot's per-file skip below, a scan failure on a single-file
	// root IS a hard error: there is no "rest of the index" to fall back to
	// serving without it.
	meta, err := scanDoc(real, base)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	return root, newDoc(label, base, real, fi.ModTime(), meta), nil
}

// newDoc is the one place a Doc is assembled from its scan result, so the
// directory walk and the single-file root can never populate a different
// subset of docMeta's fields.
func newDoc(rootLabel, relPath, absPath string, modTime time.Time, meta docMeta) Doc {
	return Doc{
		RootLabel: rootLabel,
		RelPath:   relPath,
		AbsPath:   absPath,
		Title:     meta.Title,
		Aliases:   meta.Aliases,
		Headings:  meta.Headings,
		BlockIDs:  meta.BlockIDs,
		Links:     meta.Links,
		ModTime:   modTime,
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

// walkRoot discovers markdown files under root. It does not follow symlinks
// for either directories or files during the walk — fs.WalkDir already
// doesn't descend into a symlinked directory, and a symlinked file is
// skipped outright here — so indexing can never itself be tricked into
// walking outside root. (Defense in depth only: the request-time
// ResolveInRoot chain in security.go re-verifies every serve regardless of
// what the index contains.)
func walkRoot(root Root) ([]Doc, error) {
	var docs []Doc
	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root.Path && excludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never index a symlinked file; see doc comment above
		}
		if !AllowedExt(path) {
			return nil
		}

		// Resolve through EvalSymlinks (rather than trusting that a
		// symlink-free walk under an already-canonical root produces an
		// already-canonical path) so Doc.AbsPath is byte-identical to
		// whatever ResolveInRoot computes for the same file at request
		// time — that identity is what lets Resolve's pathIndex membership
		// check work at all. A file that vanishes or becomes unreadable
		// between WalkDir's stat and this call is skipped, not a hard
		// index-build failure.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil
		}
		resolved = filepath.Clean(resolved)

		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		// A scan failure here (the file became unreadable between
		// WalkDir's stat and this read) keeps the doc in the index with
		// its filename as the title and no link metadata — the posture
		// the title-only scan always had. Dropping it would make a
		// transient read error unlist a file that Resolve's request-time
		// read may well succeed on a moment later.
		meta, err := scanDoc(path, relSlash)
		if err != nil {
			meta = docMeta{Title: titleFromFilename(relSlash)}
		}

		docs = append(docs, newDoc(root.Label, relSlash, resolved, info.ModTime(), meta))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
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
			target, _ := idx.ResolveLink(from, link.Raw)
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
	copy(out, idx.docs)
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
			return d, true
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
func (idx *Index) Resolve(rootLabel, relPath string) (string, error) {
	for _, r := range idx.roots {
		if r.Label != rootLabel {
			continue
		}
		resolved, err := ResolveInRoot(r.Path, relPath)
		if err != nil {
			return "", err
		}
		if r.OnlyFile != "" && resolved != r.OnlyFile {
			return "", ErrOutsideRoot
		}
		if !AllowedExt(resolved) {
			return "", ErrDisallowedExt
		}
		// Keyed on r.Label, so a file indexed under a DIFFERENT (possibly
		// overlapping) root does not satisfy membership for this one.
		if !idx.pathIndex[docKey{rootLabel: r.Label, absPath: resolved}] {
			return "", ErrNotIndexed
		}
		return resolved, nil
	}
	return "", ErrRootNotFound
}
