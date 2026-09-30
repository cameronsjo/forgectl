package docs

// Tests for the markup guard (markupguard.go, forgectl#628, forgectl#596).

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// guardTriggers are the superlinear goldmark inputs measured on
// origin/main, each as a unit repeated through one paragraph (with an
// optional prefix run first), and the root kind that is slow on it.
var guardTriggers = []struct {
	name, pre, unit string
	kind            RootKind
}{
	{"link openers", "", "[x](", RootDocs},
	{"emphasis run", "", "a*", RootDocs},
	{"unmatched openers then closers", "_a ", "a* ", RootDocs},
	{"strikethrough", "_a ", "a~~", RootDocs},
	{"linkify long word", "", "a_", RootDocs},
	{"code spans", "", "`a", RootDocs},
	{"raw html openers", "", "<a", RootDocs},
	{"nested quotes", "", "> ", RootDocs},
	{"nested ordered lists", "", "1. ", RootDocs},
	{"wikilink brackets", "", "[a]", RootVault},
	{"vault math", "", "$a", RootVault},
	{"vault highlight", "", "a==", RootVault},
	{"vault comment", "", "a%%", RootVault},
}

func guardTrigger(pre, unit string, size int) []byte {
	reps := size / (len(unit) + len(pre)/2)
	return []byte(strings.Repeat(pre, reps/2) + strings.Repeat(unit, reps) + "\n")
}

// TestMarkupGuard_TriggersAreRefused: every measured trigger, at 256 KB,
// is over the guard. On origin/main each of these took from about 3 s to
// over a minute to render. Mutations: dropping any byte from
// markupDelimiters, the k*size product (k alone), or lineContainersOver
// turns its rows red.
func TestMarkupGuard_TriggersAreRefused(t *testing.T) {
	for _, tc := range guardTriggers {
		if src := guardTrigger(tc.pre, tc.unit, 256<<10); !markupTooComplex(src) {
			t.Errorf("%s: 256 KB of %q is not refused", tc.name, tc.unit)
		}
	}
}

