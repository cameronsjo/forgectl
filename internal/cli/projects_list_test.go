package cli

// Test plan for projects_list.go
//
// filterRepos (Classification: pure logic / data transformer)
//   [x] Happy: no filters return input unchanged
//   [x] Happy: host filter keeps only matching-host repos
//   [x] Happy: query is a case-insensitive substring match on Name
//   [x] Happy: host + query combined narrows to their intersection
//   [x] Boundary: nil input returns empty without panic
//   [x] Boundary: query with zero matches returns empty slice
//
// renderRepoTable (Classification: data transformer + I/O boundary to io.Writer)
//   [x] Happy: empty repo list writes header + "0 projects" summary to stderr writer
//   [x] Happy: cloned-clean repo — STATUS column is "clean" (Label brackets trimmed)
//   [x] Happy: uncloned repo — STATUS column is "uncloned"
//   [x] Happy: repo with Owner set — REPO column is "owner/name"
//   [x] Happy: mirror repo — REPO column ends with "(mirror)"
//   [x] Happy: empty Host — HOST column shows "local"
//   [x] Happy: count summary (N projects, M cloned, K remote-only) on stderr writer
//
// newProjectsListCmd (Classification: API handler / cobra command)
//   [x] Unhappy: --host with unrecognised value returns error
//   [x] Happy: --json emits valid JSON array to stdout; count note on stderr
//   [x] Happy: --json on empty result emits [] not null
//   [x] Happy: --host github filters table to github rows only
//   [x] Happy: positional query arg filters table by name substring
//   [x] Happy: degradation notes from Inventory appear on stderr, not stdout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// ---- filterRepos -----------------------------------------------------------

func TestFilterRepos_NoFilters_ReturnsAllUnchanged(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github", Name: "alpha"},
		{Host: "gitea", Name: "beta"},
	}
	got := filterRepos(repos, "", "")
	if len(got) != 2 {
		t.Errorf("no-filter: got %d repos, want 2", len(got))
	}
}

func TestFilterRepos_HostFilter_KeepsOnlyMatchingHost(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github", Name: "alpha"},
		{Host: "gitea", Name: "beta"},
		{Host: "github", Name: "gamma"},
	}
	got := filterRepos(repos, "github", "")
	if len(got) != 2 {
		t.Fatalf("host=github: got %d repos, want 2", len(got))
	}
	for _, r := range got {
		if r.Host != "github" {
			t.Errorf("non-github host %q survived host filter", r.Host)
		}
	}
}

func TestFilterRepos_QueryIsCaseInsensitiveSubstring(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github", Name: "ForgeCTL"},
		{Host: "gitea", Name: "homeclaw"},
		{Host: "github", Name: "other"},
	}
	// "forge" must match "ForgeCTL" case-insensitively.
	got := filterRepos(repos, "", "forge")
	if len(got) != 1 || got[0].Name != "ForgeCTL" {
		t.Errorf("query=forge: got %+v; want only ForgeCTL", got)
	}
}

func TestFilterRepos_HostAndQueryCombined(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github", Name: "forgectl"},
		{Host: "gitea", Name: "forgectl"}, // same name, different host
		{Host: "github", Name: "other"},
	}
	got := filterRepos(repos, "github", "forge")
	if len(got) != 1 || got[0].Host != "github" {
		t.Errorf("host=github,query=forge: got %+v; want only github/forgectl", got)
	}
}

func TestFilterRepos_NilInput_ReturnsEmpty(t *testing.T) {
	got := filterRepos(nil, "github", "forge")
	if len(got) != 0 {
		t.Errorf("nil input: got %d repos, want 0", len(got))
	}
}

func TestFilterRepos_QueryNoMatch_ReturnsEmpty(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github", Name: "alpha"},
	}
	got := filterRepos(repos, "", "zzzzz")
	if len(got) != 0 {
		t.Errorf("no-match query: got %+v, want empty", got)
	}
}

// ---- renderRepoTable -------------------------------------------------------

func TestRenderRepoTable_EmptyList_WritesHeaderAndZeroCount(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := renderRepoTable(&out, &errOut, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "HOST") {
		t.Errorf("header missing: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "0 projects") {
		t.Errorf("zero-count summary missing from stderr writer: %q", errOut.String())
	}
}

func TestRenderRepoTable_ClonedCleanRepo_StatusIsClean(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "github", Owner: "cameronsjo", Name: "forgectl", Cloned: true},
		// Status is zero-value: Label()="[clean]"; Trim("[]")="clean".
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "clean") {
		t.Errorf("cloned-clean repo: want 'clean' in STATUS column, got: %q", out.String())
	}
}

func TestRenderRepoTable_UnclonedRepo_StatusIsUncloned(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "github", Owner: "cameronsjo", Name: "newrepo", Cloned: false},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "uncloned") {
		t.Errorf("uncloned repo: want 'uncloned' in STATUS column, got: %q", out.String())
	}
}

func TestRenderRepoTable_RepoWithOwner_ShowsOwnerSlashName(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "github", Owner: "cameronsjo", Name: "forgectl", Cloned: true},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "cameronsjo/forgectl") {
		t.Errorf("want 'cameronsjo/forgectl' in REPO column, got: %q", out.String())
	}
}

func TestRenderRepoTable_MirrorRepo_HasMirrorSuffix(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "gitea", Owner: "cameron", Name: "upstream", Mirror: true},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "(mirror)") {
		t.Errorf("mirror repo: want '(mirror)' suffix in REPO column, got: %q", out.String())
	}
}

func TestRenderRepoTable_EmptyHost_ShowsLocal(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "", Name: "scratch", Cloned: true},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "local") {
		t.Errorf("empty-host repo: want 'local' in HOST column, got: %q", out.String())
	}
}

