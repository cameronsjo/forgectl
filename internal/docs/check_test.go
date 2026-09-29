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
//   [x] Happy: a vault root is skipped and reported as unchecked
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

func TestCheck_VaultRootSkipped(t *testing.T) {
	docsDir := t.TempDir()
	checkWrite(t, filepath.Join(docsDir, "README.md"), "# R\n")
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	checkWrite(t, filepath.Join(vault, "n.md"), "# N\n\n[[missing]]\n")

	r := checkIndex(t, docsDir, vault).Check()
	if len(r.Roots) != 2 {
		t.Fatalf("roots = %+v, want 2", r.Roots)
	}
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
	if vaultRoot.Checked || vaultRoot.Skipped == "" || vaultRoot.Docs != 1 {
		t.Errorf("vault root = %+v, want unchecked with a reason and 1 doc", vaultRoot)
	}
	if len(r.Findings) != 0 {
		t.Errorf("findings = %+v, want none from a skipped vault root", r.Findings)
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
