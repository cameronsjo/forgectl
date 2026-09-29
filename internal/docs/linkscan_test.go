package docs

// Test plan for linkscan.go (scanDoc)
//
// scanDoc (Classification: one-pass parser — title/frontmatter/AST scan)
//   [x] Happy: title extracted from the first "# " heading
//   [x] Happy: title falls back to the filename when no heading is present
//   [x] Unhappy: a "# " line in a fence, $$ block, indented code block or
//       frontmatter comment is no docs-root title
//   [x] Happy: frontmatter aliases as a YAML list
//   [x] Happy: frontmatter aliases as a bare YAML scalar
//   [x] Happy: headings, each carrying goldmark's auto-ID slug
//   [x] Happy: Obsidian ^block-id markers
//   [x] Happy: outbound links classified by form (plain, alias, embed,
//       heading, block, nested-heading, relative markdown link)
//   [x] Happy: Obsidian ^block-id markers inside code blocks are ignored
//   [x] Unhappy: an unclosed fence masks the rest of the file
//   [x] Unhappy: a document over maxScanBytes is indexed by title only
//   [x] Edge: a document of exactly maxScanBytes is fully scanned
//   [x] Unhappy: a ^id inside a $$ block is no block id in a docs root; one
//       after the block still is
//   [x] Happy: aliases and trust fields both come out of one frontmatter

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// padToSize appends "x\n" lines to head until the result is exactly n bytes.
func padToSize(head string, n int) string {
	var b strings.Builder
	b.WriteString(head)
	for b.Len() < n-1 {
		b.WriteString("x\n")
	}
	if b.Len() < n {
		b.WriteString("\n")
	}
	return b.String()[:n]
}

func fixtureAbs(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "links", rel))
	if err != nil {
		t.Fatalf("fixtureAbs(%q): %v", rel, err)
	}
	return abs
}

func TestScanDoc_TitleFromHeading(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "vault/notes/orphan.md"), "notes/orphan.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if meta.Title != "Orphan" {
		t.Errorf("Title = %q, want %q", meta.Title, "Orphan")
	}
}

func TestScanDoc_TitleFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-heading.md")
	writeFile(t, path, "just a paragraph, no heading\n")

	meta, err := scanDoc(path, "no-heading.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if meta.Title != "no-heading" {
		t.Errorf("Title = %q, want %q", meta.Title, "no-heading")
	}
}

// TestScanDoc_TitleFromParse: a docs-root title is a level-1 heading the
// page renders, so a "# " line inside a code fence, a $$ block, an indented
// code block or a YAML frontmatter comment is never the title. A real
// heading's text is still taken verbatim, as firstH1 took it.
func TestScanDoc_TitleFromParse(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"fence", "```sh\n# not a title\n```\n\n# Real\n", "Real"},
		{"tilde fence", "~~~\n# not a title\n~~~\n", "t"},
		{"math block", "$$\n# not a title\n$$\n\n# Real\n", "Real"},
		{"indented code", "para\n\n    # not a title\n\n# Real\n", "Real"},
		{"frontmatter comment", "---\n# not a title\nkey: v\n---\n\n# Real\n", "Real"},
		{"verbatim text", "  # A *b* `c` \\_d\n", "A *b* `c` \\_d"},
		{"setext is no title", "Setext\n===\n", "t"},
	}
	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "t.md")
		writeFile(t, path, tc.src)
		meta, err := scanDoc(path, "t.md")
		if err != nil {
			t.Fatalf("%s: scanDoc: %v", tc.name, err)
		}
		if meta.Title != tc.want {
			t.Errorf("%s: Title = %q, want %q", tc.name, meta.Title, tc.want)
		}
	}
}

func TestScanDoc_AliasesList(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "vault/notes/Alpha.md"), "notes/Alpha.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	want := []string{"Alpha One", "First Alpha"}
	if len(meta.Aliases) != len(want) {
		t.Fatalf("Aliases = %v, want %v", meta.Aliases, want)
	}
	for i, w := range want {
		if meta.Aliases[i] != w {
			t.Errorf("Aliases[%d] = %q, want %q", i, meta.Aliases[i], w)
		}
	}
}

func TestScanDoc_AliasesScalar(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "vault/notes/beta.md"), "notes/beta.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if len(meta.Aliases) != 1 || meta.Aliases[0] != "Beta Note" {
		t.Errorf("Aliases = %v, want [Beta Note]", meta.Aliases)
	}
}

