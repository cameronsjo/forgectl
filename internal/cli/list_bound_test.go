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
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
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
	return runReviewAt(t, filepath.Join(t.TempDir(), "review-reviewed.json"), src, args...)
}

// runReviewAt is runReview with an explicit reviewed-store path ("" = none).
func runReviewAt(t *testing.T, reviewedPath string, src fakeReviewSource, args ...string) (string, string, error) {
	t.Helper()
	cmd := newReviewCmdForSources([]review.Source{src}, reviewedPath, review.GitHubHost, theme.Theme{})
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
	_, errOut, err := runReview(t, fakeReviewSource{items: manyReviewItems(1)}, "--json", "--fields", "nope")
	if err == nil || !strings.Contains(errOut, "unknown --fields") || !strings.Contains(errOut, "isDraft") {
		t.Errorf("err = %v, stderr %q; want 'unknown --fields' naming the valid fields", err, errOut)
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

// The notes test above counted one note; the reviewed-store path now
// contributes none because runReview supplies a real one.

func TestStatusLimit_TextViewTakesTheSameLimit(t *testing.T) {
	// Eleven dirty projects: the default text view lists 10 and says "1 more".
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		found := make([]projects.Project, 0, 11)
		for i := 0; i < 11; i++ {
			found = append(found, projects.Project{Name: fmt.Sprintf("d%02d", i), Dir: "/p", Status: projects.GitStatus{State: projects.StatusOK, Modified: 1}})
		}
		return newStatusGit("/p", found), nil, nil
	}
	rows := func(out string) int { return strings.Count(out, "[1 modified]") }
	def, _, err := runStatus(t, src)
	if err != nil {
		t.Fatal(err)
	}
	three, _, err := runStatus(t, src, "--limit", "3")
	if err != nil {
		t.Fatalf("status --limit 3 (text): %v", err)
	}
	all, _, err := runStatus(t, src, "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if rows(def) != 10 || rows(three) != 3 || rows(all) != 11 {
		t.Errorf("text rows default/3/0 = %d/%d/%d, want 10/3/11", rows(def), rows(three), rows(all))
	}
	if !strings.Contains(three, "… 8 more (status --json lists every project)") {
		t.Errorf("the hidden-rows line must count what --limit 3 hid: %q", three)
	}
}

func TestStatusLimit_TUIRefusesIt(t *testing.T) {
	_, _, err := runStatus(t, okStatusSources(), "--tui", "--limit", "3")
	if err == nil || !strings.Contains(err.Error(), "drop --limit") {
		t.Errorf("err = %v; want --tui --limit refused with a message saying to drop --limit", err)
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
	if !strings.Contains(bad, "unknown menu group") || !strings.Contains(bad, "commands:") || !strings.Contains(bad, "areas:") ||
		!strings.Contains(bad, "pr,") || !strings.Contains(bad, "review") || !strings.Contains(bad, "repos") {
		t.Errorf("stderr = %q; want 'unknown menu group' listing the command names and the area names", bad)
	}
	if !strings.Contains(bad, `"code": "usage_error"`) {
		t.Errorf("unknown menu group must carry the usage_error code like an unknown flag: %q", bad)
	}
}

// ---- the unpinned edges (coordinator round) --------------------------------

// failureCode decodes the one JSON failure object a --json verb writes to
// stderr and returns its code.
func failureCode(t *testing.T, stderr string) string {
	t.Helper()
	var o struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	// A standalone command (no silenced root) may add cobra's bare "Error:"
	// line after the object; read the first JSON value only.
	if err := json.NewDecoder(strings.NewReader(stderr)).Decode(&o); err != nil {
		t.Fatalf("stderr is not a JSON failure object: %v\n%q", err, stderr)
	}
	return o.Code
}

func TestReviewJSON_LimitEqualToTotalAndOneBelow(t *testing.T) {
	src := fakeReviewSource{items: manyReviewItems(4)}
	for _, tc := range []struct {
		limit     string
		shown     int
		truncated bool
	}{{"4", 4, false}, {"3", 3, true}, {"0", 4, false}} {
		out, _, err := runReview(t, src, "--json", "--limit", tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Shown     int  `json:"shown"`
			Total     int  `json:"total"`
			Truncated bool `json:"truncated"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("--limit %s is not an envelope: %v", tc.limit, err)
		}
		if doc.Shown != tc.shown || doc.Total != 4 || doc.Truncated != tc.truncated {
			t.Errorf("--limit %s: shown %d total %d truncated %v; want %d/4/%v", tc.limit, doc.Shown, doc.Total, doc.Truncated, tc.shown, tc.truncated)
		}
	}
}

func TestReviewJSON_TruncatedComesBeforeItems(t *testing.T) {
	out, _, err := runReview(t, fakeReviewSource{items: manyReviewItems(3)}, "--json", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if ti, ii := strings.Index(out, `"truncated"`), strings.Index(out, `"items"`); ti < 0 || ii < 0 || ti > ii {
		t.Errorf("truncated at %d, items at %d: a head -c read must see the cut before the rows", ti, ii)
	}
}

func TestReviewJSON_NoReviewedStorePathIsANote(t *testing.T) {
	out, errOut, err := runReviewAt(t, "", fakeReviewSource{items: manyReviewItems(1)}, "--json", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "reviewed-store path unavailable") {
		t.Errorf("stderr lacks the note: %q", errOut)
	}
	var doc struct {
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Notes) != 1 || !strings.Contains(doc.Notes[0], "reviewed-store path unavailable") {
		t.Errorf("notes = %q; the stderr note must also ride in the JSON", doc.Notes)
	}
}

func TestListBound_BadValuesAreUsageErrorsWithExitOne(t *testing.T) {
	// review: a negative --limit and an unknown --fields name.
	for _, args := range [][]string{
		{"--json", "--limit", "-1"},
		{"--json", "--fields", "nope"},
	} {
		cmd := newReviewCmdForSources([]review.Source{fakeReviewSource{}}, filepath.Join(t.TempDir(), "r.json"), review.GitHubHost, theme.Theme{})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		if err == nil || ExitCode(err) != 1 {
			t.Errorf("review %v: err %v exit %d, want an error with exit 1 (unchanged by this PR)", args, err, ExitCode(err))
		}
		if got := failureCode(t, stderr.String()); got != "usage_error" {
			t.Errorf("review %v: code %q, want usage_error", args, got)
		}
		_ = stdout // a standalone cobra command prints usage to stdout on error; the real root silences it
	}
	// projects list: the same two.
	for _, args := range [][]string{
		{"--json", "--limit", "-2"},
		{"--json", "--fields", "nope"},
	} {
		client := listFixture(t, twoHostRunFunc(manyRepoJSON(2), "owner\tname\ttype\tssh\n"))
		cmd := newProjectsListCmd(client)
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		if err == nil || ExitCode(err) != 1 {
			t.Errorf("projects list %v: err %v exit %d, want exit 1", args, err, ExitCode(err))
		}
		if got := failureCode(t, stderr.String()); got != "usage_error" {
			t.Errorf("projects list %v: code %q, want usage_error", args, got)
		}
	}
	// status: a negative --limit.
	cmd := newStatusCmdForSources(okStatusSources(), theme.Theme{})
	var stderr bytes.Buffer
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", "--limit", "-3"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || ExitCode(err) != 1 {
		t.Errorf("status --limit -3: err %v exit %d, want exit 1", err, ExitCode(err))
	}
	if got := failureCode(t, stderr.String()); got != "usage_error" {
		t.Errorf("status --limit -3: code %q, want usage_error", got)
	}
}

func TestProjectsListJSON_LimitEqualToTotalAndZero(t *testing.T) {
	for _, tc := range []struct {
		limit     string
		truncated bool
	}{{"6", false}, {"5", true}, {"0", false}} {
		out, _, err := runProjectsList(t, 6, "--json", "--limit", tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Truncated bool `json:"truncated"`
			Total     int  `json:"total"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("--limit %s is not an envelope: %v", tc.limit, err)
		}
		if doc.Truncated != tc.truncated || doc.Total != 6 {
			t.Errorf("--limit %s: truncated %v total %d, want %v/6", tc.limit, doc.Truncated, doc.Total, tc.truncated)
		}
	}
}

func TestStatusJSON_LimitCutsPRListsAndEqualToLengthCutsNothing(t *testing.T) {
	src := okStatusSources()
	src.PRs = func(context.Context) (prDashJSON, []string, error) {
		rows := func(n int) []prRowJSON {
			out := make([]prRowJSON, n)
			for i := range out {
				out[i] = prRowJSON{Ref: fmt.Sprintf("o/r#%d", i+1), Title: "t"}
			}
			return out
		}
		return prDashJSON{ActiveReviews: []prDashReviewJSON{}, AwaitingYou: rows(5), YourOpen: rows(3)}, nil, nil
	}
	type bound struct {
		Bound *struct {
			Truncated bool `json:"truncated"`
			Cut       []struct {
				List  string `json:"list"`
				Total int    `json:"total"`
				Shown int    `json:"shown"`
			} `json:"cut"`
		} `json:"bound"`
		PRs struct {
			Data struct {
				AwaitingYou []json.RawMessage `json:"awaiting_you"`
				YourOpen    []json.RawMessage `json:"your_open"`
			} `json:"data"`
		} `json:"prs"`
	}
	out, _, err := runStatus(t, src, "--json", "--limit", "3")
	if err != nil {
		t.Fatal(err)
	}
	var d bound
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.PRs.Data.AwaitingYou) != 3 || len(d.PRs.Data.YourOpen) != 3 || d.Bound == nil || len(d.Bound.Cut) != 1 ||
		d.Bound.Cut[0].List != "prs.awaiting_you" || d.Bound.Cut[0].Total != 5 || d.Bound.Cut[0].Shown != 3 {
		t.Errorf("limit 3: awaiting %d, your_open %d, bound %+v; want awaiting cut 5 -> 3 and your_open (3) untouched", len(d.PRs.Data.AwaitingYou), len(d.PRs.Data.YourOpen), d.Bound)
	}
	// limit == the longest list: nothing is cut, and the report says so.
	out, _, err = runStatus(t, src, "--json", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	d = bound{}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	if d.Bound == nil || d.Bound.Truncated || len(d.Bound.Cut) != 0 || len(d.PRs.Data.AwaitingYou) != 5 {
		t.Errorf("limit 5: bound %+v, awaiting %d; want truncated false, empty cut, all 5 rows", d.Bound, len(d.PRs.Data.AwaitingYou))
	}
}

func TestStatusJSON_LimitZeroIsEveryRowWithNoBoundKey(t *testing.T) {
	out, _, err := runStatus(t, manyStatusSources(9), "--json", "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["bound"]; ok {
		t.Error("--limit 0 must add no bound key")
	}
	if n := strings.Count(out, `"/p/p`); n != 9 {
		t.Errorf("--limit 0 lists %d projects, want all 9", n)
	}
}

func TestStatusJSON_BoundComesFirst(t *testing.T) {
	out, _, err := runStatus(t, manyStatusSources(9), "--json", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if bi, gi := strings.Index(out, `"bound"`), strings.Index(out, `"git"`); bi < 0 || bi > gi {
		t.Errorf("bound at %d, git at %d: the cut must be readable before the lists", bi, gi)
	}
}

func TestMenuGroup_AcceptsAnAreaName(t *testing.T) {
	isolateJSONContractEnv(t)
	runner := &exec.FakeRunner{}
	out, stderr, err := runJSONThroughFang(t, productionJSONRoot(runner), "menu", "--json", "repos")
	if err != nil {
		t.Fatalf("menu --json repos: %v (stderr %q)", err, stderr)
	}
	var doc menuJSON
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Commands) == 0 {
		t.Fatal("menu --json repos kept no command rows")
	}
	for _, r := range doc.Commands {
		if r.Group != "repos" {
			t.Errorf("row %q has group %q, want repos", r.Command, r.Group)
		}
	}
	if len(doc.Pinned) != 0 {
		t.Errorf("an area keeps no pinned rows, got %d", len(doc.Pinned))
	}
}

func TestMenuHelp_PointsAtTheGroupArgumentAndNamesTheAreas(t *testing.T) {
	cmd := newMenuCmd(module.Deps{})
	for _, want := range []string{"33 KB", "menu --json desk", "agents", "repos", "other"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("menu --help lacks %q:\n%s", want, cmd.Long)
		}
	}
	if f := cmd.Flags().Lookup("json"); f == nil || !strings.Contains(f.Usage, "33 KB") {
		t.Error("the --json flag text must carry the size and the group pointer")
	}
}
