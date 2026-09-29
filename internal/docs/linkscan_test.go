package docs

// Test plan for linkscan.go (scanDoc)
//
// scanDoc (Classification: one-pass parser — title/frontmatter/AST scan)
//   [x] Happy: title extracted from the first "# " heading
//   [x] Happy: title falls back to the filename when no heading is present
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