func TestScanDoc_HeadingsWithSlug(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "vault/notes/anchors.md"), "notes/anchors.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	want := []Heading{
		{Text: "Anchors", Slug: "anchors"},
		{Text: "Some Heading", Slug: "some-heading"},
		{Text: "Sub", Slug: "sub"},
	}
	if len(meta.Headings) != len(want) {
		t.Fatalf("Headings = %+v, want %+v", meta.Headings, want)
	}
	for i, w := range want {
		if meta.Headings[i] != w {
			t.Errorf("Headings[%d] = %+v, want %+v", i, meta.Headings[i], w)
		}
	}
}

func TestScanDoc_BlockIDs(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "vault/notes/anchors.md"), "notes/anchors.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if len(meta.BlockIDs) != 1 || meta.BlockIDs[0] != "blk-1" {
		t.Errorf("BlockIDs = %v, want [blk-1]", meta.BlockIDs)
	}
}

func TestScanDoc_WikilinkFormsInVaultIndex(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "vault/index.md"), "index.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	want := []LinkRef{
		{Path: "notes/orphan", Fragment: "", Form: FormPlain},
		{Path: "notes/beta", Fragment: "", Form: FormAlias},
		{Path: "notes/anchors", Fragment: "", Form: FormEmbed},
		{Path: "notes/anchors", Fragment: "Some Heading", Form: FormHeading},
		{Path: "notes/anchors", Fragment: "^blk-1", Form: FormBlock},
		{Path: "notes/anchors", Fragment: "Some Heading#Sub", Form: FormHeading},
		{Path: "deep/Alpha", Fragment: "", Form: FormPlain},
		{Path: "../repo/index", Fragment: "", Form: FormPlain},
	}
	if len(meta.Links) != len(want) {
		t.Fatalf("Links has %d entries, want %d: %+v", len(meta.Links), len(want), meta.Links)
	}
	for i, w := range want {
		got := meta.Links[i]
		if got.Path != w.Path || got.Fragment != w.Fragment || got.Form != w.Form {
			t.Errorf("Links[%d] = {Path:%q Fragment:%q Form:%v}, want {Path:%q Fragment:%q Form:%v}",
				i, got.Path, got.Fragment, got.Form, w.Path, w.Fragment, w.Form)
		}
	}
}

func TestScanDoc_RelativeMarkdownLinksInRepoIndex(t *testing.T) {
	meta, err := scanDoc(fixtureAbs(t, "repo/index.md"), "index.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	want := []LinkRef{
		{Path: "guide.md", Fragment: "getting-started", Form: FormRelPath},
		{Path: "../../etc/passwd", Fragment: "", Form: FormRelPath},
	}
	if len(meta.Links) != len(want) {
		t.Fatalf("Links has %d entries, want %d: %+v", len(meta.Links), len(want), meta.Links)
	}
	for i, w := range want {
		got := meta.Links[i]
		if got.Path != w.Path || got.Fragment != w.Fragment || got.Form != w.Form {
			t.Errorf("Links[%d] = {Path:%q Fragment:%q Form:%v}, want {Path:%q Fragment:%q Form:%v}",
				i, got.Path, got.Fragment, got.Form, w.Path, w.Fragment, w.Form)
		}
	}
}

func TestScanDoc_LinkLineCountsFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []int
	}{
		{"none", "[a](a.md)\n\ntext [b](b.md) [[c]]\n", []int{1, 3, 3}},
		{"yaml", "---\nk: v\n---\n[a](a.md)\n\ntext [b](b.md) [[c]]\n", []int{4, 6, 6}},
		{"toml", "+++\nk = 1\n+++\n\n[a](a.md)\n[b](b.md)\n[[c]]\n", []int{5, 6, 7}},
		{"crlf", "---\r\nk: v\r\n---\r\n[a](a.md)\r\n\r\n[b](b.md)\r\n[[c]]\r\n", []int{4, 6, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "p.md")
			if err := os.WriteFile(p, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			meta, err := scanDoc(p, "p.md")
			if err != nil {
				t.Fatalf("scanDoc: %v", err)
			}
			got := make([]int, 0, len(meta.Links))
			for _, l := range meta.Links {
				got = append(got, l.Line)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("link lines = %v, want %v (links %+v)", got, tc.want, meta.Links)
			}
		})
	}
}