// TestMarkupGuard_RenderAndScanAreBounded is the wall-clock check: each
// trigger at 256 KB renders and scans, in its slow root kind, in well
// under the bound, where origin/main took from about 3 s to over a minute
// per trigger. The guard itself runs in milliseconds, so 3 s is generous.
// Mutation: skipping the guard in renderHidden or scanDocFor turns this red.
func TestMarkupGuard_RenderAndScanAreBounded(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range guardTriggers {
		src := guardTrigger(tc.pre, tc.unit, 256<<10)
		start := time.Now()
		html, _, err := renderHidden(src, tc.kind, nil)
		if err != nil {
			t.Fatalf("%s: render: %v", tc.name, err)
		}
		if !strings.Contains(html, `data-forgectl-notice="plain-text"`) {
			t.Errorf("%s: render is not the plain-text fallback", tc.name)
		}
		p := filepath.Join(dir, "trigger.md")
		if err := os.WriteFile(p, src, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := scanDocFor(tc.kind, p, "trigger.md"); err != nil {
			t.Fatalf("%s: scan: %v", tc.name, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: render and scan took %v, want well under 3s", tc.name, d)
		}
	}
}

// TestMarkupGuard_WorkBoundIsExact: a paragraph whose delimiters times its
// length is exactly maxInlineWork passes, and one more delimiter does not.
// Mutation: `>` to `>=` in the bound check turns the first case red; a
// larger constant turns the second red.
func TestMarkupGuard_WorkBoundIsExact(t *testing.T) {
	// 4096 delimiters in a 32 KiB line: 4096 * 32768 = 1 << 27.
	const k, size = 4096, 32768
	line := func(delims int) []byte {
		b := []byte(strings.Repeat("*", delims) + strings.Repeat("a", size-delims))
		return append(b, '\n')
	}
	if k*size != maxInlineWork {
		t.Fatalf("fixture is %d, want maxInlineWork %d", k*size, maxInlineWork)
	}
	if markupTooComplex(line(k)) {
		t.Error("a paragraph exactly at maxInlineWork is refused")
	}
	if !markupTooComplex(line(k + 1)) {
		t.Error("a paragraph one delimiter over maxInlineWork is not refused")
	}
}

// TestMarkupGuard_ContainerWork: the container bound is the sum of each
// line's containers squared. One line of 4,096 quotes is exactly at it and
// passes, one more quote is over; the quotes, list markers and indentation
// under open lists all count; and a document whose lists are all closed
// again stops counting its indentation. Mutations: `>` to `>=` turns the
// first case red; dropping list markers, or the min(open, cols/2) term,
// turns the mixed or indented case red; never resetting open turns the
// closed-lists case red.
func TestMarkupGuard_ContainerWork(t *testing.T) {
	if markupTooComplex([]byte(strings.Repeat(">", 4096) + " x\n")) {
		t.Error("one line of 4096 quotes (exactly maxContainerWork) is refused")
	}
	if !markupTooComplex([]byte(strings.Repeat(">", 4097) + " x\n")) {
		t.Error("one line of 4097 quotes is not refused")
	}
	// List markers count as quotes do: one line opening 4,097 list items.
	if !markupTooComplex([]byte(strings.Repeat("- ", 4097) + "x\n")) {
		t.Error("one line of 4097 list markers is not refused")
	}
	// 128 lines of 400 mixed quote and list markers: 128 * 400^2 > 1 << 24.
	mixed := strings.Repeat(strings.Repeat("> - ", 200)+"x\n", 128)
	if !markupTooComplex([]byte(mixed)) {
		t.Error("128 lines of 400 mixed markers are not refused")
	}
	// 2,000 nested list items, then lines indented under all of them: each
	// line reaches 2,000 containers. On origin/main 1 MiB of this took 11 s.
	indented := strings.Repeat("- ", 2000) + "x\n" + strings.Repeat(strings.Repeat(" ", 4000)+"x\n", 8)
	if !markupTooComplex([]byte(indented)) {
		t.Error("lines indented under 2,000 open list items are not refused")
	}
	// The same indentation with every list closed by a paragraph at
	// column 0 after a blank line reaches no container: a deep code block.
	closed := strings.Repeat("- ", 2000) + "x\n\nclosed\n\n" + strings.Repeat(strings.Repeat(" ", 4000)+"x\n", 8)
	if markupTooComplex([]byte(closed)) {
		t.Error("indentation after every list is closed is refused")
	}
	// Markers need a following space: "-x" and "1.x" open nothing.
	if markupTooComplex([]byte(strings.Repeat("-x1.x", 1000) + "\n")) {
		t.Error("text that only looks like markers is refused")
	}
}

// pipelineParsers are every goldmark instance that parses a whole
// document or a fragment of one: render (docs and vault, with and without
// frontmatter), the index scan (docs and vault) and the heading-fragment
// parser.
func pipelineParsers() map[string]goldmark.Markdown {
	return map[string]goldmark.Markdown{
		"render docs":                  markdown,
		"render docs, no frontmatter":  markdownPlain,
		"render vault":                 markdownVault,
		"render vault, no frontmatter": markdownVaultPlain,
		"scan docs":                    linkMarkdown,
		"scan vault":                   linkMarkdownVault,
		"heading fragment":             fragmentMarkdown,
	}
}

// TestMarkupGuard_NeverFinerThanAPipeline: for block shapes whose
// segmentation differs between pipelines (tables, a link reference
// definition over a delimiter row, frontmatter, $$ and %% blocks, tables in
// lists and quotes), the guard's inline work is never less than the work
// any pipeline's own block structure implies, because its blocks are never
// finer. Mutation: giving markupGuardParser the GFM table transformer
// counts a table cell by cell and turns the scan-docs rows red, with or
// without goldmark's default paragraph transformers alongside it.
func TestMarkupGuard_NeverFinerThanAPipeline(t *testing.T) {
	row := "|" + strings.Repeat("_a a* ", 4) + "|\n"
	rows := strings.Repeat(row, 6)
	inputs := map[string]string{
		"table":                      "h\n|-|\n" + rows,
		"table with header":          "| x | y |\n|---|---|\n" + strings.Repeat("| `a` *b* | [c](d) _e_ |\n", 6),
		"ref def over delimiter row": "[a]: /u\n|-|\n" + rows,
		"frontmatter then table":     "---\ntitle: t\n---\nh\n|-|\n" + rows,
		"math and comment blocks":    "$$\nx_1\n$$\n%%\nc *d*\n%%\nh\n|-|\n" + rows,
		"table in a list":            "- h\n  |-|\n" + strings.ReplaceAll(rows, "|_", "  |_"),
		"table in a quote":           "> h\n> |-|\n" + strings.ReplaceAll(rows, "|_", "> |_"),
		"paragraphs":                 strings.Repeat("a *b* [c](d) _e_\n\n", 4),
	}
	const unlimited = int(^uint(0) >> 1)
	for name, src := range inputs {
		b := []byte(src)
		markupGuardMu.Lock()
		guard := inlineWork(markupGuardParser.Parse(text.NewReader(b)), b, unlimited)
		markupGuardMu.Unlock()
		for pname, md := range pipelineParsers() {
			doc := md.Parser().Parse(text.NewReader(b), parser.WithContext(newParseContext()))
			if got := inlineWork(doc, b, unlimited); got > guard {
				t.Errorf("%s under %s: pipeline work %d > guard work %d, so the guard's blocks are finer", name, pname, got, guard)
			}
		}
	}
}

// reviewTableRepro is a GFM-table-shaped document of n rows of unmatched
// emphasis delimiters, after prefix. Two pipelines inline-parse its rows as
// one paragraph: the docs index scan, which has no table transformer, and,
// with prefix "[a]: /u\n", render and the vault scan, where the stripped
// reference definition leaves a header-less delimiter row that is no
// table. On a guard that counted cells, 1,000 rows (123 KB) took 7.1 s to
// scan and 8.9 s to render, the latter under renderMu, and 3,000 took over
// 60 s.
func reviewTableRepro(prefix string, n int) []byte {
	return []byte(prefix + "h\n|-|\n" + strings.Repeat("|"+strings.Repeat("_a a* ", 20)+"|\n", n))
}

// TestMarkupGuard_TableReprosAreBounded: both review repros are refused and
// render and scan, in both root kinds, well inside a generous wall-clock
// bound. Mutation: giving markupGuardParser the GFM table transformer turns
// this red (and slow).
func TestMarkupGuard_TableReprosAreBounded(t *testing.T) {
	dir := t.TempDir()
	for _, prefix := range []string{"", "[a]: /u\n"} {
		src := reviewTableRepro(prefix, 3000)
		if !markupTooComplex(src) {
			t.Errorf("prefix %q: a %d-byte table repro is not refused", prefix, len(src))
		}
		p := filepath.Join(dir, "table.md")
		if err := os.WriteFile(p, src, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []RootKind{RootDocs, RootVault} {
			start := time.Now()
			if _, _, err := renderHidden(src, kind, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := scanDocFor(kind, p, "table.md"); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("prefix %q, kind %v: render and scan took %v, want well under 3s", prefix, kind, d)
			}
		}
	}
}

// TestMarkupGuard_WrittenDocsPass: every markdown file in this repository,
// and a long document of ordinary dense prose, is under the guard.
// Mutation: a much smaller maxInlineWork turns this red.
func TestMarkupGuard_WrittenDocsPass(t *testing.T) {
	var prose strings.Builder
	for prose.Len() < maxScanBytes-1024 {
		prose.WriteString("Run `forgectl docs serve` with **care**, see [the guide](guide.md) and <https://example.com>; the _flag_ is ~~old~~ new.\n\n")
	}
	if markupTooComplex([]byte(prose.String())) {
		t.Error("a 1 MiB document of ordinary prose is refused")
	}
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	repo := root.FS()
	n := 0
	err = fs.WalkDir(repo, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".claude" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := fs.ReadFile(repo, p)
		if err != nil {
			return err
		}
		n++
		if len(b) <= maxScanBytes && markupTooComplex(b) {
			t.Errorf("%s is refused by the markup guard", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Fatalf("walked %d markdown files, want the repository's docs", n)
	}
}

// TestRenderDocFor_GuardedDocIsPlainText: a refused document renders as
// its escaped source under the notice, with no outline, and its words are
// still counted. Mutation: dropping html.EscapeString in plainTextDoc
// turns the escape check red.
func TestRenderDocFor_GuardedDocIsPlainText(t *testing.T) {
	src := "# Title\n\n## Section\n\n<script>alert(1)</script>\n\n" + string(guardTrigger("", "[x](", 256<<10))
	r, err := RenderDocFor(RootDocs, []byte(src), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.HTML, `data-forgectl-notice="plain-text"`) || !strings.Contains(r.HTML, `<pre class="doc-plain-text">`) {
		t.Fatalf("not the plain-text fallback: %.200s", r.HTML)
	}
	if strings.Contains(r.HTML, "<script>") || !strings.Contains(r.HTML, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the source is not HTML-escaped")
	}
	if !strings.Contains(r.HTML, "# Title\n\n## Section") {
		t.Error("the source is not shown as written")
	}
	if len(r.Outline) != 0 {
		t.Errorf("outline = %+v, want none for a plain-text page", r.Outline)
	}
	if r.Words == 0 {
		t.Error("words not counted")
	}
}

// TestScanDoc_GuardedDocIsTitleOnly: a refused document is indexed by its
// H1 title only, as an over-cap one is, with no headings or links, in
// both root kinds; a refused title line falls back to the filename.
// Mutation: dropping the guard from scanDocFor turns the links check red
// (and the scan slow).
func TestScanDoc_GuardedDocIsTitleOnly(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range []RootKind{RootDocs, RootVault} {
		p := filepath.Join(dir, "g.md")
		body := "# Guarded\n\n[a](a.md) [[b]]\n\n" + string(guardTrigger("", "a*", 256<<10))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		meta, err := scanDocFor(kind, p, "g.md")
		if err != nil {
			t.Fatal(err)
		}
		if meta.Title != "Guarded" || len(meta.Links) != 0 || len(meta.Headings) != 0 {
			t.Errorf("kind %v: meta = title %q, %d links, %d headings; want title only", kind, meta.Title, len(meta.Links), len(meta.Headings))
		}
	}
	// firstH1 reads one line of any length: a vault title line that is
	// itself a trigger is not parsed, and the filename stands in.
	start := time.Now()
	if got := vaultLineTitle(string(guardTrigger("", "[x](", 256<<10))); got != "" {
		t.Errorf("vaultLineTitle of a trigger line = %.40q, want \"\"", got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("vaultLineTitle took %v, want well under 3s", d)
	}
}
