package docs

// Test plan for check.go
//
// Index.Check (Classification: pure analysis over a built Index)
//   [x] Unhappy: a link to a missing file is broken_link, carrying the raw target
//   [x] Unhappy: a missing heading anchor is broken_anchor; a valid one is silent
//   [x] Unhappy: a missing ^block anchor is broken_anchor
//   [x] Unhappy: a fragment-only #x with no such heading is broken_anchor
//   [x] Unhappy: a target matching two docs is ambiguous_link
//   [x] Happy: a directory link and a non-markdown file link are not broken
//       (the existence fallback)
//   [x] Happy: a root-relative /LICENSE from a subdirectory doc is not broken
//       when only the root has a LICENSE; a missing /NOPE is
//   [x] Contract: a target's control bytes reach Finding.Target raw (the CLI
//       escapes them)
//   [x] Unhappy: a symlink escaping the root is broken, not "exists"
//   [x] Unhappy: an unlinked doc is an orphan; the root README is not
//   [x] Happy: a README in a subdirectory is not a root index
//   [x] Happy: a directory link counts as inbound to that directory's README
//       or index page (any case, relative or root-relative); a sibling doc
//       in the directory is still an orphan
//   [x] Unhappy: a README's own "./" link does not make it inbound
//   [x] Happy: "sub/" and "/sub/." count for sub/README.md even when a
//       sub.md sits beside the directory, and leave that sub.md an orphan
//   [x] Happy: a link finding carries the source line of the file as written,
//       frontmatter lines included
//   [x] Happy: an out-of-root link is counted, never reported
//   [x] Happy: a vault root is checked: broken/ambiguous wikilinks and broken
//       heading and ^block anchors are findings, orphans are never
//   [x] Parity: check's broken vault wikilinks equal the reader's wikilink-miss set
//   [x] Happy: the checked-in vault fixture checks clean, its outside-root link counted
//   [x] Happy: a single-file root has no orphans
//   [x] Happy: findings sort by root, path, then link findings by line, target
//       and kind, then lineless findings by kind — not walk order
//   [x] Happy: no findings encodes as [], never null
//   [x] Unhappy: a passed stale_after is one stale finding carrying the value;
//       a future one is none
//   [x] Unhappy: status: deprecated is one deprecated finding
//   [x] Happy: a past date-only stale_after is no finding
//   [x] Happy: a stale doc in a vault root is no finding
//   [x] Happy: a stale finding encodes as kind, root, path, stale_after
//
// isRootIndex, existsInRoot are exercised through Check.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// checkWrite writes a fixture file with owner-only permissions.
func checkWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkIndex(t *testing.T, paths ...string) *Index {
	t.Helper()
	idx, err := NewIndex(paths)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return idx
}

// findingsOf returns the report's findings of one kind.
func findingsOf(r CheckReport, kind FindingKind) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestCheck_BrokenLink(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[gone](missing.md) and [ok](a.md)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "# A\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenLink)
	if len(got) != 1 {
		t.Fatalf("broken_link findings = %+v, want exactly 1", r.Findings)
	}
	if got[0].Path != "README.md" || got[0].Target != "missing.md" {
		t.Errorf("finding = %+v, want README.md -> missing.md", got[0])
	}
	if r.Summary.BrokenLinks != 1 {
		t.Errorf("Summary.BrokenLinks = %d, want 1", r.Summary.BrokenLinks)
	}
}

func TestCheck_BrokenHeadingAnchor(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"),
		"# R\n\n[bad](a.md#nope) [good](a.md#real-heading)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "# A\n\n## Real heading\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenAnchor)
	if len(got) != 1 || got[0].Target != "a.md#nope" {
		t.Fatalf("broken_anchor findings = %+v, want only a.md#nope", got)
	}
}

