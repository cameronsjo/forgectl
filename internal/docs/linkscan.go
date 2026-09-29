package docs

import (
	"bufio"
	"bytes"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/wikilink"
	"gopkg.in/yaml.v3"
)

// docMeta is scanDoc's one-pass result — everything Doc needs about a
// document (Title, Aliases, Headings, BlockIDs, Links) read from a single
// file open, plus the raw OKF trust fields (Status, StaleAfter; see trust.go).
type docMeta struct {
	Title      string
	Aliases    []string
	Headings   []Heading
	BlockIDs   []string
	Links      []LinkRef
	Status     string
	StaleAfter string
}

// linkMarkdown is the goldmark instance scanDoc parses document BODIES
// with (frontmatter is split off by splitFrontmatter before this ever sees
// the bytes). It carries only what scanDoc needs to extract: the heading-id
// rule, taken from render.go's headingParserOptions so the slug this
// package computes is the id the browser actually renders, and the
// wikilink extension. Extensions that only affect rendered *output* (GFM,
// syntax highlighting, frontmatter) are deliberately absent: scanDoc never
// renders.
//
// Not locked: the index build that drives scanDoc is single-goroutine.
// render.go serializes its twin behind renderMu because third-party
// extensions may not be concurrency-safe; a parallel scan would need the
// same treatment or one instance per worker.
var linkMarkdown = newLinkMarkdown()

// linkMarkdownVault is the vault scan parser. It is built by the vault
// render constructor itself (newMarkdown with the vault flag, without the
// frontmatter extension because scanDoc strips frontmatter first), so the
// index parses a vault note with exactly the inline, block and delimiter set
// the page does: GFM, math, ==, ~~, #tag, [[…]] and the %% comment parsers
// and transformer. A %% therefore pairs the same way in both, and the index
// holds exactly the headings, slugs, links and block ids the page shows.
// It is never used to render. Its own instance, not the render one, so an
// index build never contends for renderMu.
var linkMarkdownVault = newMarkdown(false, true)

func newLinkMarkdown() goldmark.Markdown {
	md := goldmark.New(
		goldmark.WithParserOptions(headingParserOptions(false)...),
		// Parse-only: a $$ block's lines are TeX, not headings or links.
		goldmark.WithParserOptions(mathBlockParserOptions()...),
	)
	// Parse-only: never render with this instance. The default resolver turns [[https://x/]] into an external href the sanitizer keeps.
	(&wikilink.Extender{}).Extend(md)
	return md
}

// blockIDPattern matches a trailing Obsidian block-id marker on a line:
// "...paragraph text ^block-id". scanBlockIDs applies it only to lines that
// are not part of a code block: a "^id" inside a fence or an indented block
// is code, not a marker.
var blockIDPattern = regexp.MustCompile(`\^([A-Za-z0-9_-]+)\s*$`)

// urlSchemePrefix matches a markdown link destination that already names a
// URL scheme (http:, mailto:, ...) — such destinations are never a
// same-vault or same-docs-tree target, so scanBody excludes them from
// Links entirely.
var urlSchemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func isURLLike(dest string) bool {
	return strings.HasPrefix(dest, "//") || urlSchemePrefix.MatchString(dest)
}

// scanDoc reads absPath once and returns everything Doc needs about it:
// title (same rule titleFor used to apply — the first "# " heading in the
// first 64 lines, else relPath's filename without extension), frontmatter
// aliases, headings (with goldmark's auto-ID slug), Obsidian ^block-id
// markers, and outbound links from both wikilinks and plain markdown links
// whose destination carries no URL scheme.
//
// scanDoc scans as a docs root; scanDocFor takes the root's kind.
func scanDoc(absPath, relPath string) (docMeta, error) {
	return scanDocFor(RootDocs, absPath, relPath)
}

