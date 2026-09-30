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
	if keys := auditSortedKeys(got); keys != "carriers,entries_scanned,repos_scanned,root,truncated,unreadable_dirs" {
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
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	wantRow := map[string]any{
		"path":     filepath.Join(resolvedRepo, "AGENTS.md"),
		"repo":     resolvedRepo,
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