func TestScanDoc_PercentEncodedDestinationIsDecoded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "page.md")
	if err := os.WriteFile(path, []byte("[x](My%20Doc.md#a%20heading)\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	meta, err := scanDoc(path, "page.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if len(meta.Links) != 1 {
		t.Fatalf("Links = %+v, want exactly one", meta.Links)
	}
	if got := meta.Links[0]; got.Path != "My Doc.md" || got.Fragment != "a heading" {
		t.Errorf("Links[0] = {Path:%q Fragment:%q}, want {Path:%q Fragment:%q}", got.Path, got.Fragment, "My Doc.md", "a heading")
	}
}

const blockIDsInCodeBody = "# T\n\nreal para ^keep\n\n```\ncode x^2\nfence ^fenced-id\n```\n\n~~~python ^info-id\ny ^tilde-id\n~~~\n\n    indented ^indented-id\n\n> quote\n> ```\n> q ^quoted-fence\n> ```\n\n- item\n\n      ^listcode\n\nafter ^after\n"

func TestScanDoc_BlockIDsIgnoreCode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(p, []byte(blockIDsInCodeBody), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDoc(p, "p.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if want := []string{"after", "keep"}; !reflect.DeepEqual(meta.BlockIDs, want) {
		t.Errorf("BlockIDs = %v, want %v", meta.BlockIDs, want)
	}
}

func TestScanDoc_UnclosedFenceMasksToEOF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(p, []byte("```\n^ghost"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDoc(p, "p.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if len(meta.BlockIDs) != 0 {
		t.Errorf("BlockIDs = %v, want none", meta.BlockIDs)
	}
}

func TestScanDoc_OverCap_TitleFromPrefixNoLinkMeta(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.md")
	content := padToSize("# Big\n\n[[target]] ^blk\n", maxScanBytes+1)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDoc(p, "big.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if meta.Title != "Big" {
		t.Errorf("Title = %q, want Big", meta.Title)
	}
	if len(meta.Links) != 0 || len(meta.Headings) != 0 || len(meta.BlockIDs) != 0 {
		t.Errorf("over-cap doc carries scan metadata: %+v", meta)
	}
}

// TestScanDoc_OverCap_TitleSkipsFrontmatterComments: an over-cap document's
// line-scan title comes from the body, not from a YAML "# comment" line in
// its frontmatter.
func TestScanDoc_OverCap_TitleSkipsFrontmatterComments(t *testing.T) {
	for name, tc := range map[string]struct{ head, want string }{
		"body heading wins": {"---\n# yaml comment\ntitle: t\n---\n# Real\n", "Real"},
		"no body heading":   {"---\n# yaml comment\n---\n\n", "big"},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "big.md")
			if err := os.WriteFile(p, []byte(padToSize(tc.head, maxScanBytes+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			meta, err := scanDoc(p, "big.md")
			if err != nil {
				t.Fatalf("scanDoc: %v", err)
			}
			if meta.Title != tc.want {
				t.Errorf("Title = %q, want %q", meta.Title, tc.want)
			}
		})
	}
}

func TestScanDoc_AtCap_FullyScanned(t *testing.T) {
	p := filepath.Join(t.TempDir(), "edge.md")
	content := padToSize("# Edge\n\n[[target]]\n", maxScanBytes)
	if len(content) != maxScanBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(content), maxScanBytes)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDoc(p, "edge.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if len(meta.Links) != 1 {
		t.Errorf("Links = %+v, want 1 (a doc exactly at the cap is scanned)", meta.Links)
	}
}

func TestScanDoc_BlockIDsIgnoreMathBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(p, []byte("$$\nx ^blk\n$$\n\nafter ^keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDoc(p, "p.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if want := []string{"keep"}; !reflect.DeepEqual(meta.BlockIDs, want) {
		t.Errorf("BlockIDs = %v, want %v", meta.BlockIDs, want)
	}
}

func TestScanDoc_FrontmatterAliasesAndTrust(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.md")
	src := "---\naliases:\n  - one\n  - two\nstatus: deprecated\nstale_after: 2026-09-23T00:00:00Z\n---\n# T\n"
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDoc(p, "p.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if want := []string{"one", "two"}; !reflect.DeepEqual(meta.Aliases, want) {
		t.Errorf("Aliases = %v, want %v", meta.Aliases, want)
	}
	if meta.Status != "deprecated" || meta.StaleAfter != "2026-09-23T00:00:00Z" {
		t.Errorf("trust = (%q, %q), want (deprecated, 2026-09-23T00:00:00Z)", meta.Status, meta.StaleAfter)
	}
}

// A link or wikilink written inside an image's alt text is shown only as alt
// text, so it is not indexed (forgectl#596). A link beside the image, and a
// link nested in ordinary link text, still are: they render as links.
// Mutation: drop either hasImageAncestor check in scanBodyFor and both root
// kinds go red (the docs-root scan indexes wikilinks too).
func TestScanBody_LinksInsideImageAltAreNotIndexed(t *testing.T) {
	src := "![alt [in](in.md) [[wiki]] end](pic.png) [out](out.md)\n\n[[l](l.md)](m.md)\n"
	for _, kind := range []RootKind{RootDocs, RootVault} {
		scan, err := scanBodyFor(kind, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, l := range scan.links {
			got = append(got, l.Raw)
		}
		want := []string{"out.md", "l.md"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("kind %v: links = %q, want %q", kind, got, want)
		}
	}
}
