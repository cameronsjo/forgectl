package cli

// Test plan for list_bound.go and the verbs that use it (#1084)
//
//   [x] review --json with no flag stays the bare array (ADR-0008 additive
//       rule): same rows, no envelope, every row
//   [x] review --json --limit N emits {items,total,shown,limit,truncated,hint,
//       notes}; truncated is true only when rows were dropped; the stderr
//       "results may be truncated" note rides in notes
//   [x] review --json --fields keeps only the named keys, in the order given;
//       an unknown field is an error naming the valid ones
//   [x] review table caps at the default and says so on stderr; --limit 0 is
//       every row; a negative --limit is an error naming the fix
//   [x] projects list --json: same bare-array / envelope / fields contract
//   [x] status --json --limit cuts each list and reports it under "bound";
//       the default document has no "bound" key
//   [x] menu <group> keeps only that group's rows; an unknown group lists the
//       valid ones

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/review"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// compactJSON strips the indentation the verbs encode with.
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// manyReviewItems returns n issues in one repo, numbered 1..n.
func manyReviewItems(n int) []review.Item {
	items := make([]review.Item, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, reviewItem(review.KindIssue, "forgectl", i))
	}
	return items
}

// runReview executes `review args...` over one fake source and returns stdout,
// stderr and the RunE error.
func runReview(t *testing.T, src fakeReviewSource, args ...string) (string, string, error) {
	t.Helper()
	cmd := newReviewCmdForSources([]review.Source{src}, "", review.GitHubHost, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestReviewJSON_NoLimitIsTheBareArrayWithEveryRow(t *testing.T) {
	out, _, err := runReview(t, fakeReviewSource{items: manyReviewItems(150)}, "--json")
	if err != nil {
		t.Fatalf("review --json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("default --json is no longer a bare array: %v\n%.200s", err, out)
	}
	if len(rows) != 150 {
		t.Errorf("default --json kept %d rows, want all 150 (a default cap drops rows from existing scripts)", len(rows))
	}
}

func TestReviewJSON_LimitEmitsEnvelopeAndSaysItTruncated(t *testing.T) {
	src := fakeReviewSource{
		items: manyReviewItems(12),
		notes: []string{"cameronsjo: results may be truncated at 1000"},
	}
	out, _, err := runReview(t, src, "--json", "--limit", "5")
	if err != nil {
		t.Fatalf("review --json --limit 5: %v", err)
	}
	var doc struct {
		Items     []map[string]any `json:"items"`
		Total     int              `json:"total"`
		Shown     int              `json:"shown"`
		Limit     int              `json:"limit"`
		Truncated bool             `json:"truncated"`
		Hint      string           `json:"hint"`
		Notes     []string         `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not an envelope: %v\n%.300s", err, out)
	}
	if len(doc.Items) != 5 || doc.Total != 12 || doc.Shown != 5 || doc.Limit != 5 || !doc.Truncated {
		t.Errorf("envelope = items %d total %d shown %d limit %d truncated %v; want 5/12/5/5/true",
			len(doc.Items), doc.Total, doc.Shown, doc.Limit, doc.Truncated)
	}
	if !strings.Contains(doc.Hint, "--kind") || !strings.Contains(doc.Hint, "--limit") {
		t.Errorf("hint %q names no way to narrow", doc.Hint)
	}
	if len(doc.Notes) != 1 || !strings.Contains(doc.Notes[0], "truncated at 1000") {
		t.Errorf("the upstream truncation note must ride in the JSON, got notes %q", doc.Notes)
	}
}

func TestReviewJSON_LimitAboveTotalIsNotTruncated(t *testing.T) {
	out, _, err := runReview(t, fakeReviewSource{items: manyReviewItems(3)}, "--json", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Items     []any  `json:"items"`
		Truncated bool   `json:"truncated"`
		Hint      string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) != 3 || doc.Truncated || doc.Hint != "" {
		t.Errorf("3 rows under limit 10 = items %d truncated %v hint %q; want 3/false/empty", len(doc.Items), doc.Truncated, doc.Hint)
	}
}

func TestReviewJSON_FieldsKeepsNamedKeysInOrder(t *testing.T) {
	out, _, err := runReview(t, fakeReviewSource{items: manyReviewItems(2)}, "--json", "--fields", "repo,number")
	if err != nil {
		t.Fatal(err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("--fields alone must stay a bare array: %v\n%s", err, out)
	}
	if got, want := compactJSON(t, rows[0]), `{"repo":"cameronsjo/forgectl","number":1}`; got != want {
		t.Errorf("row = %s, want %s", got, want)
	}
}

func TestReviewJSON_UnknownFieldNamesTheValidOnes(t *testing.T) {
	_, _, err := runReview(t, fakeReviewSource{items: manyReviewItems(1)}, "--json", "--fields", "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown --fields") || !strings.Contains(err.Error(), "isDraft") {
		t.Errorf("err = %v; want 'unknown --fields' naming the valid fields", err)
	}
	_, _, err = runReview(t, fakeReviewSource{}, "--fields", "repo")
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("--fields without --json: err = %v; want it to say add --json", err)
	}
}

func TestReviewTable_CapsAtTheDefaultAndSaysSo(t *testing.T) {
	src := fakeReviewSource{items: manyReviewItems(humanListLimit + 20)}
	out, errOut, err := runReview(t, src)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimRight(out, "\n"), "\n"); got != humanListLimit { // header + N rows = N newlines
		t.Errorf("table has %d data rows, want %d", got, humanListLimit)
	}
	want := fmt.Sprintf("showing %d of %d", humanListLimit, humanListLimit+20)
	if !strings.Contains(errOut, want) || !strings.Contains(errOut, "--kind, --repo") {
		t.Errorf("stderr %q lacks %q and a narrowing hint", errOut, want)
	}

	all, allErr, err := runReview(t, src, "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimRight(all, "\n"), "\n"); got != humanListLimit+20 {
		t.Errorf("--limit 0 table has %d rows, want every one (%d)", got, humanListLimit+20)
	}
	if strings.Contains(allErr, "showing") {
		t.Errorf("--limit 0 must not report a cut: %q", allErr)
	}
}

func TestReviewLimit_NegativeNamesTheFix(t *testing.T) {
	_, _, err := runReview(t, fakeReviewSource{}, "--limit", "-1")
	if err == nil || !strings.Contains(err.Error(), "0 for every row") {
		t.Errorf("err = %v; want a message naming 0 as the way to ask for every row", err)
	}
}

// ---- projects list --------------------------------------------------------

func manyRepoJSON(n int) string {
	var parts []string
	for i := 1; i <= n; i++ {
		parts = append(parts, fmt.Sprintf(`{"name":"repo-%03d","sshUrl":"git@github.com:cameronsjo/repo-%03d.git","isPrivate":false}`, i, i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func runProjectsList(t *testing.T, n int, args ...string) (string, string, error) {
	t.Helper()
	client := listFixture(t, twoHostRunFunc(manyRepoJSON(n), "owner\tname\ttype\tssh\n"))
	cmd := newProjectsListCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestProjectsListJSON_NoLimitIsTheBareArrayWithEveryRow(t *testing.T) {
	out, _, err := runProjectsList(t, 30, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var repos []projects.Repo
	if err := json.Unmarshal([]byte(out), &repos); err != nil {
		t.Fatalf("default --json is no longer a bare array: %v", err)
	}
	if len(repos) != 30 {
		t.Errorf("default --json kept %d rows, want 30", len(repos))
	}
}

func TestProjectsListJSON_LimitEnvelopeAndFields(t *testing.T) {
	out, _, err := runProjectsList(t, 30, "--json", "--limit", "7", "--fields", "name,cloned")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Items     []json.RawMessage `json:"items"`
		Total     int               `json:"total"`
		Shown     int               `json:"shown"`
		Truncated bool              `json:"truncated"`
		Hint      string            `json:"hint"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not an envelope: %v\n%.300s", err, out)
	}
	if len(doc.Items) != 7 || doc.Total != 30 || doc.Shown != 7 || !doc.Truncated {
		t.Errorf("envelope = items %d total %d shown %d truncated %v; want 7/30/7/true", len(doc.Items), doc.Total, doc.Shown, doc.Truncated)
	}
	if !strings.Contains(doc.Hint, "--host") {
		t.Errorf("hint %q names no way to narrow", doc.Hint)
	}
	if got := compactJSON(t, doc.Items[0]); got != `{"name":"repo-001","cloned":false}` {
		t.Errorf("first projected row = %s", got)
	}
}

func TestProjectsListTable_CapsAtTheDefaultAndSaysSo(t *testing.T) {
	out, errOut, err := runProjectsList(t, humanListLimit+5)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimRight(out, "\n"), "\n"); got != humanListLimit {
		t.Errorf("table has %d data rows, want %d", got, humanListLimit)
	}
	if !strings.Contains(errOut, fmt.Sprintf("showing %d of %d", humanListLimit, humanListLimit+5)) {
		t.Errorf("stderr lacks the cut report: %q", errOut)
	}
}

// ---- status ---------------------------------------------------------------

func manyStatusSources(n int) statusSources {
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		found := make([]projects.Project, 0, n)
		for i := 0; i < n; i++ {
			found = append(found, projects.Project{Name: fmt.Sprintf("p%02d", i), Dir: fmt.Sprintf("/p/p%02d", i), Status: projects.GitStatus{State: projects.StatusOK}})
		}
		return newStatusGit("/p", found), nil, nil
	}
	return src
}

func TestStatusJSON_LimitCutsListsAndReportsItUnderBound(t *testing.T) {
	out, _, err := runStatus(t, manyStatusSources(9), "--json", "--limit", "4")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Git struct {
			Data struct {
				Total    int               `json:"total"`
				Projects []json.RawMessage `json:"projects"`
			} `json:"data"`
		} `json:"git"`
		Bound struct {
			Limit     int  `json:"limit"`
			Truncated bool `json:"truncated"`
			Cut       []struct {
				List  string `json:"list"`
				Total int    `json:"total"`
				Shown int    `json:"shown"`
			} `json:"cut"`
			Hint string `json:"hint"`
		} `json:"bound"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Git.Data.Projects) != 4 || doc.Git.Data.Total != 9 {
		t.Errorf("git projects kept %d (total %d), want 4 of 9", len(doc.Git.Data.Projects), doc.Git.Data.Total)
	}
	if !doc.Bound.Truncated || doc.Bound.Limit != 4 || len(doc.Bound.Cut) != 1 ||
		doc.Bound.Cut[0].List != "git.projects" || doc.Bound.Cut[0].Total != 9 || doc.Bound.Cut[0].Shown != 4 || doc.Bound.Hint == "" {
		t.Errorf("bound = %+v; want one cut of git.projects 9 -> 4 with a hint", doc.Bound)
	}
}

func TestStatusJSON_DefaultHasNoBoundKeyAndEveryRow(t *testing.T) {
	out, _, err := runStatus(t, manyStatusSources(9), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["bound"]; ok {
		t.Error("the default status --json grew a \"bound\" key")
	}
	if n := strings.Count(out, `"/p/p`); n != 9 {
		t.Errorf("default status --json lists %d projects, want all 9", n)
	}
}

