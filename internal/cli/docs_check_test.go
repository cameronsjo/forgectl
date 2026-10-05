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
//   [x] Happy: a vault-only run is checked (exit 1 on a broken wikilink, 0 clean), no stderr skip note
//   [x] Unhappy: an unknown flag exits 2
//   [x] Unhappy: control characters in a path never reach stdout raw
//   [x] Unhappy: a passed stale_after exits 1 with one stale line carrying the
//       value; a future one exits 0 silent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
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
	assertKeys(t, "top-level", got, "schema_version", "roots", "findings", "summary", "skipped")
	if v, _ := got["schema_version"].(float64); v != 1 {
		t.Errorf("schema_version = %v, want 1", got["schema_version"])
	}
	assertKeys(t, "summary", got["summary"].(map[string]any),
		"broken_links", "ambiguous_links", "broken_anchors", "orphans", "ignored_orphans", "outside_root_links",
		"deprecated", "stale", "unchecked_anchors")

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
			assertKeys(t, "vault root", r, "label", "kind", "checked", "docs")
			if r["checked"] != true {
				t.Errorf("vault root = %v, want checked", r)
			}
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

func TestDocsCheckCmd_OnlyVaultRootIsChecked(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	docsCheckWrite(t, filepath.Join(vault, "n.md"), "# N\n\n[[missing]]\n[[m#^blk]]\n")
	docsCheckWrite(t, filepath.Join(vault, "m.md"), "# M\n")

	stdout, stderr, code := runDocsCheck(t, vault)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (broken wikilink)", code)
	}
	for _, w := range []string{"n.md:3: broken_link missing", "n.md:4: broken_anchor m#^blk"} {
		if !strings.Contains(stdout, w) {
			t.Errorf("stdout = %q, want %q", stdout, w)
		}
	}
	if strings.Contains(stdout, "orphan") || strings.Contains(stderr, "skipping vault root") {
		t.Errorf("stdout %q / stderr %q: a vault reports no orphans and no skip note", stdout, stderr)
	}

	// A clean vault exits 0.
	clean := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clean, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	docsCheckWrite(t, filepath.Join(clean, "a.md"), "# A\n\n[[b]]\n")
	docsCheckWrite(t, filepath.Join(clean, "b.md"), "# B\n")
	if _, _, code := runDocsCheck(t, clean); code != 0 {
		t.Errorf("clean vault exit = %d, want 0", code)
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
	if !strings.HasPrefix(err.Error(), "docs check: encode error") {
		t.Errorf("err = %q, want it to name docs check", err.Error())
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

// Test plan for partial-tree handling (#568)
//   [x] Unhappy: a skipped path makes docs check exit 2 and list it (human)
//   [x] Unhappy: --json carries the skipped array and still exits 2
//   [x] Happy: a clean tree's --json carries an empty skipped array
//   [x] Unhappy: docs list, a tolerant verb, prints the note on stderr and exits 0
//   [x] Unhappy: --json exit 2 is a silent error; reasons carry no path
//   [x] Unhappy: a skip plus an error finding exits 2, summary counts both
//   [x] Unhappy: docs list --json keeps stderr clean (#649)
//   [x] Unhappy: docs read and docs serve print the note too
// The fault is injected through docspkg.InjectWalkFaultForTest, so these run
// as root, where chmod cannot make a directory unreadable.

func TestDocsCheckCmd_SkippedPathExits2AndListsIt(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	stdout, _, code := runDocsCheck(t, dir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (a partial tree cannot be vouched for)", code)
	}
	if !strings.Contains(stdout, "/locked: skipped (") || !strings.Contains(stdout, "/gone.md: skipped (") {
		t.Errorf("skipped paths not listed on stdout:\n%s", stdout)
	}
}

func TestDocsCheckCmd_SkippedPathJSONField(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	stdout, stderr, code := runDocsCheck(t, "--json", dir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stderr != "" {
		t.Errorf("a partial tree is a finding-level exit 2: the report is on stdout, so no stderr error object; got %q", stderr)
	}
	var got struct {
		Skipped []map[string]any `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped = %v, want 2 entries", got.Skipped)
	}
	assertKeys(t, "skipped entry", got.Skipped[0], "root", "path", "reason")
	// The reason is the bare cause: the entry already names root + relative
	// path, so the absolute path must not ride along in reason.
	for _, sp := range got.Skipped {
		if r, _ := sp["reason"].(string); r == "" || strings.Contains(r, dir) || strings.Contains(r, "/") {
			t.Errorf("reason = %q, want the bare cause with no path", r)
		}
	}
}

// Under --json the partial-tree exit 2 is silent (the report is the whole
// answer): the returned error renders nothing, so Execute prints no
// "Error: ..." line after the JSON.
func TestDocsCheckCmd_SkippedPathJSONReturnsSilentError(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	cmd := newDocsCheckCmd(module.Deps{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json", dir})
	err := cmd.ExecuteContext(context.Background())
	var silent *silentCodedError
	if !errors.As(err, &silent) || err.Error() != "" || ExitCode(err) != 2 {
		t.Fatalf("err = %#v (%q), want a silent exit-2 error", err, err)
	}
}

// Exit 2 (partial tree) outranks exit 1 (error findings), and the human
// summary still counts the error findings the report printed.
func TestDocsCheckCmd_SkippedPathOutranksErrorFinding(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n\n[x](missing.md)\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	cmd := newDocsCheckCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{dir})
	err := cmd.ExecuteContext(context.Background())
	if code := ExitCode(err); code != 2 {
		t.Fatalf("exit = %d (err %v), want 2: a partial tree outranks error findings", code, err)
	}
	if !strings.Contains(stdout.String(), "broken_link missing.md") {
		t.Errorf("the error finding must still be reported:\n%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "could not be read") || !strings.Contains(err.Error(), "1 error finding(s)") {
		t.Errorf("summary = %q, want the skip count and the error-finding count", err.Error())
	}
}

// Under --json the tolerant list keeps stderr clear of the plain-text note
// (#649: stderr carries at most one JSON object) and still exits 0.
func TestDocsListCmd_SkippedPathJSONKeepsStderrClean(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	cmd := newDocsListCmd(module.Deps{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", dir})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("docs list --json: %v", err)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty under --json", stderr.String())
	}
	var docs []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &docs); err != nil || len(docs) != 1 {
		t.Errorf("stdout = %s (err %v), want a one-entry array", stdout.String(), err)
	}
}

func TestDocsReadCmd_SkippedPathPrintsStderrNote(t *testing.T) {
	docsReadFixture(t)
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	res := runDocsReadForTest(t, "README.md")
	if res.err != nil {
		t.Fatalf("docs read: %v", res.err)
	}
	if !strings.Contains(res.stderr, "unreadable path(s) under") {
		t.Errorf("stderr note missing: %q", res.stderr)
	}
}

func TestDocsServeCmd_SkippedPathPrintsStderrNote(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	cmd := newDocsServeCmd(module.Deps{})
	var stderr bytes.Buffer
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(&stderr)
	// A relative --token-file fails in runDocsServe before any bind, after
	// the note has been printed.
	cmd.SetArgs([]string{"--token-file", "relative-token", dir})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected the token-file error")
	}
	if !strings.Contains(stderr.String(), "unreadable path(s) under") {
		t.Errorf("stderr note missing: %q", stderr.String())
	}
}

func TestDocsCheckCmd_CleanTreeJSONSkippedIsEmptyArray(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	stdout, _, code := runDocsCheck(t, "--json", dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"skipped":[]`) {
		t.Errorf("clean report must carry an empty skipped array:\n%s", stdout)
	}
}

func TestDocsListCmd_SkippedPathPrintsStderrNoteAndStaysTolerant(t *testing.T) {
	dir := t.TempDir()
	docsCheckWrite(t, filepath.Join(dir, "README.md"), "# R\n")
	t.Cleanup(docspkg.InjectWalkFaultForTest())

	cmd := newDocsListCmd(module.Deps{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{dir})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("docs list must stay tolerant: %v", err)
	}
	if !strings.Contains(stderr.String(), "skipped 2 unreadable path(s) under") ||
		!strings.Contains(stderr.String(), "(see docs check)") {
		t.Errorf("stderr note missing: %q", stderr.String())
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
	// Under --json stderr is reserved for the one error object (#649); an
	// exit-0 run writes nothing to it (#672).
	if stderr != "" {
		t.Errorf("stderr = %q, want empty under --json", stderr)
	}
	// Human mode keeps the one-line summary.
	_, humanStderr, humanCode := runDocsCheck(t, dir)
	if humanCode != 0 || !strings.Contains(humanStderr, "1 informational finding(s), no errors") {
		t.Errorf("human: exit %d, stderr %q, want the informational summary", humanCode, humanStderr)
	}
}

// Under --json a vault-only run emits the report on stdout, roots[] marks the
// vault checked, and stderr stays empty.
func TestDocsCheckCmd_OnlyVaultRoot_JSON(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	docsCheckWrite(t, filepath.Join(vault, "n.md"), "# N\n\n[[missing]]\n")

	stdout, stderr, code := runDocsCheck(t, "--json", vault)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty under --json", stderr)
	}
	var rep struct {
		Roots []struct {
			Kind    string `json:"kind"`
			Checked bool   `json:"checked"`
		} `json:"roots"`
		Findings []struct {
			Kind string `json:"kind"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if len(rep.Roots) != 1 || rep.Roots[0].Kind != "vault" || !rep.Roots[0].Checked {
		t.Errorf("roots = %+v, want one checked vault", rep.Roots)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != "broken_link" {
		t.Errorf("findings = %+v, want one broken_link", rep.Findings)
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
