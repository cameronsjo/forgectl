package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attachmentVerdict is what one vault wikilink to a non-note file must come
// to, in the reader and in docs check alike (forgectl#709).
type attachmentVerdict int

const (
	attResolved attachmentVerdict = iota
	attMissing
	attAmbiguous
	attOutside
)

// attachmentVault builds a vault at base/vault whose linking note is
// sub/n.md, with one link per line starting at line 3. A file sits outside
// the vault at base/outside.png, reachable only through the symlinks when
// symlinks are available (symlinks reports whether they are).
func attachmentVault(t *testing.T, links []string) (idx *Index, from *Doc, symlinks bool) {
	t.Helper()
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	for _, f := range []string{
		"assets/logo.png",
		"assets/my pic.png",
		"assets/Banner.PNG",
		"docs/report.pdf",
		"a/dup.png",
		"b/dup.png",
		"deep/z/dup.png",
		"pick.png",
		"a/pick.png",
		"b/pick.png",
		"plain",
		"near.png",
		"deep/x/near.png",
		"sub/sibling.png",
		".hidden.png",
		".trash/trashed.png",
		"node_modules/pkg/pkg.png",
		"folder/inside-folder.txt",
	} {
		checkWrite(t, filepath.Join(vault, f), "x")
	}
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(base, "outside.png"), "secret")
	checkWrite(t, filepath.Join(base, "outdir", "inside-link.png"), "secret")

	symlinks = os.Symlink(filepath.Join(base, "outside.png"), filepath.Join(vault, "leak.png")) == nil &&
		os.Symlink(filepath.Join(base, "outdir"), filepath.Join(vault, "linkdir")) == nil

	var src strings.Builder
	src.WriteString("# N\n\n")
	for _, l := range links {
		src.WriteString("- " + l + "\n")
	}
	checkWrite(t, filepath.Join(vault, "sub", "n.md"), src.String())

	idx = checkIndex(t, vault)
	return idx, mustFindDoc(t, idx, idx.roots[0].Label, "sub/n.md"), symlinks
}

var attachmentCases = []struct {
	link string
	want attachmentVerdict
}{
	// Resolved: root-relative, bare basename, case-folded, a space in the
	// name, a leading "/", a fragment, relative to the note, closest to the
	// root wins, a path suffix picks the one it names, and an alias label.
	{"[[assets/logo.png]]", attResolved},
	{"[[logo.png]]", attResolved},
	{"[[LOGO.PNG]]", attResolved},
	{"[[my pic.png]]", attResolved},
	{"[[banner.png]]", attResolved},
	{"[[/assets/logo.png]]", attResolved},
	{"[[report.pdf#page=3]]", attResolved},
	{"[[./sibling.png]]", attResolved},
	{"[[../assets/logo.png]]", attResolved},
	{"[[near.png]]", attResolved},
	{"[[x/near.png]]", attResolved},
	{"[[logo.png|The logo]]", attResolved},
	// The closest match wins even when the matches further from the root
	// tie among themselves; an extensionless file is named without one.
	{"[[pick.png]]", attResolved},
	{"[[plain]]", attResolved},
	// Missing: no such file, no extension, a suffix that names another
	// folder, a directory form, relative to the wrong folder, a dot-file, a
	// file under an excluded directory, and a folder.
	{"[[nope.png]]", attMissing},
	{"[[logo]]", attMissing},
	{"[[other/logo.png]]", attMissing},
	{"[[logo.png/]]", attMissing},
	{"[[./logo.png]]", attMissing},
	{"[[.hidden.png]]", attMissing},
	{"[[trashed.png]]", attMissing},
	{"[[pkg.png]]", attMissing},
	{"[[folder]]", attMissing},
	// A suffix matches whole folder names only, and ".md" is never
	// stripped from an attachment target.
	{"[[ssets/logo.png]]", attMissing},
	{"[[plain.md]]", attMissing},
	// Two matches equally close to the root, though a third further out is
	// unique.
	{"[[dup.png]]", attAmbiguous},
	// Out of the root, though the file exists there.
	{"[[../../outside.png]]", attOutside},
}

// symlinkCases name files that exist only through a symlink out of the
// root: a symlinked file and a file under a symlinked directory. Neither is
// an attachment.
var symlinkCases = []string{"[[leak.png]]", "[[inside-link.png]]", "[[linkdir/inside-link.png]]"}

func TestAttachment_ReaderRendersEachVerdict(t *testing.T) {
	links := make([]string, 0, len(attachmentCases))
	for _, c := range attachmentCases {
		links = append(links, c.link)
	}
	idx, from, symlinks := attachmentVault(t, append(links, symlinkCases...))

	check := func(link string, want attachmentVerdict) {
		t.Helper()
		out := renderVaultFrom(t, idx, from, link+"\n")
		if strings.Contains(out, "href=") {
			t.Errorf("%s: rendered an href, want none: %s", link, out)
		}
		var marker string
		switch want {
		case attResolved:
			marker = `<span class="wikilink wikilink-attachment" title="` + titleAttachment + `">`
		case attMissing:
			marker = `<span class="wikilink wikilink-miss" title="` + titleNoTarget + `">`
		case attAmbiguous:
			marker = `<span class="wikilink wikilink-miss" title="` + titleAmbiguous + `">`
		case attOutside:
			marker = `<span class="wikilink wikilink-miss" title="` + titleOutsideRoot + `">`
		}
		if !strings.Contains(out, marker) {
			t.Errorf("%s: want %s in: %s", link, marker, out)
		}
		if want != attResolved && strings.Contains(out, "wikilink-attachment") {
			t.Errorf("%s: marked as an attachment: %s", link, out)
		}
	}
	for _, c := range attachmentCases {
		check(c.link, c.want)
	}
	if symlinks {
		for _, l := range symlinkCases {
			check(l, attMissing)
		}
	}
}