func TestRenderRepoTable_CountSummaryMatchesCounts(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "github", Name: "cloned1", Cloned: true},
		{Host: "github", Name: "cloned2", Cloned: true},
		{Host: "gitea", Name: "remote1", Cloned: false},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	summary := errOut.String()
	if !strings.Contains(summary, "3 projects") {
		t.Errorf("want '3 projects' in count summary, got: %q", summary)
	}
	if !strings.Contains(summary, "2 cloned") {
		t.Errorf("want '2 cloned' in count summary, got: %q", summary)
	}
	if !strings.Contains(summary, "1 remote-only") {
		t.Errorf("want '1 remote-only' in count summary, got: %q", summary)
	}
}

// ---- newProjectsListCmd (cobra command integration) ------------------------

// listFixture builds a *projects.Client whose Inventory returns repos driven
// entirely by the provided RunFunc. PROJECTS_DIR is set to an empty temp dir
// so localRepos contributes nothing — the test controls all output via gh/tea.
func listFixture(t *testing.T, runFunc func(string, []string) (string, error)) *projects.Client {
	t.Helper()
	t.Setenv("PROJECTS_DIR", t.TempDir())
	fake := &exec.FakeRunner{RunFunc: runFunc}
	return projects.New(fake)
}

// twoHostRunFunc returns a RunFunc that serves ghJSON for gh calls and teaTSV
// for tea calls. All other calls (git) return ("", nil).
func twoHostRunFunc(ghJSON, teaTSV string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		switch name {
		case "gh":
			return ghJSON, nil
		case "tea":
			return teaTSV, nil
		}
		return "", nil
	}
}

func TestListCmd_InvalidHost_ReturnsError(t *testing.T) {
	client := listFixture(t, twoHostRunFunc("[]", ""))
	cmd := newProjectsListCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--host", "bitbucket"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --host bitbucket, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --host") {
		t.Errorf("error = %q; want 'invalid --host'", err.Error())
	}
}

func TestListCmd_JSONFlag_EmitsValidJSONToStdout(t *testing.T) {
	ghJSON := `[{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false}]`
	// Minimal TSV with only a header (no data rows → giteaList returns empty).
	client := listFixture(t, twoHostRunFunc(ghJSON, "owner\tname\ttype\tssh\n"))
	cmd := newProjectsListCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// stdout must be valid JSON.
	var repos []projects.Repo
	if err := json.Unmarshal(stdout.Bytes(), &repos); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout.String())
	}
	if len(repos) == 0 {
		t.Error("expected at least one repo in JSON output")
	}

	// The --json path skips renderRepoTable, so there is no count-summary line.
	// What must NOT happen: prose leaking onto stdout next to the JSON payload.
	// A bare JSON parse verifies stdout is clean; stderr may be empty (no notes).
	if strings.Contains(stdout.String(), "projects (") {
		t.Errorf("count summary prose leaked onto stdout: %q", stdout.String())
	}
}

func TestListCmd_JSONFlag_EmptyResultIsJSONArray(t *testing.T) {
	// No repos from either host — --json must emit [] not null.
	client := listFixture(t, twoHostRunFunc("[]", "owner\tname\ttype\tssh\n"))
	cmd := newProjectsListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	trimmed := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(trimmed, "[") {
		t.Errorf("--json empty: want JSON array starting with '[', got %q", trimmed)
	}
}

func TestListCmd_HostFlag_FiltersToGithubOnly(t *testing.T) {
	ghJSON := `[{"name":"gh-repo","sshUrl":"git@github.com:cameronsjo/gh-repo.git","isPrivate":false}]`
	teaTSV := "owner\tname\ttype\tssh\n" +
		"cameron\tgt-repo\tsource\tssh://git@git.sjo.lol:222/cameron/gt-repo.git\n"
	client := listFixture(t, twoHostRunFunc(ghJSON, teaTSV))
	cmd := newProjectsListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--host", "github"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := stdout.String()
	if !strings.Contains(body, "gh-repo") {
		t.Errorf("--host github: want gh-repo in output, got: %q", body)
	}
	if strings.Contains(body, "gt-repo") {
		t.Errorf("--host github: gt-repo should be filtered out, got: %q", body)
	}
}

func TestListCmd_QueryArg_FiltersByNameSubstring(t *testing.T) {
	ghJSON := `[` +
		`{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false},` +
		`{"name":"homeclaw","sshUrl":"git@github.com:cameronsjo/homeclaw.git","isPrivate":false}` +
		`]`
	client := listFixture(t, twoHostRunFunc(ghJSON, "owner\tname\ttype\tssh\n"))
	cmd := newProjectsListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"forge"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := stdout.String()
	if !strings.Contains(body, "forgectl") {
		t.Errorf("query=forge: want forgectl in output, got: %q", body)
	}
	if strings.Contains(body, "homeclaw") {
		t.Errorf("query=forge: homeclaw should be filtered out, got: %q", body)
	}
}

func TestListCmd_DegradationNotes_AppearOnStderrNotStdout(t *testing.T) {
	// github errors → note; gitea succeeds → one row in the table.
	teaTSV := "owner\tname\ttype\tssh\n" +
		"cameron\thomeclaw\tsource\tssh://git@git.sjo.lol:222/cameron/homeclaw.git\n"
	client := listFixture(t, func(name string, args []string) (string, error) {
		switch name {
		case "gh":
			return "", errors.New("gh: not authenticated")
		case "tea":
			return teaTSV, nil
		}
		return "", nil
	})
	cmd := newProjectsListCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error (degraded host must not fail the command): %v", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Errorf("degradation notes must go to stderr, not stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note:") {
		t.Errorf("degradation notes missing from stderr: %q", stderr.String())
	}
}