func TestStatusLimit_WithoutJSONNamesTheFix(t *testing.T) {
	_, _, err := runStatus(t, okStatusSources(), "--limit", "3")
	if err == nil || !strings.Contains(err.Error(), "add --json") {
		t.Errorf("err = %v; want it to say add --json", err)
	}
}

// ---- menu -----------------------------------------------------------------

func TestMenuGroup_KeepsOnlyThatGroupAndUnknownListsTheValidOnes(t *testing.T) {
	isolateJSONContractEnv(t)
	runner := &exec.FakeRunner{}
	root := productionJSONRoot(runner)
	full, _, err := runJSONThroughFang(t, root, "menu", "--json")
	if err != nil {
		t.Fatalf("menu --json: %v", err)
	}
	scoped, stderr, err := runJSONThroughFang(t, productionJSONRoot(runner), "menu", "--json", "review")
	if err != nil {
		t.Fatalf("menu --json review: %v (stderr %q)", err, stderr)
	}
	var doc menuJSON
	if err := json.Unmarshal([]byte(scoped), &doc); err != nil {
		t.Fatalf("decode: %v\n%.300s", err, scoped)
	}
	if len(doc.Commands) == 0 {
		t.Fatal("menu --json review kept no command rows")
	}
	for _, list := range [][]menuRowJSON{doc.Pinned, doc.Recent, doc.Commands} {
		for _, r := range list {
			if len(r.Argv) == 0 || r.Argv[0] != "review" {
				t.Errorf("row %q is outside the review group", r.Command)
			}
		}
	}
	if len(scoped) >= len(full)/2 {
		t.Errorf("scoped menu is %d bytes against %d for the whole hub; a group must cut it down", len(scoped), len(full))
	}

	// A pinned command is a group too: "pr" appears only under pinned.
	pinned, _, err := runJSONThroughFang(t, productionJSONRoot(runner), "menu", "--json", "pr")
	if err != nil {
		t.Fatalf("menu --json pr: %v", err)
	}
	var pdoc menuJSON
	if err := json.Unmarshal([]byte(pinned), &pdoc); err != nil || len(pdoc.Pinned) != 1 || pdoc.Pinned[0].Command != "pr" {
		t.Errorf("menu --json pr pinned = %+v (err %v); want exactly the pr row", pdoc.Pinned, err)
	}

	// Under --json the failure is one JSON object on stderr, not a returned error.
	_, bad, _ := runJSONThroughFang(t, productionJSONRoot(runner), "menu", "--json", "nosuchgroup")
	if !strings.Contains(bad, "unknown menu group") || !strings.Contains(bad, "valid:") ||
		!strings.Contains(bad, "pr,") || !strings.Contains(bad, "review") {
		t.Errorf("stderr = %q; want 'unknown menu group' listing pinned and grouped command names", bad)
	}
}
