package docs

// Tests for the markup guard (markupguard.go, forgectl#628, forgectl#596).

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/perftest"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// refused reports whether the guard refuses src for the docs render
// pipeline, or for the vault one when vault is set. The container and
// work bounds these tests pin do not depend on the pipeline.
func refused(src []byte, vault bool) bool {
	md := markdownPlain
	if vault {
		md = markdownVaultPlain
	}
	tooComplex, err := markupTooComplex(md, src)
	if err != nil {
		panic(err)
	}
	return tooComplex
}

// refusedByAny reports whether any pipeline's guard refuses src.
func refusedByAny(src []byte) bool {
	for md := range guardTwins {
		if tooComplex, err := markupTooComplex(md, src); err != nil || tooComplex {
			return true
		}
	}
	return false
}

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
		if src := guardTrigger(tc.pre, tc.unit, 256<<10); !refused(src, tc.kind == RootVault) {
			t.Errorf("%s: 256 KB of %q is not refused", tc.name, tc.unit)
		}
	}
}

// TestMarkupGuard_RenderAndScanAreBounded: each trigger at 256 KB renders
// as the plain-text fallback and renders and scans, in its slow root kind, in
// time linear in its size, where origin/main took from about 3 s to over a
// minute per trigger. The check is a ratio (perftest.Linear, forgectl#879,
// #919) against the same trigger an eighth the size, in process CPU time; a
// 3 s wall-clock bound failed under host load.
//
// Mutation: skipping the guard in renderHidden or scanDocFrom turns this red.
func TestMarkupGuard_RenderAndScanAreBounded(t *testing.T) {
	const size, k = 256 << 10, 8
	dir := t.TempDir()
	for _, tc := range guardTriggers {
		t.Run(tc.name, func(t *testing.T) {
			renderAndScan := func(src []byte, file string) func() {
				p := filepath.Join(dir, file)
				if err := os.WriteFile(p, src, 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {
					html, _, err := renderHidden(src, tc.kind, nil)
					if err != nil {
						t.Fatalf("render: %v", err)
					}
					if !strings.Contains(html, `data-forgectl-notice="plain-text"`) {
						t.Fatalf("render of %d bytes is not the plain-text fallback", len(src))
					}
					if _, err := scanDocFor(tc.kind, p, file); err != nil {
						t.Fatalf("scan: %v", err)
					}
				}
			}
			small, large := perftest.Amortize(
				renderAndScan(guardTrigger(tc.pre, tc.unit, size/k), "small.md"),
				renderAndScan(guardTrigger(tc.pre, tc.unit, size), "large.md"))
			perftest.Linear(t, "render and scan", k, small, large)
		})
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
	if refused(line(k), false) {
		t.Error("a paragraph exactly at maxInlineWork is refused")
	}
	if !refused(line(k+1), false) {
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
	if refused([]byte(strings.Repeat(">", 4096)+" x\n"), false) {
		t.Error("one line of 4096 quotes (exactly maxContainerWork) is refused")
	}
	if !refused([]byte(strings.Repeat(">", 4097)+" x\n"), false) {
		t.Error("one line of 4097 quotes is not refused")
	}
	// List markers count as quotes do: one line opening 4,097 list items.
	if !refused([]byte(strings.Repeat("- ", 4097)+"x\n"), false) {
		t.Error("one line of 4097 list markers is not refused")
	}
	// 128 lines of 400 mixed quote and list markers: 128 * 400^2 > 1 << 24.
	mixed := strings.Repeat(strings.Repeat("> - ", 200)+"x\n", 128)
	if !refused([]byte(mixed), false) {
		t.Error("128 lines of 400 mixed markers are not refused")
	}
	// 2,000 nested list items, then lines indented under all of them: each
	// line reaches 2,000 containers. On origin/main 1 MiB of this took 11 s.
	indented := strings.Repeat("- ", 2000) + "x\n" + strings.Repeat(strings.Repeat(" ", 4000)+"x\n", 8)
	if !refused([]byte(indented), false) {
		t.Error("lines indented under 2,000 open list items are not refused")
	}
	// The same indentation with every list closed by a paragraph at
	// column 0 after a blank line reaches no container: a deep code block.
	closed := strings.Repeat("- ", 2000) + "x\n\nclosed\n\n" + strings.Repeat(strings.Repeat(" ", 4000)+"x\n", 8)
	if refused([]byte(closed), false) {
		t.Error("indentation after every list is closed is refused")
	}
	// Markers need a following space: "-x" and "1.x" open nothing.
	if refused([]byte(strings.Repeat("-x1.x", 1000)+"\n"), false) {
		t.Error("text that only looks like markers is refused")
	}
}

// TestMarkupGuard_TwinsAreBlockOnly: every twin's parse leaves inline
// markup as plain text, so the guard never runs the superlinear inline
// parsers it exists to avoid. Mutation: letting blockOnlyParser pass on
// inline parser options turns this red (strikethrough, math, linkify, the
// vault parsers).
func TestMarkupGuard_TwinsAreBlockOnly(t *testing.T) {
	src := []byte("para *a* [b](c) ~~d~~ `e` $f$ $$g$$ [[h]] ==i== %%j%% #tag https://x.io <b>k</b>\n")
	for md, twin := range guardTwins {
		doc := twin.Parser().Parse(text.NewReader(src), parser.WithContext(newParseContext()))
		_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if entering && n.Type() == ast.TypeInline && n.Kind() != ast.KindText {
				t.Errorf("twin of %p produced an inline %s node", md, n.Kind())
			}
			return ast.WalkContinue, nil
		})
	}
}

// guardShapes are block shapes on which a pipeline's own block parsers or
// paragraph transformers segment a document differently from goldmark's
// defaults: tables (the docs scan has no table transformer), a link
// reference definition over a delimiter row, frontmatter hiding a fence or
// an HTML opener, $$ and %% blocks over a fence or an HTML opener, and
// tables in lists and quotes. Each is followed by rows of unmatched
// emphasis delimiters, the costly part.
func guardShapes(rows string) map[string]string {
	return map[string]string{
		"table":                      "h\n|-|\n" + rows,
		"table with header":          "| x | y |\n|---|---|\n" + rows,
		"ref def over delimiter row": "[a]: /u\n|-|\n" + rows,
		"frontmatter hiding a fence": "---\na: |\n  ```\n---\n" + rows,
		"frontmatter hiding <pre>":   "---\na: |\n  <pre>\n---\n" + rows,
		"math over <script>":         "$$\n<script>\n$$\n" + rows,
		"math over <pre>":            "$$\n<pre>\n$$\n" + rows,
		"math over a fence":          "$$\n```\n$$\n" + rows,
		"comment over <script>":      "%%\n<script>\n%%\n" + rows,
		"comment over a fence":       "%%\n```\n%%\n" + rows,
		"math and comment blocks":    "$$\nx_1\n$$\n%%\nc *d*\n%%\n" + rows,
		"table in a list":            "- h\n  |-|\n" + strings.ReplaceAll(rows, "|_", "  |_"),
		"table in a quote":           "> h\n> |-|\n" + strings.ReplaceAll(rows, "|_", "> |_"),
	}
}

func guardRows(n int) string {
	return strings.Repeat("|"+strings.Repeat("_a a* ", 20)+"|\n", n)
}

// TestMarkupGuard_NeverFinerThanAPipeline: on every adversarial shape, the
// work the guard measures for a pipeline is at least the work that
// pipeline's own full parse, inline pass included, gives the same bytes.
// Mutation: reverting the guard to one parser of goldmark's default block
// parsers (no pipeline's own) turns the frontmatter rows red for the
// frontmatter render pipelines; giving it the GFM table transformer on
// top turns the table rows red for the docs scan.
func TestMarkupGuard_NeverFinerThanAPipeline(t *testing.T) {
	const unlimited = int(^uint(0) >> 1)
	for name, src := range guardShapes(guardRows(6)) {
		b := []byte(src)
		for md, twin := range guardTwins {
			pipe := inlineWork(md.Parser().Parse(text.NewReader(b), parser.WithContext(newParseContext())), b, unlimited)
			guard := inlineWork(twin.Parser().Parse(text.NewReader(b), parser.WithContext(newParseContext())), b, unlimited)
			if pipe > guard {
				t.Errorf("%s: pipeline work %d > guard work %d; the guard misses blocks this pipeline inline-parses", name, pipe, guard)
			}
		}
	}
}

// escapedPipeTables are GFM tables of cells holding "\|" after a backtick,
// which goldmark's tableASTTransformer handles in quadratic time
// (escapedPipeWork). The twin runs no AST transformer, so only
// escapedPipeWork's charge sees them. On the guard without that charge,
// the one-column table (420 KB) took 11.4 s to render and the eight-column
// one (294 KB) 9.8 s to render and 9.3 s to scan. On a guard that took the
// positions from the twin's table cells, the orphan header (1 MB) passed
// and then took 11.6 s to render and 12.6 s to scan in a vault.
func escapedPipeTables() map[string]string { return escapedPipeTablesOver(1) }

// escapedPipeTablesOver is escapedPipeTables with every repeat count divided
// by div, for the small side of a timing ratio.
func escapedPipeTablesOver(div int) map[string]string {
	eight := "|" + strings.Repeat("`\\|`|", 8) + "\n"
	return map[string]string{
		"escaped pipes, 1 column":  "| a |\n|-|\n" + strings.Repeat("|`\\|`|\n", 52500/div),
		"escaped pipes, 8 columns": "| a | b | c | d | e | f | g | h |\n|-|-|-|-|-|-|-|-|\n" + strings.Repeat(eight, 6000/div),
		// A header whose cell count does not match its delimiter row is no
		// table, but goldmark has already recorded its "\|" positions, and
		// the small real table after it pays for every one of them.
		"escaped pipes, orphan header": "`" + strings.Repeat("\\|", 490000/div) + " | b\n|-|\n\n" + "|a|\n|-|\n" + strings.Repeat("|`\\|`|\n", 8150/div),
	}
}

// TestMarkupGuard_EscapedPipeCharge: the escaped-pipe tables are refused
// for the render pipelines, which carry the table transformers, and a
// table of a few such cells is not. Mutations: dropping escapedPipeWork
// from markupTooComplex turns this red (and ReprosAreBounded slow), and so
// does counting positions from the twin's table cells, for the orphan
// header.
func TestMarkupGuard_EscapedPipeCharge(t *testing.T) {
	for name, src := range escapedPipeTables() {
		for _, vault := range []bool{false, true} {
			if !refused([]byte(src), vault) {
				t.Errorf("%s (vault %v) is not refused", name, vault)
			}
		}
	}
	small := "| a |\n|-|\n" + strings.Repeat("|`\\|`|\n", 20)
	if refused([]byte(small), false) {
		t.Error("a 20-row escaped-pipe table is refused")
	}
}

// TestMarkupGuard_UnknownPipelineIsAnError: a pipeline with no twin is an
// error on any input, the smallest included, so a miswired call site fails
// in its first test. Mutation: looking the twin up after the fast path
// turns this red.
func TestMarkupGuard_UnknownPipelineIsAnError(t *testing.T) {
	if _, err := markupTooComplex(fragmentMarkdown, []byte("x\n")); !errors.Is(err, errNoGuardTwin) {
		t.Errorf("err = %v, want errNoGuardTwin", err)
	}
}

// mixedOption adds an inline parser and sets a parser option map entry, so
// blockOnlyParser can neither pass it on nor drop it.
type mixedOption struct{}

func (mixedOption) SetParserOption(c *parser.Config) {
	c.InlineParsers = append(c.InlineParsers, util.Prioritized(parser.NewCodeSpanParser(), 100))
	c.Options["forgectl-test"] = true
}

// TestBlockOnlyParser_RefusesMixedOptions: an option that mixes an inline
// parser with any other Config field panics, and a purely inline one is
// dropped. Mutation: dropping the reflect.DeepEqual check turns the panic
// case red.
func TestBlockOnlyParser_RefusesMixedOptions(t *testing.T) {
	p := blockOnlyParser{parser.NewParser()}
	p.AddOptions(parser.WithInlineParsers(util.Prioritized(parser.NewCodeSpanParser(), 100)))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a mixed inline-and-options parser option did not panic")
			}
		}()
		p.AddOptions(mixedOption{})
	}()
}

