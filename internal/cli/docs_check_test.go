package cli

// Test plan for docs_check.go
//
// newDocsCheckCmd (Classification: API handler / cobra command)
//   [x] Happy: a clean tree exits 0 with no output
//   [x] Unhappy: a seeded broken link exits 1 and prints the finding as
//       path:line
//   [x] Unhappy: a seeded orphan exits 1
//   [x] Happy: --json key sets are frozen at the top, finding, summary, and
//       root levels (ADR-0008 rule 2: additive only)
//   [x] Unhappy: a missing root exits 2
//   [x] Unhappy: only vault roots exits 2, with a skip note on stderr
//   [x] Unhappy: an unknown flag exits 2
//   [x] Unhappy: control characters in a path never reach stdout raw
//   [x] Unhappy: a passed stale_after exits 1 with one stale line carrying the
//       value; a future one exits 0 silent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/module"
)

// runDocsCheck executes `docs check args...` and returns stdout, stderr, and
// the exit code the process would report.
func runDocsCheck(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := newDocsCheckCmd(module.Deps{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	code := 0
	if err != nil {
		code = ExitCode(err)
	}
	return stdout.String(), stderr.String(), code
}

func docsCheckWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := sortedKeys(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s keys = %v, want %v", what, got, want)
	}
}

func TestDocsCheckCmd_CleanExitsZero(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[a](a.md)\n")
	docsCheckWrite(t, filepath.Join(dir, "a.md"), "# A\n")

	stdout, _, code := runDocsCheck(t, dir)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (stdout %q)", code, stdout)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty for a clean tree", stdout)
	}
}

func TestDocsCheckCmd_SeededBrokenLinkExits1(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[x](missing.md)\n")

	stdout, _, code := runDocsCheck(t, dir)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "README.md:3: broken_link missing.md") {
		t.Errorf("stdout = %q, want the broken_link line", stdout)
	}
}

func TestDocsCheckCmd_SeededOrphanExits1(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	docsCheckWrite(t, filepath.Join(dir, "lonely.md"), "# L\n")

	stdout, _, code := runDocsCheck(t, dir)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "lonely.md: orphan") {
		t.Errorf("stdout = %q, want the orphan line", stdout)
	}
}

func TestDocsCheckCmd_JSONSchemaFrozen(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[x](missing.md)\n")
	docsCheckWrite(t, filepath.Join(dir, "lonely.md"), "# L\n")
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	docsCheckWrite(t, filepath.Join(vault, "n.md"), "# N\n")

	stdout, _, code := runDocsCheck(t, "--json", dir, vault)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	assertKeys(t, "top-level", got, "schema_version", "roots", "findings", "summary")
	if v, _ := got["schema_version"].(float64); v != 1 {
		t.Errorf("schema_version = %v, want 1", got["schema_version"])
	}
	assertKeys(t, "summary", got["summary"].(map[string]any),
		"broken_links", "ambiguous_links", "broken_anchors", "orphans", "outside_root_links",
		"deprecated", "stale")

	byKind := map[string]map[string]any{}
	for _, raw := range got["findings"].([]any) {
		f := raw.(map[string]any)
		byKind[f["kind"].(string)] = f
	}
	if f, ok := byKind["broken_link"]; !ok {
		t.Error("no broken_link finding in the report")
	} else {
		assertKeys(t, "link finding", f, "kind", "severity", "root", "path", "target", "line")
	}
	if f, ok := byKind["orphan"]; !ok {
		t.Error("no orphan finding in the report")
	} else {
		assertKeys(t, "orphan finding", f, "kind", "severity", "root", "path")
	}

	roots := got["roots"].([]any)
	if len(roots) != 2 {
		t.Fatalf("roots = %v, want 2", roots)
	}
	for _, raw := range roots {
		r := raw.(map[string]any)
		if r["kind"] == "vault" {
			assertKeys(t, "vault root", r, "label", "kind", "checked", "skipped", "docs")
		} else {
			assertKeys(t, "docs root", r, "label", "kind", "checked", "docs")
		}
	}
}

