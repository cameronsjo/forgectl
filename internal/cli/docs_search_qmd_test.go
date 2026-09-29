package cli

// Test plan for the qmd backend selection in docs_search.go
//
// newDocsSearchCmd --backend / [docs] search_backend (Classification: cobra
// command, opt-in backend switch)
//   [x] Happy: with neither flag nor config, rg runs even when qmd resolves
//       (no auto-selection)
//   [x] Happy: --backend qmd runs qmd and reports backend "qmd"
//   [x] Happy: [docs] search_backend = "qmd" runs qmd; --backend ripgrep
//       overrides it
//   [x] Unhappy: a bad --backend, and a bad search_backend config value,
//       exit 2 with one JSON error object before anything runs
//   [x] Happy: the truncation hint holds when qmd's full window leaves fewer
//       than --limit results, or none
//   [x] Unhappy: qmd output that is not one JSON array exits 2 under
//       --json with stdout empty and one error object on stderr

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// bothBackends resolves rg and qmd alike, so only the selection decides.
func bothBackends(name string) (string, error) { return "/usr/bin/" + name, nil }

func qmdHitJSON(t *testing.T, file string) string {
	t.Helper()
	b, err := json.Marshal([]map[string]any{{"score": 1, "file": file, "line": 2, "title": "t", "snippet": "@@ -1,2 @@ (0 before, 0 after)\nneedle here"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func runDocsSearchCfg(t *testing.T, cfg config.DocsConfig, runner *searchRunner, args ...string) (string, string, error) {
	t.Helper()
	cmd := newDocsSearchCmd(module.Deps{Runner: runner, Cfg: config.Config{Docs: cfg}})
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

func TestDocsSearchDefaultsToRipgrepEvenWithQMDInstalled(t *testing.T) {
	docsSearchFixture(t)
	stubSearchLookPath(t, bothBackends)
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}}

	stdout, _, err := runDocsSearch(t, runner, "--json", "needle")
	if err != nil {
		t.Fatalf("docs search: %v", err)
	}
	if runner.name != "/usr/bin/rg" || !strings.Contains(stdout, `"backend":"ripgrep"`) {
		t.Errorf("ran %q, stdout %s; want rg and backend ripgrep", runner.name, stdout)
	}
}

func TestDocsSearchBackendQMDFlag(t *testing.T) {
	page := docsSearchFixture(t)
	stubSearchLookPath(t, bothBackends)
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}, stdout: qmdHitJSON(t, page)}

	stdout, _, err := runDocsSearch(t, runner, "--json", "--backend", "qmd", "needle")
	if err != nil {
		t.Fatalf("docs search: %v", err)
	}
	if runner.name != "/usr/bin/qmd" || len(runner.args) == 0 || runner.args[0] != "search" {
		t.Errorf("ran %q %q, want qmd search", runner.name, runner.args)
	}
	var resp struct {
		Backend string `json:"backend"`
		Results []struct {
			Path string `json:"path"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if resp.Backend != "qmd" || len(resp.Results) != 1 || resp.Results[0].Path != filepath.Base(page) {
		t.Errorf("response = %s, want backend qmd and one page.md hit", stdout)
	}
}

func TestDocsSearchBackendFromConfig(t *testing.T) {
	page := docsSearchFixture(t)
	stubSearchLookPath(t, bothBackends)
	cfg := config.DocsConfig{SearchBackend: "qmd"}

	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}, stdout: qmdHitJSON(t, page)}
	if _, _, err := runDocsSearchCfg(t, cfg, runner, "needle"); err != nil {
		t.Fatalf("docs search: %v", err)
	}
	if runner.name != "/usr/bin/qmd" {
		t.Errorf("config search_backend=qmd ran %q, want qmd", runner.name)
	}
}

func TestDocsSearchBackendFlagOverridesConfig(t *testing.T) {
	docsSearchFixture(t)
	stubSearchLookPath(t, bothBackends)
	cfg := config.DocsConfig{SearchBackend: "qmd"}

	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}}
	stdout, _, err := runDocsSearchCfg(t, cfg, runner, "--json", "--backend", "ripgrep", "needle")
	if err != nil {
		t.Fatalf("docs search: %v", err)
	}
	if runner.name != "/usr/bin/rg" || !strings.Contains(stdout, `"backend":"ripgrep"`) {
		t.Errorf("--backend ripgrep over config qmd ran %q, stdout %s; want rg and backend ripgrep", runner.name, stdout)
	}
}

// qmdWindowJSON is a full qmd -n window for --limit 1 (10 rows): inRoot rows
// naming page, the rest outside every root.
func qmdWindowJSON(t *testing.T, page string, inRoot int) string {
	t.Helper()
	rows := make([]map[string]any, 0, 10)
	for i := range 10 {
		file := fmt.Sprintf("/elsewhere/other-%d.md", i)
		if i < inRoot {
			file = page
		}
		rows = append(rows, map[string]any{"score": 1, "file": file, "line": i + 1, "snippet": "needle"})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Under qmd, truncated can be set with fewer than --limit results shown, so
// the human hint must not claim a count of results it did not show.
func TestDocsSearchQMDTruncationHint(t *testing.T) {
	for name, tc := range map[string]struct {
		inRoot    int
		wantLines int
		wantNoHit bool
	}{
		"fewer than limit survive": {inRoot: 1, wantLines: 1},
		"none survive":             {inRoot: 0, wantLines: 0, wantNoHit: true},
	} {
		t.Run(name, func(t *testing.T) {
			page := docsSearchFixture(t)
			stubSearchLookPath(t, bothBackends)
			runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}, stdout: qmdWindowJSON(t, page, tc.inRoot)}
			stdout, stderr, err := runDocsSearch(t, runner, "--backend", "qmd", "--limit", "1", "needle")
			if err != nil {
				t.Fatalf("docs search: %v", err)
			}
			if got := strings.Count(stdout, "\n"); got != tc.wantLines {
				t.Errorf("stdout has %d lines, want %d: %q", got, tc.wantLines, stdout)
			}
			if !strings.Contains(stderr, "more matches may exist") {
				t.Errorf("stderr = %q, want the more-matches hint", stderr)
			}
			if strings.Contains(stderr, "first") {
				t.Errorf("stderr = %q claims a count of results shown", stderr)
			}
			if tc.wantNoHit != strings.Contains(stderr, "no matches") {
				t.Errorf("stderr = %q, want \"no matches\" = %v", stderr, tc.wantNoHit)
			}
		})
	}
}

func TestDocsSearchBadBackendExits2(t *testing.T) {
	docsSearchFixture(t)
	stubSearchLookPath(t, bothBackends)
	for name, tc := range map[string]struct {
		cfg  config.DocsConfig
		args []string
		want string
	}{
		"flag":   {args: []string{"--json", "--backend", "elastic", "needle"}, want: "--backend"},
		"config": {cfg: config.DocsConfig{SearchBackend: "elastic"}, args: []string{"--json", "needle"}, want: "search_backend"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}}
			stdout, stderr, err := runDocsSearchCfg(t, tc.cfg, runner, tc.args...)
			assertOneDocsJSONError(t, stdout, stderr, err, tc.want)
			if runner.calls != 0 {
				t.Errorf("a backend ran %d times", runner.calls)
			}
		})
	}
}

func TestDocsSearchQMDImpureOutputExits2(t *testing.T) {
	page := docsSearchFixture(t)
	stubSearchLookPath(t, bothBackends)
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}, stdout: "Loading models...\n" + qmdHitJSON(t, page)}

	stdout, stderr, err := runDocsSearch(t, runner, "--json", "--backend", "qmd", "needle")
	assertOneDocsJSONError(t, stdout, stderr, err, "not a JSON array")
}

// assertOneDocsJSONError checks the shared "could not run" contract: exit 2,
// stdout empty, exactly one {"error","code","root"} object on stderr whose
// error names want.
func assertOneDocsJSONError(t *testing.T, stdout, stderr string, err error, want string) {
	t.Helper()
	if code := ExitCode(err); code != 2 {
		t.Errorf("exit code = %d (err %v), want 2", code, err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stderr))
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("stderr is not a JSON object: %v\n%s", err, stderr)
	}
	msg, _ := obj["error"].(string)
	if _, ok := obj["root"]; !ok || obj["code"] != float64(2) || !strings.Contains(msg, want) {
		t.Errorf("error object = %v, want code 2, a root key, and an error naming %q", obj, want)
	}
	if dec.More() {
		t.Errorf("stderr holds more than one JSON value: %s", stderr)
	}
}