// scanDocFor is scanDoc for a document in a root of the given kind. A vault
// root parses with linkMarkdownVault, whose comment parsers and heading-id
// transformer are the render's own, so %% comment text reaches none of the
// title, headings, slugs, links or block ids. Both kinds take their title
// from the parse (parsedTitle); only an over-cap document falls back to
// firstH1's line scan.
func scanDocFor(kind RootKind, absPath, relPath string) (docMeta, error) {
	f, err := os.Open(absPath) //nolint:gosec // G304: absPath is a doc walkRoot/indexFileRoot already resolved under a canonicalized, operator-configured root
	if err != nil {
		return docMeta{}, err
	}
	defer func() { _ = f.Close() }()
	// Read one byte past the cap so an over-cap file is detected without
	// reading the rest of it.
	source, err := io.ReadAll(io.LimitReader(f, maxScanBytes+1))
	if err != nil {
		return docMeta{}, err
	}

	if len(source) > maxScanBytes {
		// Past the cap there is no whole-document parse, so the title is
		// firstH1's line scan; a vault title is then parsed on its own to
		// drop its comments. A "# " line inside a fence or a %% block of an
		// over-cap document can still reach it.
		// The scan runs on the body: a YAML "# comment" in the frontmatter
		// is not a heading.
		scanSrc := source
		if fm, ok := splitFrontmatter(source); ok {
			scanSrc = fm.body
		}
		title := firstH1(scanSrc)
		if kind == RootVault && title != "" {
			title = vaultLineTitle(title)
		}
		if title == "" {
			title = titleFromFilename(relPath)
		}
		slog.Debug("docs: document exceeds scan cap; indexed by title only.",
			"path", relPath, "limit", maxScanBytes)
		return docMeta{Title: title}, nil
	}

	body := source
	var aliases []string
	var status, staleAfter string
	if fm, ok := splitFrontmatter(source); ok {
		body = fm.body
		// Decode the block once; aliases and trust both derive from it.
		if root := frontmatterRoot(fm); root != nil {
			aliases = aliasesFromNode(root)
			status, staleAfter = trustFields(root)
		}
	}

	scan, err := scanBodyFor(kind, body)
	if err != nil {
		return docMeta{}, fmt.Errorf("scan %s: %w", relPath, err)
	}
	// scanBodyFor numbers lines within body; shift them past the stripped
	// frontmatter so each Line is the line in the file as written. body is
	// always a suffix of source that starts a line, so the lines before it
	// are exactly the newlines before it.
	if fmLines := bytes.Count(source[:len(source)-len(body)], []byte("\n")); fmLines > 0 {
		for i := range scan.links {
			if scan.links[i].Line > 0 {
				scan.links[i].Line += fmLines
			}
		}
	}
	blockIDs := scanBlockIDs(body, scan.masked, scan.hidden)
	// Within the cap the title comes from the parse, in both root kinds, so
	// a "# " line inside a code fence, a $$ block, an indented code block or
	// the frontmatter is never a title the page does not render.
	title := parsedTitle(source, len(source)-len(body), body, scan.h1s)
	if title == "" {
		title = titleFromFilename(relPath)
	}
	headings, links := scan.headings, scan.links

	return docMeta{
		Title:      title,
		Aliases:    aliases,
		Headings:   headings,
		BlockIDs:   blockIDs,
		Links:      links,
		Status:     status,
		StaleAfter: staleAfter,
	}, nil
}

// maxScanBytes bounds how much of one document scanDoc reads and parses. It
// caps peak memory per file during an index build, and the watcher rebuilds
// on every filesystem event. A larger document is still listed, by title
// only, with no aliases, headings, block ids or links.
const maxScanBytes = 1 << 20

// titleScanLines bounds firstH1's search for a "# " heading. A title is
// expected near the top; scanning the whole file would make a heading
// buried under a long preamble the title, and would cost a full line scan
// on every doc that has none.
const titleScanLines = 64