// TestMarkupGuard_ReprosAreBounded: each shape at 3,000 rows, and each
// escaped-pipe table, renders and scans in both root kinds in time linear in
// its size. On earlier guards that modelled the block structure, the table
// shapes took 7 to 9 s at 1,000 rows, and "math over <script>" and
// "frontmatter hiding a fence" 53 s and 57 s at 3,000, under renderMu. The
// check is a ratio (perftest.Linear, forgectl#879): each input against the
// same shape an eighth its size, in process CPU time; a wall-clock bound
// flaked at 3.4 to 9 s under load. A shape the guard refuses at full size
// but not at an eighth renders in full only on the small side, so its ratio
// is below 1, which passes.
//
// Mutations: making markupTooComplex return false once it has found the twin
// (no guard) turns the refused shapes red; dropping escapedPipeWork from it
// turns the escaped-pipe tables red.
func TestMarkupGuard_ReprosAreBounded(t *testing.T) {
	const rows, k = 3000, 8
	shapes := func(rows, div int) map[string]string {
		m := guardShapes(guardRows(rows))
		for name, src := range escapedPipeTablesOver(div) {
			m[name] = src
		}
		return m
	}
	small, large := shapes(rows/k, k), shapes(rows, 1)
	renderAndScan := func(t *testing.T, src string, kind RootKind) func() {
		b := []byte(src)
		return func() {
			if _, _, err := renderHidden(b, kind, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := scanDocFrom(kind, bytes.NewReader(b), "shape.md"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, src := range large {
		for _, kind := range []RootKind{RootDocs, RootVault} {
			t.Run(fmt.Sprintf("%s/kind %v", name, kind), func(t *testing.T) {
				smallRun, largeRun := perftest.Amortize(renderAndScan(t, small[name], kind), renderAndScan(t, src, kind))
				perftest.Linear(t, "render and scan", k, smallRun, largeRun)
			})
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
	if refusedByAny([]byte(prose.String())) {
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
		if len(b) <= maxScanBytes && refusedByAny(b) {
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
// Mutation: dropping the guard from scanDocFrom turns the links check red
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
	// itself a trigger is not parsed, and the filename stands in, in time
	// linear in the line (perftest.Linear, #919; it was a 3 s wall-clock
	// bound). Mutation: dropping the markupTooComplex check from
	// vaultLineTitle turns this red: the line then parses as a title.
	const k = 8
	title := func(size int) func() {
		line := string(guardTrigger("", "[x](", size))
		return func() {
			if got := vaultLineTitle(line); got != "" {
				t.Fatalf("vaultLineTitle of a %d-byte trigger line = %.40q, want \"\"", len(line), got)
			}
		}
	}
	smallRun, largeRun := perftest.Amortize(title(256<<10/k), title(256<<10))
	perftest.Linear(t, "vaultLineTitle", k, smallRun, largeRun)
}
