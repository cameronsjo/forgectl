package docs

import (
	"path"
	"strings"
)

// RootKind classifies how a root's link syntax and anchor semantics must be
// interpreted. detectRootKind (vault.go) infers it from the filesystem;
// Task 4's IndexOptions lets a caller override the inference.
type RootKind uint8

const (
	// RootDocs is an ordinary markdown docs tree: relative markdown links
	// only, GitHub-style heading anchors (goldmark's auto-ID slug).
	RootDocs RootKind = iota
	// RootVault is an Obsidian vault: wikilinks resolve by relative path,
	// bare filename, or frontmatter alias; anchors may be a heading or an
	// Obsidian block id.
	RootVault
)

// Miss classifies why ResolveLink (Task 3) could not produce a doc, or
// (MissNone) that the link fully resolved.
type Miss uint8

const (
	// MissNone means the link fully resolved — the target file, and its
	// fragment if one was given.
	MissNone Miss = iota
	// MissNoTarget means no doc matched the target, or the doc matched but
	// its heading/block-id fragment did not.
	MissNoTarget
	// MissAmbiguous means more than one doc matched the target with no
	// tiebreak available.
	MissAmbiguous
	// MissOutsideRoot means the target's reconstructed path escapes the
	// calling doc's root (a leading ".." or "/" after path.Clean).
	MissOutsideRoot
)

// LinkForm classifies the syntax a LinkRef was written in — reporting and
// future rendering, not resolution: ResolveLink dispatches on RootKind and
// on whether Fragment/Path are empty, never on Form.
type LinkForm uint8

const (
	// FormPlain is an ordinary wikilink or markdown link: [[note]].
	FormPlain LinkForm = iota
	// FormAlias is a wikilink whose display text differs from its target:
	// [[note|Display Text]].
	FormAlias
	// FormEmbed is an Obsidian embed: ![[note]].
	FormEmbed
	// FormHeading is a wikilink fragment that names a heading (does not
	// start with '^'): [[note#Heading]].
	FormHeading
	// FormBlock is a wikilink fragment that names an Obsidian block id:
	// [[note#^block-id]].
	FormBlock
	// FormRelPath is a plain (non-wikilink) markdown link whose destination
	// carries no URL scheme: [text](../guide.md#anchor).
	FormRelPath
)

// LinkRef is one outbound link scanDoc (linkscan.go) found in a document,
// already split into the form ResolveLink (Task 3) consumes directly.
type LinkRef struct {
	// Raw is the link's target exactly as authored, recombined from the
	// parser's own Target/Fragment split — before ResolveLink's
	// first-'#' reconstruction. Kept for diagnostics; ResolveLink reads
	// Path and Fragment, never Raw.
	Raw string
	// Path is Raw's portion before its FIRST '#' (the file/note target;
	// empty for a fragment-only link such as "[[#Heading]]"). This is the
	// Global Constraint's reconstruction: go.abhg.dev/goldmark/wikilink
	// splits Target/Fragment on the LAST '#', which is wrong for a nested
	// heading path like "[[note#A#B]]" — scanDoc recombines and re-splits
	// on the first '#' so Path is "note" and Fragment is "A#B", not the
	// library's own Target "note#A" / Fragment "B".
	Path string
	// Fragment is Raw's portion after its first '#' ("" when Raw has no
	// '#'). A nested heading path ("A#B") is kept whole here — matching
	// only its LAST segment against a heading is ResolveLink's job, not
	// scanDoc's.
	Fragment string
	// Form classifies the link's authored syntax; see LinkForm. An
	// Obsidian embed is Form == FormEmbed — there is no separate flag to
	// keep in step with it.
	Form LinkForm
}

// Heading is one heading scanDoc found in a document: its rendered text and
// the id goldmark's auto-heading pass assigned it. Slug MUST agree with
// render.go's md.Convert output (parser.WithAutoHeadingID(), enabled at
// render.go:73) — a docs-root anchor link resolves by matching this slug,
// so a resolver whose slug disagreed with the rendered page's actual id
// would confidently resolve to an anchor the browser can't find.
type Heading struct {
	// Text is the heading's rendered text (inline formatting stripped).
	Text string
	// Slug is the id goldmark's auto-heading-id pass assigned this
	// heading, lowercase per goldmark's own convention.
	Slug string
}