// titleFromFilename is the title a document gets when no title heading is
// found: its filename without extension.
func titleFromFilename(relPath string) string {
	base := filepath.Base(relPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// firstH1 returns a document's first level-1 heading text, or "" if none
// appears in the first titleScanLines lines. A cheap line scan, not a parse,
// used only for a document over the scan cap: it does not know a code fence
// or a $$ block from a heading, which is why a parsed document takes
// parsedTitle instead.
func firstH1(source []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(source))
	for i := 0; i < titleScanLines && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "# "); ok {
			if t := strings.TrimSpace(after); t != "" {
				return t
			}
		}
	}
	return ""
}

// frontmatterRoot decodes a YAML frontmatter block once into its top-level
// node (the mapping, for a well-formed block), or nil for a TOML (+++) block,
// an undecodable block, or an empty one. Aliases are a YAML convention and
// OKF frontmatter is YAML, so a TOML block yields nothing.
func frontmatterRoot(fm frontmatterBlock) *yaml.Node {
	if fm.delim != '-' {
		return nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(fm.block, &node); err != nil || len(node.Content) == 0 {
		return nil
	}
	return node.Content[0]
}

// aliasesFromNode returns a frontmatter mapping's `aliases` value, accepting
// either a list or a bare scalar (folded to a one-element list). It decodes
// the already-parsed node into a map, so it sees exactly what a fresh
// yaml.Unmarshal of the block would (merge keys included); the trust fields
// read raw scalars instead (see trustFields).
func aliasesFromNode(mapping *yaml.Node) []string {
	var m map[string]any
	if err := mapping.Decode(&m); err != nil {
		return nil
	}
	return toStringList(m["aliases"])
}

func toStringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// scanBlockIDs returns the sorted, de-duplicated set of Obsidian block ids
// ("^block-id" suffixes) found in body, skipping every line that overlaps a
// code segment (fenced or indented code block content, and fence info
// strings). Overlap, not line-start containment: a blockquoted code
// segment starts after its "> " prefix. A marker that itself sits inside a
// hidden segment (an inline %% comment) is skipped too; only the marker is
// tested there, since an inline comment can share its line with a real one.
func scanBlockIDs(body []byte, code, hidden []text.Segment) []string {
	seen := map[string]bool{}
	for start := 0; start < len(body); {
		end := len(body)
		next := end
		if nl := bytes.IndexByte(body[start:], '\n'); nl >= 0 {
			end = start + nl
			next = end + 1
		}
		if !overlapsAny(code, start, next) {
			if m := blockIDPattern.FindSubmatchIndex(body[start:end]); m != nil &&
				!overlapsAny(hidden, start+m[0], start+m[1]) {
				seen[string(body[start+m[2]:start+m[3]])] = true
			}
		}
		start = next
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func overlapsAny(segs []text.Segment, start, end int) bool {
	for _, seg := range segs {
		if seg.Start < end && seg.Stop > start {
			return true
		}
	}
	return false
}

// scanBody walks body's goldmark AST once, collecting headings (text plus
// the auto-generated id slug) and outbound links from both wikilinks
// (go.abhg.dev/goldmark/wikilink) and plain markdown links whose
// destination carries no URL scheme. It also returns the source segments of
// every code block (fenced content, fence info strings, indented blocks) so
// scanBlockIDs can mask them.
//
// scanBody scans as a docs root; scanBodyFor takes the root's kind.
func scanBody(body []byte) ([]Heading, []LinkRef, []text.Segment, error) {
	scan, err := scanBodyFor(RootDocs, body)
	return scan.headings, scan.links, scan.masked, err
}

// bodyScan is scanBodyFor's result.
type bodyScan struct {
	headings []Heading
	links    []LinkRef
	// masked holds the segments scanBlockIDs must skip: code block content
	// and fence info strings, plus, in a vault root, every line a %% block
	// comment consumed.
	masked []text.Segment
	// hidden holds each inline comment's source range; a block-id marker
	// inside one is not indexed.
	hidden []text.Segment
	// h1s holds each level-1 heading, in document order, for parsedTitle.
	h1s []h1Candidate
}

// h1Candidate is one level-1 heading: where its text starts in the body,
// and that text with its comments cut out (visibleSource).
type h1Candidate struct {
	at      int
	visible string
}

func scanBodyFor(kind RootKind, body []byte) (bodyScan, error) {
	reader := text.NewReader(body)
	md := linkMarkdown
	if kind == RootVault {
		md = linkMarkdownVault
	}
	ctx := newParseContext()
	doc := md.Parser().Parse(reader, parser.WithContext(ctx))

	var headings []Heading
	var links []LinkRef
	var code, hidden []text.Segment
	var h1s []h1Candidate
	lines := lineIndex{src: body}

	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case kindCommentSpan:
			// Nothing under a comment is on the page, so nothing under it
			// is indexed: skip its links, and mask its block-id markers.
			if cs, ok := n.(*commentSpanNode); ok {
				hidden = append(hidden, text.NewSegment(cs.Start, cs.Stop))
			}
			return ast.WalkSkipChildren, nil
		case ast.KindFencedCodeBlock:
			if fb, ok := n.(*ast.FencedCodeBlock); ok {
				for i := 0; i < fb.Lines().Len(); i++ {
					code = append(code, fb.Lines().At(i))
				}
				if fb.Info != nil {
					code = append(code, fb.Info.Segment)
				}
			}
		case ast.KindCodeBlock, kindCommentBlock:
			for i := 0; i < n.Lines().Len(); i++ {
				code = append(code, n.Lines().At(i))
			}
		case kindMathBlock:
			// A $$ block's lines are TeX, not block-id markers, in either
			// root kind: the docs parser builds it too (mathBlockParserOptions).
			for i := 0; i < n.Lines().Len(); i++ {
				code = append(code, n.Lines().At(i))
			}
		case kindMermaidBlock:
			// The vault scan parses with the render constructor, whose
			// transformers promote mermaid fences off KindFencedCodeBlock;
			// their lines are still code. Vault only: the docs-root parser
			// builds no mermaid node.
			if kind == RootVault {
				for i := 0; i < n.Lines().Len(); i++ {
					code = append(code, n.Lines().At(i))
				}
			}
		case ast.KindHeading:
			h, ok := n.(*ast.Heading)
			if !ok {
				return ast.WalkContinue, nil
			}
			if h.Level == 1 && h.Lines().Len() > 0 {
				// One visibleSource walk, over the first line only, per
				// H1: linear in the heading, however many lines it has.
				first := h.Lines().At(0)
				h1s = append(h1s, h1Candidate{
					at:      first.Start,
					visible: strings.TrimSpace(string(visibleSource(h, first, body))),
				})
			}
			slug := ""
			if v, ok := h.AttributeString("id"); ok {
				if b, ok := v.([]byte); ok {
					slug = string(b)
				}
			}
			headings = append(headings, Heading{
				Text: headingText(h, body),
				Slug: slug,
			})
		case wikilink.Kind:
			if hasImageAncestor(n) {
				return ast.WalkContinue, nil
			}
			if wl, ok := n.(*wikilink.Node); ok {
				ref := wikilinkRef(wl, body)
				ref.Line = lines.lineOf(n)
				links = append(links, ref)
			}
		case ast.KindLink:
			l, ok := n.(*ast.Link)
			if !ok || hasImageAncestor(n) {
				return ast.WalkContinue, nil
			}
			dest := string(l.Destination)
			if isURLLike(dest) {
				return ast.WalkContinue, nil
			}
			// A markdown destination is a URL reference, so a filename
			// with spaces arrives percent-encoded ("My%20Doc.md"); goldmark
			// keeps the source bytes verbatim. Split on '#' FIRST, then
			// decode each side: an encoded "%23" is a literal '#' in the
			// filename, and decoding before the split would turn it into a
			// fragment boundary. A malformed escape is left as written —
			// the link is then a miss, which is the truthful answer for a
			// destination no browser could open.
			path0, frag0 := splitFirstHash(dest)
			path0 = pathUnescapeOrRaw(path0)
			frag0 = pathUnescapeOrRaw(frag0)
			links = append(links, LinkRef{
				Raw:      dest,
				Path:     path0,
				Fragment: frag0,
				Form:     FormRelPath,
				Line:     lines.lineOf(n),
			})
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return bodyScan{}, err
	}

	// Comments in a paragraph commentTransformer removed are gone from the
	// tree; it left their ranges in the context.
	hidden = append(hidden, removedComments(ctx)...)

	return bodyScan{headings: headings, links: links, masked: code, hidden: hidden, h1s: h1s}, nil
}

// hasAncestorOfKind reports whether any ancestor of n has one of kinds.
func hasAncestorOfKind(n ast.Node, kinds ...ast.NodeKind) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		for _, k := range kinds {
			if p.Kind() == k {
				return true
			}
		}
	}
	return false
}

