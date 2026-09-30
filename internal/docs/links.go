package docs

import (
	"path"
	"strings"
	"sync"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
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
	// MissAttachment means a vault wikilink matched no doc but did match an
	// attachment, an existing non-markdown file in the root
	// (resolveAttachment). There is no Doc and no href: no route serves the
	// file yet, so the reader renders it as marked text, and docs check
	// counts it as resolved (forgectl#709). Only resolveWikilink returns it;
	// ResolveLink never does.
	MissAttachment
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
	// Line is the 1-based line of the link's opening bracket in the file as
	// written, frontmatter included; 0 when the parser gave no position.
	Line int
}

// Heading is one heading scanDoc found in a document: its rendered text and
// the id goldmark's auto-heading pass assigned it. Slug MUST agree with
// render.go's md.Convert output (parser.WithAutoHeadingID(), enabled at
// render.go:73) — a docs-root anchor link resolves by matching this slug,
// so a resolver whose slug disagreed with the rendered page's actual id
// would confidently resolve to an anchor the browser can't find.
type Heading struct {
	// Text is the heading's rendered text, its inline formatting flattened
	// away (appendNodeText): "## a *b* `c`" holds "a b c", "## foo\_bar"
	// holds "foo_bar", and a vault %% comment contributes nothing. A vault
	// fragment is compared against it under foldHeadingKey (see
	// matchFragment), so a link written with or without the markup matches.
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
//   - attRel / attByName: a vault root's attachments (walkRoot), keyed by
//     attKey of the root-relative path and of the basename; the attByName
//     values are the attKey'd paths. attKey folds case like relKey but never
//     strips an extension: an attachment's extension is part of its name, so
//     "[[foo.md]]" must not reach a file named "foo". Consulted by
//     resolveAttachment only, after every doc table has missed. Empty for a
//     docs root.
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
	fold      bool
	byRel     map[string][]int
	byName    map[string][]int
	byAlias   map[string][]int
	attRel    map[string]bool
	attByName map[string][]string
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

// attKey folds a slash-separated relative path to the attachment tables' key
// shape: lowercased when the root folds, and nothing stripped.
func (ri *rootIndex) attKey(p string) string {
	if ri.fold {
		return strings.ToLower(p)
	}
	return p
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

// buildRootIndexes builds one rootIndex per root, scanning docs once, and
// keys each root's attachments (walkRoot's list, by root label). Called from
// NewIndex (index.go) right after the pathIndex loop.
func buildRootIndexes(roots []Root, docs []Doc, attachments map[string][]string) map[string]*rootIndex {
	out := make(map[string]*rootIndex, len(roots))
	for _, r := range roots {
		ri := &rootIndex{
			fold:      r.Kind == RootVault,
			byRel:     map[string][]int{},
			byName:    map[string][]int{},
			byAlias:   map[string][]int{},
			attRel:    map[string]bool{},
			attByName: map[string][]string{},
		}
		for _, rel := range attachments[r.Label] {
			key := ri.attKey(rel)
			ri.attRel[key] = true
			name := ri.attKey(path.Base(rel))
			ri.attByName[name] = append(ri.attByName[name], key)
		}
		out[r.Label] = ri
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

// attachmentsByName is the root's attByName table: every attachment, keyed
// by folded basename, one attKey'd path per file, so two files whose names
// differ only in case appear twice. It determines attRel, so it is all of
// what resolveAttachment reads. nil for a root with no tables.
func (idx *Index) attachmentsByName(label string) map[string][]string {
	if ri := idx.byRoot[label]; ri != nil {
		return ri.attByName
	}
	return nil
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

// namesDirectory reports whether a link path, as authored and before
// path.Clean, names a directory: its last segment is empty (a trailing "/"),
// "." or "..".
func namesDirectory(path0 string) bool {
	last := path0[strings.LastIndex(path0, "/")+1:]
	return last == "" || last == "." || last == ".."
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
// A leading "/" is vault-root-relative. Anything else takes Obsidian's own fallback chain — byRel (a path
// relative to the vault root) first, then byName (a bare basename), then
// byAlias (a frontmatter alias) — stopping at the first table that produces
// ANY answer, including an ambiguous one. A "/" in the target narrows
// byName's candidates by RelPath suffix (the "[[deep/Alpha]]" case); a
// bare basename never touches byRel at all.
func (idx *Index) resolveVaultDoc(rootIdx *rootIndex, from *Doc, path0 string) (*Doc, Miss) {
	if isExplicitlyRelative(path0) {
		return idx.resolveDocsDoc(rootIdx, from, path0)
	}
	// Obsidian reads a leading "/" as vault-root-relative — the same
	// place a bare path is already resolved from, so drop it rather than
	// reading it as an escape.
	clean := strings.TrimPrefix(path.Clean(path0), "/")
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

// resolveAttachment resolves a vault wikilink path that matched no doc
// against the root's attachments, by the shortest-path basename rule the
// forgectl#709 owner ruling set. Its tie and precedence behaviour has not
// been checked against Obsidian itself:
//
//   - A target written relative to the linking note ("./a.png", "../a.png")
//     names exactly that path, joined against the note's directory.
//   - Anything else is a basename match, narrowed by path suffix when the
//     target holds a "/" ("[[assets/logo.png]]" matches "assets/logo.png"
//     and "x/assets/logo.png", never "other/logo.png"). The match with the
//     fewest path segments, the one closest to the root, wins. A
//     root-relative path is always its own closest match, so this one rule
//     is both the shortest-path match and the root-relative lookup.
//   - Two matches equally close to the root are MissAmbiguous: the reader
//     reports the tie rather than guessing.
//
// The extension is part of the name: "[[logo]]" never
// reaches logo.png. A target written as a directory ("logo.png/") matches
// nothing. Only the walk's own entries are ever matched, so a symlink, a
// file under an excluded directory, and anything outside the root cannot
// resolve; the caller has already refused a target that escapes the root.
//
// A note always wins over an attachment, because the doc tables are
// consulted first: "[[x.png]]" reaches a note "x.png.md" over an attachment
// "x.png".
func resolveAttachment(ri *rootIndex, from *Doc, path0 string) Miss {
	if namesDirectory(path0) {
		return MissNoTarget
	}
	if isExplicitlyRelative(path0) {
		if ri.attRel[ri.attKey(path.Clean(path.Join(path.Dir(from.RelPath), path0)))] {
			return MissAttachment
		}
		return MissNoTarget
	}
	clean := strings.TrimPrefix(path.Clean(path0), "/")
	want := ri.attKey(clean)
	best, ties := -1, 0
	for _, key := range ri.attByName[ri.attKey(path.Base(clean))] {
		if key != want && !strings.HasSuffix(key, "/"+want) {
			continue
		}
		depth := strings.Count(key, "/")
		switch {
		case best < 0 || depth < best:
			best, ties = depth, 1
		case depth == best:
			ties++
		}
	}
	switch {
	case best < 0:
		return MissNoTarget
	case ties > 1:
		return MissAmbiguous
	default:
		return MissAttachment
	}
}

// resolveWikilink is the one resolution the reader's wikilinks
// (wikilinkTarget) and docs check share. It is resolveAnchor, and then, for
// a vault wikilink or embed whose path matched no doc, resolveAttachment. A
// plain markdown link (FormRelPath) never takes the attachment step: docs
// check keeps its on-disk existence fallback for those.
func (idx *Index) resolveWikilink(from *Doc, ref LinkRef, budget *fragmentBudget) (*Doc, string, Miss) {
	doc, anchor, miss := idx.resolveAnchor(from, ref.Path, ref.Fragment, budget)
	if from == nil || doc != nil || miss != MissNoTarget || ref.Path == "" || ref.Form == FormRelPath {
		return doc, anchor, miss
	}
	root, ok := idx.rootByLabel(from.RootLabel)
	if !ok || root.Kind != RootVault {
		return doc, anchor, miss
	}
	return nil, "", resolveAttachment(idx.byRoot[from.RootLabel], from, ref.Path)
}

// matchFragment checks target's fragment against doc's anchors, once the
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
// match on the slug, or a match under foldHeadingKey (case and whitespace
// only) between the heading's rendered Text and either the fragment as
// written or the fragment's own rendered text (fragmentText), so
// "[[Note#a ==b==]]" and "[[Note#a b]]" both reach "## a ==b==", while
// "[[Note#snakecase]]" does not reach "## snake_case". A slug or as-written
// match anywhere in the note takes precedence over a rendered-text match;
// the rendered text is computed only when needed (fragmentMayRender). Only
// the vault comparison folds.
//
// matchFragment also returns the anchor a rendered link jumps to: the
// matching heading's Slug, the id the page renders on it, or for a "^id"
// block reference blockAnchor of the indexed block id, the id a vault page
// renders on the block (blockIDTransformer). The anchor is always built
// from the indexed Doc, never from the fragment as written. A vault fragment
// takes the FIRST heading that matches, so a duplicate heading's link lands
// where the resolver says it does, never on its "-1" twin. An empty
// fragment yields no anchor.
//
// budget bounds the parse work one source document can cause (see
// fragmentBudget); nil is unlimited. A fragment the budget cannot cover is
// not parsed: it matches by slug or as written, or it misses.
func matchFragment(kind RootKind, doc *Doc, fragment string, budget *fragmentBudget) (anchor string, ok bool) {
	if fragment == "" {
		return "", true
	}
	if id, isBlock := strings.CutPrefix(fragment, "^"); isBlock {
		for _, b := range doc.BlockIDs {
			if b == id {
				return blockAnchor(b), true
			}
		}
		return "", false
	}

	segments := strings.Split(fragment, "#")
	last := segments[len(segments)-1]

	if kind == RootVault {
		// Two passes. The first compares the slug and the fragment as
		// written, which costs no parse. Only when that misses, and the
		// fragment could render differently from how it is written, is it
		// parsed (fragmentText) for a second pass, so an exact match wins
		// over a rendered one. An empty key names no text: a heading that
		// renders empty (all comment) is reached by its slug only.
		lastLower := strings.ToLower(last)
		rawKey := foldHeadingKey(last)
		for _, h := range doc.Headings {
			key := foldHeadingKey(h.Text)
			if h.Slug == lastLower || key != "" && key == rawKey {
				return h.Slug, true
			}
		}
		if !fragmentMayRender(last) || !budget.take(len(last)) {
			return "", false
		}
		textKey := foldHeadingKey(fragmentText(last))
		if textKey == "" || textKey == rawKey {
			return "", false
		}
		for _, h := range doc.Headings {
			if foldHeadingKey(h.Text) == textKey {
				return h.Slug, true
			}
		}
		return "", false
	}

	for _, h := range doc.Headings {
		if h.Slug == last {
			return h.Slug, true
		}
	}
	return "", false
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
	path0, fragment := splitFirstHash(target)
	return idx.resolveParts(from, path0, fragment, nil)
}

// resolveParts is ResolveLink after the split: path0 and fragment arrive
// already separated, as scanDoc's LinkRef carries them. buildBacklinks
// calls this directly so a path containing a literal '#' (authored as
// "%23" in a markdown link and decoded by scanDoc) is looked up as the
// path it is, rather than re-split at that '#'.
func (idx *Index) resolveParts(from *Doc, path0, fragment string, budget *fragmentBudget) (*Doc, Miss) {
	doc, _, miss := idx.resolveAnchor(from, path0, fragment, budget)
	return doc, miss
}

// resolveAnchor is resolveParts plus the anchor matchFragment found, which
// only a hit carries.
func (idx *Index) resolveAnchor(from *Doc, path0, fragment string, budget *fragmentBudget) (*Doc, string, Miss) {
	if from == nil {
		return nil, "", MissNoTarget
	}
	root, ok := idx.rootByLabel(from.RootLabel)
	if !ok {
		return nil, "", MissNoTarget
	}
	rootIdx := idx.byRoot[from.RootLabel]
	if rootIdx == nil {
		return nil, "", MissNoTarget
	}

	doc := from
	if path0 != "" {
		var miss Miss
		if root.Kind == RootVault {
			doc, miss = idx.resolveVaultDoc(rootIdx, from, path0)
		} else {
			doc, miss = idx.resolveDocsDoc(rootIdx, from, path0)
			// A directory link resolves to no doc. path.Clean has already
			// dropped the trailing "/" or "/.", so "sub/" looked up "sub"
			// and could land on a sibling sub.md. Docs roots only: a vault
			// keeps Obsidian's own fallback chain.
			if miss != MissOutsideRoot && namesDirectory(path0) {
				doc, miss = nil, MissNoTarget
			}
		}
		if miss != MissNone {
			return doc, "", miss
		}
	}

	anchor, ok := matchFragment(root.Kind, doc, fragment, budget)
	if !ok {
		return doc, "", MissNoTarget
	}
	return doc, anchor, MissNone
}

// wikilinkTarget is the href a rendered wikilink gets, and its verdict. The
// href is built ONLY from the indexed Doc the link resolved to and the Slug
// of the heading or block id it matched, through docHrefFragment; nothing
// in ref reaches it. A hit links to the doc and its heading or block. A doc
// that resolved while its heading or block id did not still links to the
// doc, without a fragment.
// Every other miss has no href at all, a MissAttachment hit included.
func (idx *Index) wikilinkTarget(from *Doc, ref LinkRef, budget *fragmentBudget) (href string, miss Miss) {
	doc, anchor, miss := idx.resolveWikilink(from, ref, budget)
	if doc == nil {
		if miss == MissNone {
			miss = MissNoTarget
		}
		return "", miss
	}
	if miss == MissNoTarget {
		return docHref(doc.RootLabel, doc.RelPath), miss
	}
	if miss != MissNone {
		return "", miss
	}
	return docHrefFragment(doc.RootLabel, doc.RelPath, anchor), MissNone
}

// foldHeadingKey folds case and whitespace only: it lowercases, collapses
// each whitespace run to one space, and trims. Every other character,
// punctuation included, must match as written, so "snake_case" and
// "snakecase" stay different headings.
func foldHeadingKey(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// fragmentMarkupBytes are the bytes that can make a fragment's rendered
// text differ from the fragment as written: an escape, emphasis, a code
// span, a highlight or strikethrough, a link, wikilink or image, raw HTML or
// an autolink, an entity, and a %% comment. A fragment with none of them
// renders as it is written (a GFM bare-URL autolink renders its own text,
// see appendNodeText), so fragmentText would return it unchanged. "$" (math)
// and "!" (an image) are absent on purpose: "$x$" and "!x" render as
// written, and an image needs its "[", already in the set. "#" is absent
// because it cannot occur: matchFragment splits on "#" first, so the segment
// it parses never holds one, and an ATX closing run ("Foo #") never reaches
// fragmentText (it leaves an empty last segment, which matches nothing).
// TestFragmentText_MarkupFreeRendersAsWritten pins that, and
// TestResolveVault_HeadingMatchEveryMarkupByte pins each byte below.
const fragmentMarkupBytes = "\\*_`=~[<&%"

// maxRenderedFragment caps the fragment length fragmentText will parse;
// above it, a fragment matches by slug and as written only. A heading link
// runs to tens of bytes, and 512 is past any heading written by hand. The
// cap bounds what one crafted link costs, since goldmark's inline pass is
// superlinear on unclosed-bracket input and the parse runs under renderMu
// when a page renders: measured on "[x](" repeated, one 512-byte parse
// takes about 0.6ms, while one 100 KB fragment took over 5s uncapped.
const maxRenderedFragment = 512

// maxFragmentParseBytes is the parsed-fragment text one source document may
// spend (fragmentBudget). A heading link runs to tens of bytes, so 64 KiB is
// about 2000 realistic 30-byte fragments and no real note reaches it; a
// hostile note of about 2000 maximal (maxRenderedFragment) fragments would
// otherwise hold renderMu for about 1s (#630).
const maxFragmentParseBytes = 64 << 10

// fragmentBudget is the remaining parse allowance of ONE source document:
// one per render, and one per document while docs check runs. A nil budget
// is unlimited, which ResolveLink's single lookups use.
//
// The bound makes resolution order-dependent: once a note has spent its
// budget, its LATER heading links with markup no longer match by rendered
// text, while earlier ones did. That is deliberate and fail-closed: a link
// past the budget misses (its slug and as-written match still work), and
// never lands on a different heading.
type fragmentBudget struct {
	remaining int
	// refused counts the fragments take turned away. docs check reads it
	// to tell a link the budget left unchecked from a real broken anchor.
	refused int
}

func newFragmentBudget() *fragmentBudget {
	return &fragmentBudget{remaining: maxFragmentParseBytes}
}

// take charges n bytes and reports whether they were covered. A refusal
// charges nothing, so a later, shorter fragment can still fit.
func (b *fragmentBudget) take(n int) bool {
	if b == nil {
		return true
	}
	if n > b.remaining {
		b.refused++
		return false
	}
	b.remaining -= n
	return true
}

// fragmentMayRender reports whether fragmentText is worth running on
// fragment: it is within maxRenderedFragment and holds a markup byte.
func fragmentMayRender(fragment string) bool {
	return len(fragment) <= maxRenderedFragment && strings.ContainsAny(fragment, fragmentMarkupBytes)
}

// fragmentMarkdown parses a vault heading fragment as a heading, with the
// vault scan's inline set, so fragmentText flattens it exactly as the scan
// flattens the heading itself. It has its own lock: matchFragment runs
// under renderMu when a page's wikilink resolver calls it, and outside it
// from buildBacklinks (NewIndex, and the watcher's Rebuild) and from
// ResolveLink, which can overlap a render.
var (
	fragmentMu       sync.Mutex
	fragmentMarkdown = newMarkdown(false, true)
)

// fragmentText is a vault heading fragment's rendered text: the fragment
// parsed as a heading's text and flattened by headingText, so a link
// written with the heading's markup ("a ==b==", "e *f*") compares equal to
// the heading's Text ("a b", "e f"). A fragment that does not parse as a
// heading is returned as written.
func fragmentText(fragment string) string {
	src := []byte("## " + strings.Join(strings.Fields(fragment), " ") + "\n")
	fragmentMu.Lock()
	doc := fragmentMarkdown.Parser().Parse(text.NewReader(src), parser.WithContext(newParseContext()))
	fragmentMu.Unlock()
	if h, ok := doc.FirstChild().(*ast.Heading); ok {
		return headingText(h, src)
	}
	return fragment
}