// rootIndex holds one root's link-resolution lookup tables. Task 3
// populates it inside NewIndex, right after the pathIndex loop, and
// ResolveLink reads it via Index.byRoot[from.RootLabel] — resolution never
// crosses roots (Global Constraint).
//
// Key normalization, load-bearing for every table, lives in relKey and
// nameKey: builder and every lookup go through them, so a target is folded
// exactly as the doc that should match it was. Keys use forward slashes
// (built from Doc.RelPath, which is already slash-normalized).
//
//   - byRel: extension-stripped RelPath -> indices into
//     Index.docs. The primary table for BOTH root kinds — a docs root
//     resolves relative markdown links through it exclusively; a vault
//     root tries it first, before falling back to byName then byAlias.
//   - byName: basename (extension stripped, directory components dropped)
//     -> indices into Index.docs. Built for every root; consulted for vault
//     roots only.
//   - byAlias: lowercased frontmatter alias -> indices into Index.docs.
//     Built for every root; consulted for vault roots only, as the last
//     fallback.
//
// Case: a vault root folds every key (fold == true), matching Obsidian's
// case-insensitive links. A docs root keeps exact case — a relative markdown
// link names one file exactly, and on a case-sensitive filesystem
// "Guide.md" and "guide.md" are two files. The extension strip removes a
// MARKDOWN extension only (stripMarkdownExt): "Node.js.md" keys as
// "Node.js", never "Node".
//
// Values are []int — indices into Index.docs — rather than []*Doc, so the
// table doesn't need to pin per-doc pointers independently of Index's own
// slice-of-structs storage; Task 3 dereferences through Index.docs[i].
type rootIndex struct {
	// fold is true when keys are case-folded (vault roots).
	fold    bool
	byRel   map[string][]int
	byName  map[string][]int
	byAlias map[string][]int
}

// relKey folds a slash-separated relative path to byRel's key shape for
// this root: markdown extension stripped, lowercased when the root folds.
// Builder and every lookup go through it, so the fold is stated once.
func (ri *rootIndex) relKey(p string) string {
	key := stripMarkdownExt(p)
	if ri.fold {
		key = strings.ToLower(key)
	}
	return key
}

// nameKey is relKey applied to p's last path segment — byName's key shape.
func (ri *rootIndex) nameKey(p string) string {
	return ri.relKey(path.Base(p))
}

// stripMarkdownExt removes a markdown extension (AllowedExt: .md or
// .markdown, any case) and nothing else. path.Ext would also strip the
// ".js" from "Node.js", turning a distinct note into a lookup miss — or,
// with a "Node.md" beside it, into a confident hit on the wrong file.
func stripMarkdownExt(p string) string {
	if AllowedExt(p) {
		return strings.TrimSuffix(p, path.Ext(p))
	}
	return p
}

// buildRootIndexes builds one rootIndex per root, scanning docs once. Called
// from NewIndex (index.go) right after the pathIndex loop.
func buildRootIndexes(roots []Root, docs []Doc) map[string]*rootIndex {
	out := make(map[string]*rootIndex, len(roots))
	for _, r := range roots {
		out[r.Label] = &rootIndex{
			fold:    r.Kind == RootVault,
			byRel:   map[string][]int{},
			byName:  map[string][]int{},
			byAlias: map[string][]int{},
		}
	}
	for i, d := range docs {
		ri, ok := out[d.RootLabel]
		if !ok {
			continue
		}
		relKey := ri.relKey(d.RelPath)
		ri.byRel[relKey] = append(ri.byRel[relKey], i)
		if !ri.fold {
			// A docs root keys the exact path too, so "[x](notes.md)" and
			// "[x](notes.markdown)" each name one file; the stripped key
			// above serves an extension-less link, which is then
			// genuinely ambiguous when both files exist.
			ri.byRel[d.RelPath] = append(ri.byRel[d.RelPath], i)
		}

		nameKey := ri.nameKey(d.RelPath)
		ri.byName[nameKey] = append(ri.byName[nameKey], i)

		for _, alias := range d.Aliases {
			aliasKey := strings.ToLower(alias)
			ri.byAlias[aliasKey] = append(ri.byAlias[aliasKey], i)
		}
	}
	return out
}

