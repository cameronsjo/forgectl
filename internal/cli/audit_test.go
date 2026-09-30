package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/audit"
)

func runAuditInjection(t *testing.T, root string, args ...string) string {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cmd := newAuditInjectionCmd(func() string { return root }, func() time.Time { return now })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("audit injection %v: %v", args, err)
	}
	return out.String()
}

func auditSortedKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// TestAuditInjection_JSONShape pins the --json wire shape (ADR-0008:
// additive changes only): the exact key sets of the report and of a carrier
// row, arrays never null, and the field values for one known carrier.
func TestAuditInjection_JSONShape(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "r")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(repo, "AGENTS.md"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(runAuditInjection(t, root, "--json")), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v", err)
	}
	if keys := auditSortedKeys(got); keys != "capped_by,carriers,depth_skipped,entries_scanned,repos_scanned,root,truncated,unreadable_dirs" {
		t.Errorf("report keys = %s", keys)
	}
	carriers, ok := got["carriers"].([]any)
	if !ok || len(carriers) != 1 {
		t.Fatalf("carriers = %#v, want a one-element array", got["carriers"])
	}
	row, _ := carriers[0].(map[string]any)
	if keys := auditSortedKeys(row); keys != "anomalies,modified,path,repo,target,type" {
		t.Errorf("carrier keys = %s", keys)
	}
	wantRepo := repo // reported in the caller's spelling, never symlink-resolved
	wantRow := map[string]any{
		"path":     filepath.Join(wantRepo, "AGENTS.md"),
		"repo":     wantRepo,
		"target":   "AGENTS.md",
		"type":     "file",
		"modified": "2026-01-02T03:04:05Z",
	}
	for k, v := range wantRow {
		if row[k] != v {
			t.Errorf("carrier[%s] = %#v, want %#v", k, row[k], v)
		}
	}
	if a, ok := row["anomalies"].([]any); !ok || len(a) != 0 {
		t.Errorf("anomalies = %#v, want []", row["anomalies"])
	}
	if c, ok := got["capped_by"].([]any); !ok || len(c) != 0 {
		t.Errorf("capped_by = %#v, want []", got["capped_by"])
	}
	if got["repos_scanned"] != float64(1) || got["truncated"] != false {
		t.Errorf("repos_scanned=%v truncated=%v, want 1/false", got["repos_scanned"], got["truncated"])
	}
}

func TestAuditInjection_JSONEmptyIsArray(t *testing.T) {
	out := runAuditInjection(t, t.TempDir(), "--json")
	if !strings.Contains(out, `"carriers": []`) {
		t.Errorf("an empty scan must encode carriers as [], got:\n%s", out)
	}
}

// TestAuditInjection_TextEscapesHostilePaths: a clone-derived directory name
// carrying an escape sequence reaches the terminal escaped, not raw.
func TestAuditInjection_TextEscapesHostilePaths(t *testing.T) {
	root := t.TempDir()
	evil := filepath.Join(root, "evil\x1b[2J")
	if err := os.MkdirAll(evil, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "CLAUDE.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := runAuditInjection(t, root)
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("text output carries a raw ESC:\n%q", out)
	}
	if !strings.Contains(out, "CLAUDE.md") {
		t.Errorf("text output is missing the carrier:\n%s", out)
	}
	jsonOut := runAuditInjection(t, root, "--json")
	if strings.ContainsRune(jsonOut, '\x1b') {
		t.Errorf("--json output carries a raw ESC:\n%q", jsonOut)
	}
}

// TestAuditInjection_TextGroupsEachRepoOnce: a nested repo's carriers sort
// between its parent's (parent/CLAUDE.md, parent/m/CLAUDE.md,
// parent/zz/AGENTS.md), and the parent's header must still print once.
func TestAuditInjection_TextGroupsEachRepoOnce(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"p/.git", "p/m/.git", "p/zz"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"p/CLAUDE.md", "p/m/CLAUDE.md", "p/zz/AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := runAuditInjection(t, root)
	parent := filepath.Join(root, "p")
	headers := 0
	for _, line := range strings.Split(out, "\n") {
		if line == parent {
			headers++
		}
	}
	if headers != 1 {
		t.Errorf("parent repo header printed %d times, want 1:\n%s", headers, out)
	}
}