func TestCheck_BrokenBlockAnchor(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[blk](a.md#^nothere)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "# A\n\ntext ^other\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenAnchor)
	if len(got) != 1 || got[0].Target != "a.md#^nothere" {
		t.Fatalf("broken_anchor findings = %+v, want a.md#^nothere", got)
	}
}

func TestCheck_FragmentOnlySelfAnchor(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[self](#nowhere) [ok](#r)\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenAnchor)
	if len(got) != 1 || got[0].Target != "#nowhere" || got[0].Path != "README.md" {
		t.Fatalf("broken_anchor findings = %+v, want only README.md #nowhere", got)
	}
}

func TestCheck_Ambiguous(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[n](notes)\n")
	checkWrite(t, filepath.Join(dir, "notes.md"), "# N1\n")
	checkWrite(t, filepath.Join(dir, "notes.markdown"), "# N2\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingAmbiguousLink)
	if len(got) != 1 || got[0].Target != "notes" {
		t.Fatalf("ambiguous_link findings = %+v (all: %+v), want notes", got, r.Findings)
	}
	if r.Summary.AmbiguousLinks != 1 {
		t.Errorf("Summary.AmbiguousLinks = %d, want 1", r.Summary.AmbiguousLinks)
	}
}

func TestCheck_DirectoryAndAssetLinksAreNotBroken(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"),
		"# R\n\n[d](sub/) [lic](LICENSE) [t](notes.txt) [gone](nope.txt)\n[a](a.md)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "# A\n")
	checkWrite(t, filepath.Join(dir, "sub", "x.md"), "# X\n")
	checkWrite(t, filepath.Join(dir, "LICENSE"), "MIT\n")
	checkWrite(t, filepath.Join(dir, "notes.txt"), "hi\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenLink)
	if len(got) != 1 || got[0].Target != "nope.txt" {
		t.Fatalf("broken_link findings = %+v, want only nope.txt", got)
	}
}

// A root-relative target resolves from the root, not from the linking doc's
// directory: /LICENSE from sub/x.md names the root's LICENSE, and there is no
// sub/LICENSE for a doc-relative reading to find.
func TestCheck_RootRelativeAssetLinkIsNotBroken(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[x](sub/x.md)\n")
	checkWrite(t, filepath.Join(dir, "sub", "x.md"), "# X\n\n[l](/LICENSE) [gone](/NOPE)\n")
	checkWrite(t, filepath.Join(dir, "LICENSE"), "MIT\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenLink)
	if len(got) != 1 || got[0].Path != "sub/x.md" || got[0].Target != "/NOPE" {
		t.Fatalf("broken_link findings = %+v, want only sub/x.md -> /NOPE", got)
	}
}

// Check reports a link target as authored, control bytes included: escaping
// is the presentation layer's job (internal/cli/docs_check.go runs every
// Target through termsafe.SafeLine). This pins the raw value that layer
// receives, so a change that pre-escapes or drops it here is deliberate.
func TestCheck_TargetCarriesRawControlBytes(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[e](<bad\x1b[31m.md>)\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenLink)
	if len(got) != 1 || got[0].Target != "bad\x1b[31m.md" {
		t.Fatalf("broken_link findings = %+v, want one whose Target is the raw bad\\x1b[31m.md", got)
	}
}

func TestCheck_SymlinkEscapeIsBroken(t *testing.T) {
	outside := t.TempDir()
	checkWrite(t, filepath.Join(outside, "secret.txt"), "s\n")
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[s](leak.txt)\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "leak.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingBrokenLink)
	if len(got) != 1 || got[0].Target != "leak.txt" {
		t.Fatalf("broken_link findings = %+v, want leak.txt (escaping symlink)", r.Findings)
	}
}

// forgectl#611 item 3: a link written as a directory must name one. A
// trailing slash on a regular file, a doc or not, is a broken link; on a
// real directory it passes as before.
func TestCheck_TrailingSlashOnAFileIsBroken(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[g](guide.md/) [l](LICENSE/) [s](sub/) [ok](guide.md)\n")
	checkWrite(t, filepath.Join(dir, "guide.md"), "# G\n")
	checkWrite(t, filepath.Join(dir, "LICENSE"), "MIT\n")
	checkWrite(t, filepath.Join(dir, "sub", "notes.txt"), "n\n")

	r := checkIndex(t, dir).Check()
	var targets []string
	for _, f := range findingsOf(r, FindingBrokenLink) {
		targets = append(targets, f.Target)
	}
	if len(targets) != 2 || targets[0] != "LICENSE/" || targets[1] != "guide.md/" {
		t.Fatalf("broken_link targets = %q, want [LICENSE/ guide.md/]; findings %+v", targets, r.Findings)
	}
}

func TestCheck_Orphan(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[a](a.md)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "# A\n")
	checkWrite(t, filepath.Join(dir, "b.md"), "# B\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingOrphan)
	if len(got) != 1 || got[0].Path != "b.md" || got[0].Target != "" {
		t.Fatalf("orphan findings = %+v, want only b.md", got)
	}
	if r.Summary.Orphans != 1 {
		t.Errorf("Summary.Orphans = %d, want 1", r.Summary.Orphans)
	}
}

func TestCheck_SubdirReadmeIsNotRootIndex(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	checkWrite(t, filepath.Join(dir, "sub", "README.md"), "# Sub\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingOrphan)
	if len(got) != 1 || got[0].Path != "sub/README.md" {
		t.Fatalf("orphan findings = %+v, want sub/README.md only", got)
	}
}

func TestCheck_DirectoryLinkCountsAsIndexInbound(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[plans](plans/)\n")
	// Root-relative, from a subdirectory: resolves against the root, not plans/.
	checkWrite(t, filepath.Join(dir, "plans", "README.md"), "# Plans\n\n[guides](/guides)\n")
	checkWrite(t, filepath.Join(dir, "plans", "other.md"), "# Other\n")
	checkWrite(t, filepath.Join(dir, "guides", "Index.md"), "# Guides\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingOrphan)
	if len(got) != 1 || got[0].Path != "plans/other.md" {
		t.Fatalf("orphan findings = %+v, want plans/other.md only", got)
	}
	if n := len(findingsOf(r, FindingBrokenLink)); n != 0 {
		t.Errorf("broken_link findings = %d, want 0 for directory links", n)
	}
}

func TestCheck_TrailingSlashLinkReachesReadmeNotSameStemFile(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[s](sub/) [t](/sub/.)\n")
	checkWrite(t, filepath.Join(dir, "sub.md"), "# Sub file\n")
	checkWrite(t, filepath.Join(dir, "sub", "README.md"), "# Sub dir\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingOrphan)
	if len(got) != 1 || got[0].Path != "sub.md" {
		t.Fatalf("orphan findings = %+v, want sub.md only (sub/ is the directory)", got)
	}
	if n := len(findingsOf(r, FindingBrokenLink)); n != 0 {
		t.Errorf("broken_link findings = %d, want 0 for a directory link", n)
	}
}

func TestCheck_ReadmeSelfDirectoryLinkIsNotInbound(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	checkWrite(t, filepath.Join(dir, "sub", "README.md"), "# Sub\n\n[here](./)\n")

	r := checkIndex(t, dir).Check()
	got := findingsOf(r, FindingOrphan)
	if len(got) != 1 || got[0].Path != "sub/README.md" {
		t.Fatalf("orphan findings = %+v, want sub/README.md", got)
	}
}

func TestCheck_LinkFindingCarriesSourceLine(t *testing.T) {
	dir := t.TempDir()
	// Frontmatter is lines 1-3; the broken links sit on file lines 7 and 9.
	checkWrite(t, filepath.Join(dir, "README.md"),
		"---\ntitle: R\n---\n# R\n\ntext\nsee [gone](missing.md)\n\n[bad](README.md#nope)\n")

	r := checkIndex(t, dir).Check()
	lines := map[string]int{}
	for _, f := range r.Findings {
		lines[f.Target] = f.Line
	}
	if lines["missing.md"] != 7 || lines["README.md#nope"] != 9 {
		t.Errorf("finding lines = %v, want missing.md:7 and README.md#nope:9", lines)
	}
}

func TestCheck_OutsideRootCountedNotReported(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "A")
	b := filepath.Join(base, "B")
	checkWrite(t, filepath.Join(a, "README.md"), "# A\n\n[x](../B/x.md)\n")
	checkWrite(t, filepath.Join(b, "x.md"), "# X\n")

	r := checkIndex(t, a, b).Check()
	if r.Summary.OutsideRootLinks != 1 {
		t.Errorf("OutsideRootLinks = %d, want 1", r.Summary.OutsideRootLinks)
	}
	for _, f := range r.Findings {
		if f.Kind != FindingOrphan {
			t.Errorf("unexpected non-orphan finding %+v; out-of-root links are not findings", f)
		}
	}
	orphans := findingsOf(r, FindingOrphan)
	if len(orphans) != 1 || orphans[0].Root != "B" || orphans[0].Path != "x.md" {
		t.Errorf("orphans = %+v, want B/x.md only (links never cross roots)", orphans)
	}
}

func TestCheck_VaultRootChecked(t *testing.T) {
	docsDir := t.TempDir()
	checkWrite(t, filepath.Join(docsDir, "README.md"), "# R\n")
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(vault, "n.md"), "# N\n\n[[missing]]\n[[t#^nope]]\n[[t#No Such]]\n[[t#^blk]]\n[[dup]]\n")
	checkWrite(t, filepath.Join(vault, "t.md"), "# T\n\nPara. ^blk\n")
	checkWrite(t, filepath.Join(vault, "a/dup.md"), "# A\n")
	checkWrite(t, filepath.Join(vault, "b/dup.md"), "# B\n")
	checkWrite(t, filepath.Join(vault, "daily.md"), "# Daily\n")

	r := checkIndex(t, docsDir, vault).Check()
	var docsRoot, vaultRoot CheckedRoot
	for _, cr := range r.Roots {
		if cr.Kind == "vault" {
			vaultRoot = cr
		} else {
			docsRoot = cr
		}
	}
	if !docsRoot.Checked || docsRoot.Skipped != "" {
		t.Errorf("docs root = %+v, want checked", docsRoot)
	}
	if !vaultRoot.Checked || vaultRoot.Skipped != "" || vaultRoot.Docs != 5 {
		t.Errorf("vault root = %+v, want checked, no skip reason, 5 docs", vaultRoot)
	}
	got := map[string]bool{}
	for _, f := range r.Findings {
		if f.Root != vaultRoot.Label {
			t.Errorf("finding %+v outside the vault", f)
		}
		got[string(f.Kind)+" "+f.Target] = true
	}
	want := []string{
		"broken_link missing", "broken_anchor t#^nope", "broken_anchor t#No Such", "ambiguous_link dup",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("findings = %+v, missing %q", r.Findings, w)
		}
	}
	if len(r.Findings) != len(want) {
		t.Errorf("findings = %+v, want exactly %d (valid [[t#^blk]] silent, no orphans)", r.Findings, len(want))
	}
	if r.Summary.Orphans != 0 || r.Summary.IgnoredOrphans != 0 {
		t.Errorf("summary = %+v, want no orphan accounting in a vault", r.Summary)
	}
}

// The checker reports exactly the wikilinks the reader renders as a miss.
func TestCheck_VaultParityWithReader(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(vault, "t.md"), "---\naliases: [Tee]\n---\n# T\n\n## Some Heading\n\nPara. ^blk\n")
	checkWrite(t, filepath.Join(vault, "assets/my pic.png"), "x")
	checkWrite(t, filepath.Join(vault, "assets/doc.pdf"), "x")
	checkWrite(t, filepath.Join(vault, "sub/inner.md"), "# Inner\n")

	forms := []struct {
		link string
		want bool // broken
	}{
		{"[[t]]", false},
		{"[[t|a]]", false},
		{`[[t\|a]]`, false},
		{"[[t#^blk]]", false},
		{"[[t#^nope]]", true},
		{"[[t#Some Heading]]", false},
		{"[[t#some-heading]]", false},
		{"[[t#No Such]]", true},
		{"[[T]]", false},
		{"[[t.md]]", false},
		{"[[Tee]]", false},
		{"[[assets/my pic.png]]", true},
		{"[[assets/doc.pdf]]", true},
		{"[[sub]]", true},
		{"[[missing]]", true},
	}
	var src strings.Builder
	src.WriteString("# N\n\n")
	for _, f := range forms {
		src.WriteString("- " + f.link + "\n")
	}
	checkWrite(t, filepath.Join(vault, "n.md"), src.String())

	idx := checkIndex(t, vault)
	from := mustFindDoc(t, idx, idx.roots[0].Label, "n.md")
	broken := map[int]bool{}
	for _, f := range idx.Check().Findings {
		if f.Path == "n.md" {
			broken[f.Line] = true
		}
	}
	for i, f := range forms {
		line := i + 3
		// Render the one link alone so its miss is attributable.
		doc, err := RenderDocFor(RootVault, []byte("# N\n\n"+f.link+"\n"), idx, from)
		if err != nil {
			t.Fatal(err)
		}
		reader := strings.Contains(doc.HTML, "wikilink-miss")
		if reader != f.want || broken[line] != reader {
			t.Errorf("%s: reader miss=%v, check broken=%v, want %v", f.link, reader, broken[line], f.want)
		}
	}
}

func TestCheck_VaultFixtureClean(t *testing.T) {
	r := checkIndex(t, filepath.Join(copyLinksFixture(t), "vault")).Check()
	if len(r.Findings) != 0 {
		t.Errorf("findings = %+v, want none from the fixture vault", r.Findings)
	}
	if r.Summary.OutsideRootLinks != 1 {
		t.Errorf("outside_root_links = %d, want 1 ([[../repo/index]])", r.Summary.OutsideRootLinks)
	}
}

func TestCheck_SingleFileRootNoOrphan(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "solo.md")
	checkWrite(t, f, "# Solo\n")

	r := checkIndex(t, f).Check()
	if len(r.Findings) != 0 {
		t.Errorf("findings = %+v, want none for a single-file root", r.Findings)
	}
	if len(r.Roots) != 1 || !r.Roots[0].Checked {
		t.Errorf("roots = %+v, want one checked root", r.Roots)
	}
}

func TestCheck_FindingsSorted(t *testing.T) {
	dir := t.TempDir()
	// The index orders docs by mtime, so make a.md the OLDER file: walk
	// order then puts b.md first and only the sort can restore path order.
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[a](a.md) [b](b.md) [c](c.md)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "# A\n\n[z](zz.md) [y](yy.md)\n")
	// c.md: line order must beat target and kind order (qq line 6, bb line 8,
	// the anchor line 9), and its lineless deprecated finding comes last.
	checkWrite(t, filepath.Join(dir, "c.md"), "---\nstatus: deprecated\n---\n# C\n\n[q](qq.md)\n\n[b](bb.md)\n[k](README.md#nope)\n")
	checkWrite(t, filepath.Join(dir, "b.md"), "# B\n\n[m](mm.md)\n")
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "a.md"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "b.md"), now, now); err != nil {
		t.Fatal(err)
	}

	r := checkIndex(t, dir).Check()
	var got []string
	for _, f := range r.Findings {
		got = append(got, fmt.Sprintf("%s|%s|%s|%d", f.Path, f.Kind, f.Target, f.Line))
	}
	want := []string{
		"a.md|broken_link|yy.md|3",
		"a.md|broken_link|zz.md|3",
		"b.md|broken_link|mm.md|3",
		"c.md|broken_link|qq.md|6",
		"c.md|broken_link|bb.md|8",
		"c.md|broken_anchor|README.md#nope|9",
		"c.md|deprecated||0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCheck_EmptyFindingsEncodeAsArray(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n")

	r := checkIndex(t, dir).Check()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"findings":[]`) {
		t.Errorf("JSON = %s, want findings encoded as []", raw)
	}
	if !strings.Contains(string(raw), `"schema_version":1`) {
		t.Errorf("JSON = %s, want schema_version 1", raw)
	}
}

func TestCheckAt_Stale(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "---\nstale_after: 2026-09-28T12:00:00Z\n---\n# R\n")

	r := checkIndex(t, dir).CheckAt(trustTestNow)
	got := findingsOf(r, FindingStale)
	if len(got) != 1 || len(r.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one stale", r.Findings)
	}
	if got[0].Path != "README.md" || got[0].StaleAfter != "2026-09-28T12:00:00Z" {
		t.Errorf("finding = %+v, want README.md carrying its stale_after", got[0])
	}
	if r.Summary.Stale != 1 || r.Summary.Deprecated != 0 {
		t.Errorf("summary = %+v, want Stale 1, Deprecated 0", r.Summary)
	}

	checkWrite(t, filepath.Join(dir, "README.md"), "---\nstale_after: 2026-09-30T12:00:00Z\n---\n# R\n")
	if r := checkIndex(t, dir).CheckAt(trustTestNow); len(r.Findings) != 0 {
		t.Errorf("future stale_after findings = %+v, want none", r.Findings)
	}
}

func TestCheckAt_Deprecated(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "---\nstatus: deprecated\n---\n# R\n")

	r := checkIndex(t, dir).CheckAt(trustTestNow)
	if got := findingsOf(r, FindingDeprecated); len(got) != 1 || len(r.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one deprecated", r.Findings)
	}
	if r.Summary.Deprecated != 1 {
		t.Errorf("Summary.Deprecated = %d, want 1", r.Summary.Deprecated)
	}
}

func TestCheckAt_DateOnlyStaleAfterIgnored(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "---\nstale_after: 2020-01-01\n---\n# R\n")

	if r := checkIndex(t, dir).CheckAt(trustTestNow); len(r.Findings) != 0 {
		t.Errorf("findings = %+v, want none for a date-only value", r.Findings)
	}
}

func TestCheckAt_VaultStaleNotReported(t *testing.T) {
	docsDir := t.TempDir()
	checkWrite(t, filepath.Join(docsDir, "README.md"), "# R\n")
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(vault, "n.md"), "---\nstale_after: 2026-09-28T12:00:00Z\nstatus: deprecated\n---\n# N\n")

	if r := checkIndex(t, docsDir, vault).CheckAt(trustTestNow); len(r.Findings) != 0 {
		t.Errorf("findings = %+v, want none from a vault root", r.Findings)
	}
}

func TestCheckAt_StaleFindingWireKeys(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "---\nstale_after: 2026-09-28T12:00:00Z\n---\n# R\n")

	r := checkIndex(t, dir).CheckAt(trustTestNow)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", r.Findings)
	}
	raw, err := json.Marshal(r.Findings[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "kind,path,root,severity,stale_after" {
		t.Errorf("stale finding keys = %s, want kind,path,root,severity,stale_after", got)
	}
}

// Every kind carries a severity on the wire: deprecated is info, every other
// kind is error (stale included).
func TestCheckAt_Severity(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[a](a.md)\n[b](gone.md)\n[c](a.md#nope)\n")
	checkWrite(t, filepath.Join(dir, "a.md"), "---\nstatus: deprecated\nstale_after: 2026-09-28T12:00:00Z\n---\n# A\n")
	checkWrite(t, filepath.Join(dir, "lonely.md"), "# L\n")

	r := checkIndex(t, dir).CheckAt(trustTestNow)
	want := map[FindingKind]Severity{
		FindingDeprecated:   SeverityInfo,
		FindingStale:        SeverityError,
		FindingBrokenLink:   SeverityError,
		FindingBrokenAnchor: SeverityError,
		FindingOrphan:       SeverityError,
	}
	seen := map[FindingKind]bool{}
	for _, f := range r.Findings {
		seen[f.Kind] = true
		if w, ok := want[f.Kind]; ok && f.Severity != w {
			t.Errorf("%s severity = %q, want %q", f.Kind, f.Severity, w)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("fixture produced no %s finding; findings = %+v", k, r.Findings)
		}
	}
	if got := r.Errors(); got != len(r.Findings)-1 {
		t.Errorf("Errors() = %d of %d findings, want all but the deprecated one", got, len(r.Findings))
	}
}

// A link inside image alt text is not a link on the page, so docs check does
// not report it broken (forgectl#596). The control link beside it proves the
// check ran. Mutation: drop the hasImageAncestor check on ast.KindLink in
// scanBodyFor and this reports two broken links.
func TestCheck_LinkInsideImageAltIsNotBroken(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n\n![see [gone](alt-missing.md)](pic.png) [x](missing.md)\n")
	checkWrite(t, filepath.Join(dir, "pic.png"), "png\n")

	got := findingsOf(checkIndex(t, dir).Check(), FindingBrokenLink)
	if len(got) != 1 || got[0].Target != "missing.md" {
		t.Fatalf("broken_link findings = %+v, want only missing.md", got)
	}
}

func TestCheck_OrphanOKSuppressesOrphanAndIsCounted(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	checkWrite(t, filepath.Join(dir, "kept.md"), "---\norphan_ok: true\n---\n# Kept\n")
	checkWrite(t, filepath.Join(dir, "linked-off.md"), "---\norphan_ok: false\n---\n# Off\n")
	checkWrite(t, filepath.Join(dir, "typo.md"), "---\norphan_ok: \"true\"\n---\n# Typo\n")
	checkWrite(t, filepath.Join(dir, "plain.md"), "# Plain\n")

	r := checkIndex(t, dir).Check()
	var got []string
	for _, f := range findingsOf(r, FindingOrphan) {
		got = append(got, f.Path)
	}
	sort.Strings(got)
	if want := "linked-off.md plain.md typo.md"; strings.Join(got, " ") != want {
		t.Errorf("orphans = %v, want %s (only the boolean true opts out)", got, want)
	}
	if r.Summary.Orphans != 3 || r.Summary.IgnoredOrphans != 1 {
		t.Errorf("Summary orphans/ignored = %d/%d, want 3/1", r.Summary.Orphans, r.Summary.IgnoredOrphans)
	}
}

func TestCheck_OrphanOKDoesNotHideLinkFindings(t *testing.T) {
	dir := t.TempDir()
	checkWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	checkWrite(t, filepath.Join(dir, "kept.md"), "---\norphan_ok: true\n---\n# Kept\n\n[gone](missing.md)\n")

	r := checkIndex(t, dir).Check()
	if len(findingsOf(r, FindingBrokenLink)) != 1 {
		t.Errorf("findings = %+v, want the broken link still reported", r.Findings)
	}
	if len(findingsOf(r, FindingOrphan)) != 0 {
		t.Errorf("findings = %+v, want no orphan", r.Findings)
	}
}

// TestCheck_VaultFragmentBudgetPerDoc (#710): docs check spends at most one
// fragment budget of rendered-text parsing per source doc. A doc of more
// markup-laden heading links than the budget covers gets broken_anchor for
// the links the budget parsed and an info anchor_unchecked for the rest, a
// second doc gets a fresh budget, and a slug link past the budget still
// resolves. The broken_anchor count is the parse count, so it bounds the
// work: a nil (unlimited) budget in Check turns it red (every link parsed,
// no anchor_unchecked), and so does one budget shared by every doc (the
// second doc gets none). Returning "broken_anchor" for a refusal turns the
// severity and summary checks red.
func TestCheck_VaultFragmentBudgetPerDoc(t *testing.T) {
	// A fragment that misses "## x" as written and by its rendered text
	// ("q"), and costs one maxRenderedFragment-byte parse.
	const head, tail = "[q](y)%%", "%%"
	frag := head + strings.Repeat("c", maxRenderedFragment-len(head)-len(tail)) + tail
	const budgetLinks = maxFragmentParseBytes / maxRenderedFragment
	const extra = 7
	note := func(title string) string {
		var sb strings.Builder
		sb.WriteString("# " + title + "\n\n## x\n\n")
		for i := 0; i < budgetLinks+extra; i++ {
			sb.WriteString("[[t#" + frag + "]]\n")
		}
		sb.WriteString("[[t#x]]\n")
		return sb.String()
	}
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(vault, "t.md"), "# T\n\n## x\n")
	checkWrite(t, filepath.Join(vault, "a.md"), note("A"))
	checkWrite(t, filepath.Join(vault, "b.md"), note("B"))

	r := checkIndex(t, vault).Check()
	for _, path := range []string{"a.md", "b.md"} {
		broken, unchecked := 0, 0
		for _, f := range r.Findings {
			if f.Path != path {
				continue
			}
			switch f.Kind {
			case FindingBrokenAnchor:
				broken++
			case FindingUncheckedAnchor:
				unchecked++
				if f.Severity != SeverityInfo || f.Line == 0 || f.Target == "" {
					t.Errorf("%s: anchor_unchecked finding %+v, want severity info with a line and target", path, f)
				}
			default:
				t.Errorf("%s: unexpected finding %+v", path, f)
			}
		}
		if broken != budgetLinks || unchecked != extra {
			t.Errorf("%s: %d broken_anchor + %d anchor_unchecked, want %d + %d", path, broken, unchecked, budgetLinks, extra)
		}
	}
	if r.Summary.UncheckedAnchors != 2*extra || r.Summary.BrokenAnchors != 2*budgetLinks {
		t.Errorf("summary = %+v, want %d unchecked and %d broken anchors", r.Summary, 2*extra, 2*budgetLinks)
	}
	if got, want := r.Errors(), 2*budgetLinks; got != want {
		t.Errorf("Errors() = %d, want %d: anchor_unchecked must not fail the check", got, want)
	}
}