// hasImageAncestor reports whether n sits inside an image. An image's
// children become its alt text on the page, never a link, so a link or
// wikilink written there is not indexed (forgectl#596). The walk still
// descends into images, so a comment inside alt text stays masked.
func hasImageAncestor(n ast.Node) bool {
	return hasAncestorOfKind(n, ast.KindImage)
}

// lineIndex maps a byte offset in src to its 1-based line number. The
// line-start table is built on first use, so a document without links pays
// nothing, and each lookup is a binary search rather than a rescan.
type lineIndex struct {
	src    []byte
	starts []int
}

// lineOf returns the 1-based line of n's source position: the '[' that opens
// a link. An inline node the parser left unpositioned takes its nearest
// positioned ancestor's line; 0 means no position was found.
func (li *lineIndex) lineOf(n ast.Node) int {
	for ; n != nil; n = n.Parent() {
		if p := n.Pos(); p >= 0 {
			return li.line(p)
		}
	}
	return 0
}

func (li *lineIndex) line(offset int) int {
	if li.starts == nil {
		li.starts = []int{0}
		for i, b := range li.src {
			if b == '\n' {
				li.starts = append(li.starts, i+1)
			}
		}
	}
	// The count of line starts at or before offset is its 1-based line.
	return sort.Search(len(li.starts), func(i int) bool { return li.starts[i] > offset })
}