// TestAuditInjection_JSONEscapesC1AndBidi pins the termsafe encoder: a
// directory name carrying a C1 control (U+0085) and a bidi override (U+202E),
// both of which encoding/json passes through raw, reaches stdout escaped.
func TestAuditInjection_JSONEscapesC1AndBidi(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "x\u0085\u202ey")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := runAuditInjection(t, root, "--json")
	if strings.ContainsRune(out, '\u0085') || strings.ContainsRune(out, '\u202e') {
		t.Errorf("--json output carries a raw C1 or bidi rune:\n%q", out)
	}
	if !strings.Contains(out, `\u202e`) {
		t.Errorf("--json output lost the escaped bidi rune:\n%s", out)
	}
}

// TestAuditInjection_TextCapsLongPaths pins the echo cap: an ordinary but
// over-long path is cut (with the ellipsis) rather than printed whole.
func TestAuditInjection_TextCapsLongPaths(t *testing.T) {
	root := t.TempDir()
	long := filepath.Join(root, strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 200))
	if err := os.MkdirAll(long, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(long, "CLAUDE.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := runAuditInjection(t, root)
	if !strings.Contains(out, "…") {
		t.Errorf("a 600-rune path was not cut:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if utf8.RuneCountInString(line) > 700 {
			t.Errorf("line of %d runes exceeds the path echo cap", utf8.RuneCountInString(line))
		}
	}
}

// TestAuditInjection_TruncationNoteNamesTheCap: the note names the cap that
// fired, and says "stopped" only for a cap that stopped the scan.
func TestAuditInjection_TruncationNoteNamesTheCap(t *testing.T) {
	// Non-default caps: the notes must print the caps the scan ran with.
	caps := audit.Report{Truncated: true, MaxEntries: 7, MaxFindings: 8, MaxDepth: 9}
	var depth bytes.Buffer
	r := caps
	r.CappedBy, r.DepthSkipped = []string{audit.CapDepth}, 2
	writeAuditInjectionText(&depth, r)
	if !strings.Contains(depth.String(), "2 directories below the 9-level depth cap were not scanned") || strings.Contains(depth.String(), "stopped") {
		t.Errorf("depth-cap note:\n%s", depth.String())
	}
	var entries bytes.Buffer
	r = caps
	r.CappedBy = []string{audit.CapEntries}
	writeAuditInjectionText(&entries, r)
	if !strings.Contains(entries.String(), "stopped at the 7-entry cap") {
		t.Errorf("entries-cap note:\n%s", entries.String())
	}
	var findings bytes.Buffer
	r = caps
	r.CappedBy = []string{audit.CapFindings}
	writeAuditInjectionText(&findings, r)
	if !strings.Contains(findings.String(), "stopped at the 8-carrier cap") {
		t.Errorf("findings-cap note:\n%s", findings.String())
	}
	var js bytes.Buffer
	r = caps
	r.CappedBy, r.DepthSkipped = []string{audit.CapDepth}, 3
	if err := writeAuditInjectionJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"depth_skipped": 3`) {
		t.Errorf("--json lacks depth_skipped:\n%s", js.String())
	}
}

// TestAuditInjection_JSONZeroReportArrays: the wire arrays are never null,
// even for a Report built without the scanner's own initialization.
func TestAuditInjection_JSONZeroReportArrays(t *testing.T) {
	var out bytes.Buffer
	if err := writeAuditInjectionJSON(&out, audit.Report{Findings: []audit.Finding{{Path: "/p"}}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"capped_by": []`, `"anomalies": []`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s in:\n%s", want, out.String())
		}
	}
}
