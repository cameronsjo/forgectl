package docs

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/wikilink"
	"gopkg.in/yaml.v3"
)

// docMeta is scanDoc's one-pass result — everything Doc needs about a
// document (Title, Aliases, Headings, BlockIDs, Links) read from a single
// file open.
type docMeta struct {
	Title    string
	Aliases  []string
	Headings []Heading
	BlockIDs []string
	Links    []LinkRef
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

func newLinkMarkdown() goldmark.Markdown {
	md := goldmark.New(
		goldmark.WithParserOptions(headingParserOptions()...),
	)
	(&wikilink.Extender{}).Extend(md)
	return md
}

// blockIDPattern matches a trailing Obsidian block-id marker on a line:
// "...paragraph text ^block-id".
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
func scanDoc(absPath, relPath string) (docMeta, error) {
	source, err := os.ReadFile(absPath) //nolint:gosec // G304: absPath is a doc walkRoot/indexFileRoot already resolved under a canonicalized, operator-configured root
	if err != nil {
		return docMeta{}, err
	}

	title := firstH1(source)
	if title == "" {
		title = titleFromFilename(relPath)
	}

	body := source
	var aliases []string
	if fm, ok := splitFrontmatter(source); ok {
		body = fm.body
		aliases = frontmatterAliases(fm)
	}

	headings, links, err := scanBody(body)
	if err != nil {
		return docMeta{}, fmt.Errorf("scan %s: %w", relPath, err)
	}
	blockIDs := scanBlockIDs(body)

	return docMeta{
		Title:    title,
		Aliases:  aliases,
		Headings: headings,
		BlockIDs: blockIDs,
		Links:    links,
	}, nil
}

// titleScanLines bounds firstH1's search for a "# " heading. A title is
// expected near the top; scanning the whole file would make a heading
// buried under a long preamble the title, and would cost a full line scan
// on every doc that has none.
const titleScanLines = 64

// titleFromFilename is the title a document gets when firstH1 finds no
// heading: its filename without extension.
func titleFromFilename(relPath string) string {
	base := filepath.Base(relPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// firstH1 returns a document's first level-1 heading text, or "" if none
// appears in the first titleScanLines lines. A cheap line scan, not a parse:
// the heading text is taken verbatim, as the sidenav has always shown it.
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

// frontmatterAliases returns a YAML frontmatter block's `aliases` value,
// accepting either a list or a bare scalar (folded to a one-element list).
// A TOML (+++) block yields none: Obsidian aliases are a YAML convention,
// and the plan scopes alias extraction to YAML.
func frontmatterAliases(fm frontmatterBlock) []string {
	if fm.delim != '-' {
		return nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(fm.block, &m); err != nil {
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
// ("^block-id" suffixes) found in body.
func scanBlockIDs(body []byte) []string {
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if m := blockIDPattern.FindSubmatch(scanner.Bytes()); m != nil {
			seen[string(m[1])] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// scanBody walks body's goldmark AST once, collecting headings (text plus
// the auto-generated id slug) and outbound links from both wikilinks
// (go.abhg.dev/goldmark/wikilink) and plain markdown links whose
// destination carries no URL scheme.
func scanBody(body []byte) ([]Heading, []LinkRef, error) {
	reader := text.NewReader(body)
	doc := linkMarkdown.Parser().Parse(reader)

	var headings []Heading
	var links []LinkRef

	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindHeading:
			h, ok := n.(*ast.Heading)
			if !ok {
				return ast.WalkContinue, nil
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
			if wl, ok := n.(*wikilink.Node); ok {
				links = append(links, wikilinkRef(wl, body))
			}
		case ast.KindLink:
			l, ok := n.(*ast.Link)
			if !ok {
				return ast.WalkContinue, nil
			}
			dest := string(l.Destination)
			if isURLLike(dest) {
				return ast.WalkContinue, nil
			}
			// A markdown destination is a URL reference, so a filename
			// with spaces arrives percent-encoded ("My%20Doc.md"); goldmark
			// keeps the source bytes verbatim. Decode so the target compares
			// against the filename as it exists on disk. A malformed escape
			// is left as written — the link is then a miss, which is the
			// truthful answer for a destination no browser could open.
			if decoded, err := url.PathUnescape(dest); err == nil {
				dest = decoded
			}
			path0, frag0 := splitFirstHash(dest)
			links = append(links, LinkRef{
				Raw:      dest,
				Path:     path0,
				Fragment: frag0,
				Form:     FormRelPath,
			})
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, nil, err
	}

	return headings, links, nil
}

func headingText(n *ast.Heading, source []byte) string {
	var b strings.Builder
	appendNodeText(&b, n, source)
	return b.String()
}

func appendNodeText(b *strings.Builder, n ast.Node, source []byte) {
	if t, ok := n.(*ast.Text); ok {
		b.Write(t.Segment.Value(source))
		return
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		appendNodeText(b, c, source)
	}
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
	path0, frag0 := splitFirstHash(raw)

	isAlias := false
	if c := wl.FirstChild(); c != nil {
		if tn, ok := c.(*ast.Text); ok {
			label := string(tn.Segment.Value(source))
			isAlias = label != raw
		}
	}

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