// parsedTitle is firstH1's rule applied to the parsed document instead of
// raw lines: the first level-1 heading within titleScanLines lines whose
// line, trimmed, starts with "# ", its text taken from the parse with
// comments cut out (visibleSource). Taking candidates from the parse means a
// "# " line inside a %% block, a code block, a $$ block or the frontmatter is
// never a title. bodyOffset is where body starts in source, past any
// frontmatter.
func parsedTitle(source []byte, bodyOffset int, body []byte, h1s []h1Candidate) string {
	for _, h := range h1s {
		if bytes.Count(source[:bodyOffset+h.at], []byte("\n")) >= titleScanLines {
			break
		}
		start := bytes.LastIndexByte(body[:h.at], '\n') + 1
		end := len(body)
		if nl := bytes.IndexByte(body[h.at:], '\n'); nl >= 0 {
			end = h.at + nl
		}
		if !strings.HasPrefix(strings.TrimSpace(string(body[start:end])), "# ") {
			continue
		}
		if h.visible != "" {
			return h.visible
		}
	}
	return ""
}

// vaultLineTitle is the over-cap vault title: firstH1's text parsed on its
// own as an H1, so its comments are cut the same way the full parse cuts
// them. It falls back to "" (the filename) if the line no longer parses as
// a heading with visible text.
func vaultLineTitle(title string) string {
	scan, err := scanBodyFor(RootVault, []byte("# "+title+"\n"))
	if err != nil || len(scan.h1s) == 0 {
		return ""
	}
	return scan.h1s[0].visible
}

func headingText(n *ast.Heading, source []byte) string {
	var b strings.Builder
	appendNodeText(&b, n, source)
	return b.String()
}

