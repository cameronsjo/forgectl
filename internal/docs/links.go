package docs

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
	// Embed is true for an Obsidian embed (![[...]]).
	Embed bool
	// Form classifies the link's authored syntax; see LinkForm.
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
// Key normalization, load-bearing for every table: every key is
// lowercased, and byRel's key additionally has its markdown extension
// stripped and uses forward slashes (it is built from Doc.RelPath, which is
// already slash-normalized). Folding case matches Obsidian's own
// case-insensitive link matching; Task 3 must apply the identical fold to
// every target it looks up, or a correctly-cased document silently stops
// matching a differently-cased link.
//
//   - byRel: lowercased, extension-stripped RelPath -> indices into
//     Index.docs. The primary table for BOTH root kinds — a docs root
//     resolves relative markdown links through it exclusively; a vault
//     root tries it first, before falling back to byName then byAlias.
//   - byName: lowercased basename (extension stripped, directory
//     components dropped) -> indices into Index.docs. Vault-root only.
//   - byAlias: lowercased frontmatter alias -> indices into Index.docs.
//     Vault-root only, and the last fallback.
//
// Values are []int — indices into Index.docs — rather than []*Doc, so the
// table doesn't need to pin per-doc pointers independently of Index's own
// slice-of-structs storage; Task 3 dereferences through Index.docs[i].
//nolint:unused // Task 3's NewIndex/ResolveLink populates and reads this; Task 2 builds the type only
type rootIndex struct {
	byRel   map[string][]int
	byName  map[string][]int
	byAlias map[string][]int
}
