package docs

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
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
// with (frontmatter is stripped by hand before this ever sees the bytes;
// see extractFrontmatterAliases). It carries only what scanDoc needs to
// extract: auto heading IDs — which MUST agree with render.go's rendering
// pipeline (parser.WithAutoHeadingID(), render.go:73) so a slug this
// package computes matches the id the browser actually renders — and the
// wikilink extension. Extensions that only affect rendered *output* (GFM,
// syntax highlighting, frontmatter) are deliberately absent: scanDoc never
// renders.
var linkMarkdown = newLinkMarkdown()

func newLinkMarkdown() goldmark.Markdown {
	md := goldmark.New(
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
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
		base := filepath.Base(relPath)
		title = strings.TrimSuffix(base, filepath.Ext(base))
	}

	aliases, bodyOffset := extractFrontmatterAliases(source)
	body := source[bodyOffset:]

	headings, links := scanBody(body)
	blockIDs := scanBlockIDs(body)

	return docMeta{
		Title:    title,
		Aliases:  aliases,
		Headings: headings,
		BlockIDs: blockIDs,
		Links:    links,
	}, nil
}

// firstH1 returns a document's first level-1 heading text, or "" if none
// appears in the first 64 lines — the same cheap line scan titleFor used
// to perform, preserved exactly so switching titleFor's callers to scanDoc
// changes no observable title.
func firstH1(source []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(source))
	for i := 0; i < 64 && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "# "); ok {
			if t := strings.TrimSpace(after); t != "" {
				return t
			}
		}
	}
	return ""
}

// isFrontmatterFence reports whether line (already stripped of its
// trailing newline) is a YAML frontmatter delimiter: three or more '-' and
// nothing else.
func isFrontmatterFence(line []byte) bool {
	line = bytes.TrimRight(line, "\r")
	if len(line) < 3 {
		return false
	}
	for _, c := range line {
		if c != '-' {
			return false
		}
	}
	return true
}

// extractFrontmatterAliases returns a document's frontmatter `aliases`
// (accepting either a YAML list or a bare scalar, folded to a list here)
// and the byte offset where the body starts (0 when there is no
// well-formed leading YAML frontmatter block). TOML frontmatter (+++
// fences) is intentionally not recognized — per the plan, its aliases are
// simply not extracted, and it is treated as ordinary body content.
func extractFrontmatterAliases(source []byte) (aliases []string, bodyOffset int) {
	lines := bytes.SplitAfter(source, []byte("\n"))
	if len(lines) == 0 {
		return nil, 0
	}
	first := bytes.TrimSuffix(lines[0], []byte("\n"))
	if !isFrontmatterFence(first) {
		return nil, 0
	}
	for i := 1; i < len(lines); i++ {
		line := bytes.TrimSuffix(lines[i], []byte("\n"))
		if !isFrontmatterFence(line) {
			continue
		}
		block := bytes.Join(lines[1:i], nil)
		offset := 0
		for j := 0; j <= i; j++ {
			offset += len(lines[j])
		}
		var m map[string]any
		if err := yaml.Unmarshal(block, &m); err == nil {
			if v, ok := m["aliases"]; ok {
				aliases = toStringList(v)
			}
		}
		return aliases, offset
	}
	return nil, 0
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
func scanBody(body []byte) ([]Heading, []LinkRef) {
	reader := text.NewReader(body)
	doc := linkMarkdown.Parser().Parse(reader)

	var headings []Heading
	var links []LinkRef

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
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

	return headings, links
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
		Embed:    wl.Embed,
		Form:     form,
	}
}