// rootByLabel returns the Root with the given Label, if indexed.
func (idx *Index) rootByLabel(label string) (Root, bool) {
	for _, r := range idx.roots {
		if r.Label == label {
			return r, true
		}
	}
	return Root{}, false
}

// pickCandidate turns a table lookup's index slice into a resolution
// verdict: no indices is MissNoTarget (try the next table, or give up), one
// index is the hit, more than one is MissAmbiguous — the seat's stop
// condition (d) verdict (Task 1 Learnings) declined an Obsidian
// same-folder-then-shortest-path tiebreak, so any true multi-match stays
// ambiguous rather than being narrowed further.
func (idx *Index) pickCandidate(indices []int) (*Doc, Miss) {
	switch len(indices) {
	case 0:
		return nil, MissNoTarget
	case 1:
		return &idx.docs[indices[0]], MissNone
	default:
		return nil, MissAmbiguous
	}
}

// filterBySuffix narrows a byName candidate list to the ones whose RelPath
// key ends with a "/"-qualified target — how "[[deep/Alpha]]" picks the one
// Alpha.md a bare "[[Alpha]]" can't. clean is already path.Clean'd; both
// sides go through relKey so they compare in the same shape.
func filterBySuffix(ri *rootIndex, docs []Doc, indices []int, clean string) []int {
	want := ri.relKey(clean)
	var out []int
	for _, i := range indices {
		key := ri.relKey(docs[i].RelPath)
		if key == want || strings.HasSuffix(key, "/"+want) {
			out = append(out, i)
		}
	}
	return out
}

// escapesRoot reports whether a path.Clean'd target has walked outside the
// root it's being resolved against: a leading ".." segment (after
// path.Clean, the only way "up" can survive) or an absolute path.
func escapesRoot(clean string) bool {
	return clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean)
}

// resolveDocsDoc resolves path0 for a RootDocs caller: an ordinary
// relative markdown link, joined against the calling doc's own directory.
// A leading "/" is root-relative, as GitHub renders it — never the
// filesystem root, and never joined onto the calling doc's directory
// (path.Join would fold "/guide.md" from "sub/" into "sub/guide.md", a
// confident hit on the wrong file when one exists there). byRel is the
// only table a docs root ever consults (Global Constraint).
func (idx *Index) resolveDocsDoc(rootIdx *rootIndex, from *Doc, path0 string) (*Doc, Miss) {
	var clean string
	if path.IsAbs(path0) {
		clean = strings.TrimPrefix(path.Clean(path0), "/")
	} else {
		clean = path.Clean(path.Join(path.Dir(from.RelPath), path0))
	}
	if escapesRoot(clean) {
		return nil, MissOutsideRoot
	}
	key := clean
	if rootIdx.fold || !AllowedExt(clean) {
		key = rootIdx.relKey(clean)
	}
	return idx.pickCandidate(rootIdx.byRel[key])
}

// isExplicitlyRelative reports whether a link target was authored relative
// to the linking document — it starts with "./" or "../". Obsidian resolves
// such markdown-link paths against the source file's own directory, not
// the vault top; every other vault target goes through the vault-relative
// fallback chain.
func isExplicitlyRelative(target string) bool {
	return strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../")
}

// resolveVaultDoc resolves path0 for a RootVault caller. A target authored
// relative to the linking doc ("./sibling.md", "../other.md") resolves
// exactly as it would in a docs root — joined against from's directory.
// Anything else takes Obsidian's own fallback chain — byRel (a path
// relative to the vault root) first, then byName (a bare basename), then
// byAlias (a frontmatter alias) — stopping at the first table that produces
// ANY answer, including an ambiguous one. A "/" in the target narrows
// byName's candidates by RelPath suffix (the "[[deep/Alpha]]" case); a
// bare basename never touches byRel at all.
func (idx *Index) resolveVaultDoc(rootIdx *rootIndex, from *Doc, path0 string) (*Doc, Miss) {
	if isExplicitlyRelative(path0) {
		return idx.resolveDocsDoc(rootIdx, from, path0)
	}
	clean := path.Clean(path0)
	if escapesRoot(clean) {
		return nil, MissOutsideRoot
	}
	if doc, miss := idx.pickCandidate(rootIdx.byRel[rootIdx.relKey(clean)]); miss != MissNoTarget {
		return doc, miss
	}

	candidates := rootIdx.byName[rootIdx.nameKey(clean)]
	if strings.Contains(clean, "/") {
		candidates = filterBySuffix(rootIdx, idx.docs, candidates, clean)
	}
	if doc, miss := idx.pickCandidate(candidates); miss != MissNoTarget {
		return doc, miss
	}

	return idx.pickCandidate(rootIdx.byAlias[strings.ToLower(clean)])
}

