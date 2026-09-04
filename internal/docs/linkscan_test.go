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

import (
	"path/filepath"
	"testing"
)

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
		{Path: "notes/orphan", Fragment: "", Embed: false, Form: FormPlain},
		{Path: "notes/beta", Fragment: "", Embed: false, Form: FormAlias},
		{Path: "notes/anchors", Fragment: "", Embed: true, Form: FormEmbed},
		{Path: "notes/anchors", Fragment: "Some Heading", Embed: false, Form: FormHeading},
		{Path: "notes/anchors", Fragment: "^blk-1", Embed: false, Form: FormBlock},
		{Path: "notes/anchors", Fragment: "Some Heading#Sub", Embed: false, Form: FormHeading},
		{Path: "deep/Alpha", Fragment: "", Embed: false, Form: FormPlain},
		{Path: "../repo/index", Fragment: "", Embed: false, Form: FormPlain},
	}
	if len(meta.Links) != len(want) {
		t.Fatalf("Links has %d entries, want %d: %+v", len(meta.Links), len(want), meta.Links)
	}
	for i, w := range want {
		got := meta.Links[i]
		if got.Path != w.Path || got.Fragment != w.Fragment || got.Embed != w.Embed || got.Form != w.Form {
			t.Errorf("Links[%d] = {Path:%q Fragment:%q Embed:%v Form:%v}, want {Path:%q Fragment:%q Embed:%v Form:%v}",
				i, got.Path, got.Fragment, got.Embed, got.Form, w.Path, w.Fragment, w.Embed, w.Form)
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