// The attachment span's label is the wikilink's label, and the sanitizer
// keeps the span's class and title as written.
func TestAttachment_ReaderSpanShape(t *testing.T) {
	idx, from, _ := attachmentVault(t, nil)
	out := renderVaultFrom(t, idx, from, "[[logo.png|The <b>logo</b>]]\n")
	want := `<span class="wikilink wikilink-attachment" title="Attachment, not viewable in the reader yet">The &lt;b&gt;logo&lt;/b&gt;</span>`
	if !strings.Contains(out, want) {
		t.Errorf("want %s in: %s", want, out)
	}
}

func TestAttachment_CheckReportsEachVerdict(t *testing.T) {
	links := make([]string, 0, len(attachmentCases))
	for _, c := range attachmentCases {
		links = append(links, c.link)
	}
	idx, _, symlinks := attachmentVault(t, append(links, symlinkCases...))
	r := idx.Check()

	byLine := map[int]FindingKind{}
	for _, f := range r.Findings {
		if f.Path != "sub/n.md" {
			t.Errorf("finding %+v outside sub/n.md", f)
			continue
		}
		byLine[f.Line] = f.Kind
	}
	outside := 0
	for i, c := range attachmentCases {
		got, reported := byLine[i+3]
		switch c.want {
		case attResolved:
			if reported {
				t.Errorf("%s: reported %s, want nothing", c.link, got)
			}
		case attMissing:
			if got != FindingBrokenLink {
				t.Errorf("%s: finding %q, want %s", c.link, got, FindingBrokenLink)
			}
		case attAmbiguous:
			if got != FindingAmbiguousLink {
				t.Errorf("%s: finding %q, want %s", c.link, got, FindingAmbiguousLink)
			}
		case attOutside:
			outside++
			if reported {
				t.Errorf("%s: reported %s, want only the outside-root count", c.link, got)
			}
		}
	}
	if r.Summary.OutsideRootLinks != outside {
		t.Errorf("outside_root_links = %d, want %d", r.Summary.OutsideRootLinks, outside)
	}
	if symlinks {
		for j, l := range symlinkCases {
			if got := byLine[len(attachmentCases)+j+3]; got != FindingBrokenLink {
				t.Errorf("%s: finding %q, want %s (a symlink out of the root)", l, got, FindingBrokenLink)
			}
		}
	}
}

// An embed is resolved the same way in docs check; the reader still renders
// it as its source text (#581).
func TestAttachment_CheckResolvesEmbeds(t *testing.T) {
	idx, from, _ := attachmentVault(t, []string{"![[logo.png]]", "![[nope.png]]", "![[dup.png]]"})
	byLine := map[int]FindingKind{}
	for _, f := range idx.Check().Findings {
		byLine[f.Line] = f.Kind
	}
	if got, ok := byLine[3]; ok {
		t.Errorf("![[logo.png]]: reported %s, want nothing", got)
	}
	if byLine[4] != FindingBrokenLink {
		t.Errorf("![[nope.png]]: finding %q, want %s", byLine[4], FindingBrokenLink)
	}
	if byLine[5] != FindingAmbiguousLink {
		t.Errorf("![[dup.png]]: finding %q, want %s", byLine[5], FindingAmbiguousLink)
	}
	if out := renderVaultFrom(t, idx, from, "![[logo.png]]\n"); strings.Contains(out, "wikilink-attachment") {
		t.Errorf("embed rendered as an attachment span, want its source text: %s", out)
	}
}

// A plain markdown link keeps docs check's on-disk fallback and never takes
// the basename match: logo.png is not next to sub/n.md.
func TestAttachment_MarkdownLinkNotBasenameResolved(t *testing.T) {
	idx, _, _ := attachmentVault(t, []string{"[pic](logo.png)", "[img](../assets/logo.png)"})
	byLine := map[int]FindingKind{}
	for _, f := range idx.Check().Findings {
		byLine[f.Line] = f.Kind
	}
	if byLine[3] != FindingBrokenLink {
		t.Errorf("[pic](logo.png): finding %q, want %s", byLine[3], FindingBrokenLink)
	}
	if got, ok := byLine[4]; ok {
		t.Errorf("[img](../assets/logo.png): reported %s, want nothing", got)
	}
}

// A note still wins over an attachment of the same basename, and a docs
// root lists no attachments at all.
func TestAttachment_NoteWinsAndDocsRootHasNone(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(vault, "plan.md"), "# Plan\n")
	checkWrite(t, filepath.Join(vault, "plan"), "not a note")
	checkWrite(t, filepath.Join(vault, "n.md"), "# N\n\n[[plan]]\n")
	docsDir := filepath.Join(base, "docs")
	checkWrite(t, filepath.Join(docsDir, "README.md"), "# R\n")
	checkWrite(t, filepath.Join(docsDir, "logo.png"), "x")

	idx := checkIndex(t, vault, docsDir)
	from := mustFindDoc(t, idx, idx.roots[0].Label, "n.md")
	if out := renderVaultFrom(t, idx, from, "[[plan]]\n"); !strings.Contains(out, `href="/doc/`+idx.roots[0].Label+`/plan.md"`) {
		t.Errorf("[[plan]]: want the note, got: %s", out)
	}
	if ri := idx.byRoot[idx.roots[1].Label]; len(ri.attRel) != 0 || len(ri.attByName) != 0 {
		t.Errorf("docs root attachments = %v / %v, want none", ri.attRel, ri.attByName)
	}
	if ri := idx.byRoot[idx.roots[0].Label]; !ri.attRel["plan"] {
		t.Errorf("vault attachments = %v, want plan listed", ri.attRel)
	}
}