// resolveFragment checks target's fragment against doc's anchors, once the
// file itself has resolved (or the link was fragment-only, in which case
// doc is the calling doc itself). An empty fragment always succeeds — the
// caller only wanted the file. A "^id" fragment is an Obsidian block-id
// reference and is checked identically for both root kinds (a docs root
// simply never has any BlockIDs to match, so it always misses there). A
// heading fragment matches on its LAST '#'-segment (the nested-heading
// case, "[[note#A#B]]" -> match "B"): a docs root matches goldmark's exact
// auto-ID slug, case-sensitively — the slug is what a browser matches
// against "id=", and a browser does not fold (the Global Constraint's
// slug-agreement pin); a vault root additionally accepts a case-folded
// match on the slug or the heading's rendered text, mirroring Obsidian's
// own case-insensitive heading links.
func (idx *Index) resolveFragment(kind RootKind, doc *Doc, fragment string) (*Doc, Miss) {
	if fragment == "" {
		return doc, MissNone
	}
	if id, ok := strings.CutPrefix(fragment, "^"); ok {
		for _, b := range doc.BlockIDs {
			if b == id {
				return doc, MissNone
			}
		}
		return doc, MissNoTarget
	}

	segments := strings.Split(fragment, "#")
	last := segments[len(segments)-1]

	if kind == RootVault {
		lastLower := strings.ToLower(last)
		for _, h := range doc.Headings {
			if h.Slug == lastLower || strings.ToLower(h.Text) == lastLower {
				return doc, MissNone
			}
		}
		return doc, MissNoTarget
	}

	for _, h := range doc.Headings {
		if h.Slug == last {
			return doc, MissNone
		}
	}
	return doc, MissNoTarget
}

// ResolveLink resolves target — a link's raw target text, first-'#' split
// exactly as scanDoc's LinkRef.Path/Fragment already are (a caller may pass
// LinkRef.Raw directly; ResolveLink performs the identical first-'#' split
// itself) — against the tables built for from's own root. Resolution NEVER
// crosses roots: idx.byRoot[from.RootLabel] is the only table consulted
// (Global Constraint).
//
// Contract: a non-nil Doc paired with MissNoTarget means the file itself
// resolved but its anchor fragment did not; every other Miss returns a nil
// Doc. MissNone means both the file (if any was named) and the fragment (if
// any was given) resolved.
//
// Splitting on the first '#' is Obsidian's own limitation, not a shortcut
// unique to this resolver: go.abhg.dev/goldmark/wikilink splits on the
// LAST '#' (parser.go:83-85), so "[[note#A#B]]" needs recombining and
// re-splitting on the first '#' to get the intended path "note" / fragment
// "A#B" (see LinkRef.Path's doc comment) — but that reconstruction also
// means a filename genuinely containing '#' can never be the target of a
// link: everything from its first '#' onward reads as a fragment, with no
// escape syntax. The Task 1 spike found five such filenames in the live
// vault (Learnings); this is Obsidian's own limitation, carried through
// unchanged, not a defect in this resolver.
func (idx *Index) ResolveLink(from *Doc, target string) (*Doc, Miss) {
	if from == nil {
		return nil, MissNoTarget
	}
	root, ok := idx.rootByLabel(from.RootLabel)
	if !ok {
		return nil, MissNoTarget
	}
	rootIdx := idx.byRoot[from.RootLabel]
	if rootIdx == nil {
		return nil, MissNoTarget
	}

	path0, fragment := splitFirstHash(target)

	doc := from
	if path0 != "" {
		var miss Miss
		if root.Kind == RootVault {
			doc, miss = idx.resolveVaultDoc(rootIdx, from, path0)
		} else {
			doc, miss = idx.resolveDocsDoc(rootIdx, from, path0)
		}
		if miss != MissNone {
			return doc, miss
		}
	}

	return idx.resolveFragment(root.Kind, doc, fragment)
}