// appendNodeText flattens n's inline nodes into their rendered text, in one
// walk: markup is gone, escapes and entities are resolved.
// It is the one heading-text builder for both root kinds; a docs root reads
// Text nowhere (its anchors match the slug), so both kinds share the rule
// that a line break is a space. The vault-only
// nodes a docs root never produces are handled here too: a %% comment
// contributes nothing (it is not on the page), a tag contributes "#name",
// and inline math its "$…$" source, since neither has a text child.
func appendNodeText(b *strings.Builder, n ast.Node, source []byte) {
	switch t := n.(type) {
	case *commentSpanNode:
		return
	case *tagNode:
		b.WriteByte('#')
		b.Write(t.Name)
		return
	case *mathInline:
		delim := "$"
		if t.display {
			delim = "$$"
		}
		b.WriteString(delim)
		b.Write(t.tex)
		b.WriteString(delim)
		return
	case *ast.Text:
		appendRenderedText(b, t, source)
		// A setext heading's line break reads as a space, as it renders.
		if t.SoftLineBreak() || t.HardLineBreak() {
			b.WriteByte(' ')
		}
		return
	case *ast.AutoLink:
		// "<https://x>" or a GFM bare URL has no Text child; the page
		// shows its label, as written.
		b.Write(t.Label(source))
		return
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		appendNodeText(b, c, source)
	}
}

// appendRenderedText writes t's text as the page shows it. A raw segment
// (a code span's) is written as is; any other goes through goldmark's own
// text writer, which drops a backslash escape and resolves an entity
// reference, and is then unescaped back from HTML. So "foo\_bar" reads
// "foo_bar" and "&amp;" reads "&", as they render.
func appendRenderedText(b *strings.Builder, t *ast.Text, source []byte) {
	v := t.Segment.Value(source)
	if t.IsRaw() {
		b.Write(v)
		return
	}
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	goldmarkhtml.DefaultWriter.Write(w, v)
	if err := w.Flush(); err != nil {
		// A bytes.Buffer never fails a write; keep the source text if it did.
		b.Write(v)
		return
	}
	b.WriteString(html.UnescapeString(buf.String()))
}

// pathUnescapeOrRaw percent-decodes s, or returns it unchanged when it is
// not a valid escape sequence.
func pathUnescapeOrRaw(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

// splitFirstHash splits s on its FIRST '#' — the Global Constraint's
// reconstruction rule.
func splitFirstHash(s string) (string, string) {
	idx := strings.Index(s, "#")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// wikilinkRef converts a parsed wikilink.Node into a LinkRef, reconstructing
// the target per the Global Constraint: go.abhg.dev/goldmark/wikilink
// splits on the LAST '#' (so "[[note#A#B]]" parses as Target="note#A",
// Fragment="B"), but Obsidian's nested-heading syntax means the FIRST '#'
// is the real path/fragment boundary. Recombining Target and Fragment and
// re-splitting on the first '#' undoes the library's split, producing the
// intended Path "note", Fragment "A#B".
func wikilinkRef(wl *wikilink.Node, source []byte) LinkRef {
	target := string(wl.Target)
	frag := string(wl.Fragment)
	raw := target
	if frag != "" {
		raw = target + "#" + frag
	}
	isAlias := false
	if c := wl.FirstChild(); c != nil {
		if tn, ok := c.(*ast.Text); ok {
			label := string(tn.Segment.Value(source))
			isAlias = label != raw
		}
	}
	// "[[note\|alias]]" is Obsidian's alias escaped for a table cell, and
	// it reads the same outside one. The library splits on the '|' and
	// keeps the backslash in the target, so drop that one backslash.
	if isAlias && strings.HasSuffix(raw, `\`) {
		raw = strings.TrimSuffix(raw, `\`)
	}
	path0, frag0 := splitFirstHash(raw)

	form := FormPlain
	switch {
	case wl.Embed:
		form = FormEmbed
	case strings.HasPrefix(frag0, "^"):
		form = FormBlock
	case frag0 != "":
		form = FormHeading
	case isAlias:
		form = FormAlias
	}

	return LinkRef{
		Raw:      raw,
		Path:     path0,
		Fragment: frag0,
		Form:     form,
	}
}