func TestDocsCheckCmd_MissingRootExits2(t *testing.T) {
	_, _, code := runDocsCheck(t, filepath.Join(t.TempDir(), "missing"))
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestDocsCheckCmd_OnlyVaultRootExits2(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	docsCheckWrite(t, filepath.Join(vault, "n.md"), "# N\n")

	stdout, stderr, code := runDocsCheck(t, vault)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty when nothing could be checked", stdout)
	}
	if !strings.Contains(stderr, "skipping vault root") {
		t.Errorf("stderr = %q, want a skipping-vault note", stderr)
	}
}

func TestDocsCheckCmd_UnknownFlagExits2(t *testing.T) {
	_, _, code := runDocsCheck(t, "--nope")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestDocsCheckCmd_ControlCharsInTargetEscaped(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	docsCheckWrite(t, filepath.Join(dir, "e\x1b[31mx.md"), "# E\n")

	stdout, _, code := runDocsCheck(t, dir)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (the odd-named doc is an orphan)", code)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("stdout carries a raw ESC: %q", stdout)
	}
	if !strings.Contains(stdout, "orphan") {
		t.Errorf("stdout = %q, want the orphan finding still printed", stdout)
	}
}

func TestDocsCheckCmd_StaleAfterExits1(t *testing.T) {
	dir := t.TempDir()
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "---\nstale_after: "+past+"\n---\n# R\n")

	stdout, _, code := runDocsCheck(t, dir)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, stdout)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "/README.md: stale "+past) {
		t.Errorf("stdout = %q, want one <label>/README.md: stale %s line", stdout, past)
	}

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "---\nstale_after: "+future+"\n---\n# R\n")
	stdout, _, code = runDocsCheck(t, dir)
	if code != 0 || stdout != "" {
		t.Errorf("future stale_after: exit = %d, stdout = %q; want 0 and empty", code, stdout)
	}
}

// A deadline whose JSON error object cannot be written names docs check, not
// docs list, whose helper it shares.
func TestDocsCheckCmd_DeadlineEncodeFailureNamesCheck(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")

	cmd := newDocsCheckCmd(module.Deps{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(failingWriter{err: errWriteFailed})
	cmd.SetArgs([]string{"--json", "--timeout", "1ns", dir})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "docs check: encode deadline error") {
		t.Errorf("err = %q, want it to name docs check", err.Error())
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

// A tree whose only finding is a deprecated page is informational: exit 0,
// the finding still on stdout, and its JSON severity "info".
func TestDocsCheckCmd_DeprecatedOnlyExitsZero(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[old](old.md)\n")
	docsCheckWrite(t, filepath.Join(dir, "old.md"), "---\nstatus: deprecated\n---\n# Old\n")

	stdout, stderr, code := runDocsCheck(t, "--json", dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 for a deprecated-only tree (stdout %q)", code, stdout)
	}
	var got struct {
		Findings []struct {
			Kind     string `json:"kind"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(got.Findings) != 1 || got.Findings[0].Kind != "deprecated" || got.Findings[0].Severity != "info" {
		t.Errorf("findings = %+v, want one deprecated with severity info", got.Findings)
	}
	if !strings.Contains(stderr, "1 informational finding(s), no errors") {
		t.Errorf("stderr = %q, want the informational summary", stderr)
	}
}

// A deprecated page beside an error finding still exits 1, and the summary
// separates the informational count.
func TestDocsCheckCmd_DeprecatedPlusErrorExits1(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[old](old.md)\n[x](gone.md)\n")
	docsCheckWrite(t, filepath.Join(dir, "old.md"), "---\nstatus: deprecated\n---\n# Old\n")

	stdout, _, code := runDocsCheck(t, dir)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, stdout)
	}
	if !strings.Contains(stdout, "old.md: deprecated") || !strings.Contains(stdout, "broken_link") {
		t.Errorf("stdout = %q, want both the deprecated and broken_link lines", stdout)
	}
	// The error text carries the summary; ExitCode wraps it, so read it back.
	cmd := newDocsCheckCmd(module.Deps{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{dir})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "2 finding(s), 1 informational") {
		t.Errorf("err = %v, want %q", err, "docs check: 2 finding(s), 1 informational")
	}
}
