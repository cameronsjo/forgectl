//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// The intake fixtures live with the gate's own tests: page_captured.json is a
// live `gh api graphql` response from cameronsjo/forgectl (2026-10-09), and
// issue_base.json one issue node in the same shape.
const intakeFixtures = "../surface/intake/testdata"

// ghFixtureRunner answers `gh` from canned pages, keyed by the "after"
// cursor ("" for the first page), and runs everything else for real. The
// pinned runner sends gh through RunWithEnvFiltered, so that is where gh is
// caught; the recorded env proves the pin.
type ghFixtureRunner struct {
	exec.OSRunner
	pages map[string]string
	err   error

	mu    sync.Mutex
	calls [][]string
	envs  []map[string]string
}

func (r *ghFixtureRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	if name != "gh" {
		return r.OSRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
	}
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.envs = append(r.envs, env)
	r.mu.Unlock()
	after := ""
	for i, a := range args {
		if i > 0 && args[i-1] == "-f" && strings.HasPrefix(a, "after=") {
			after = strings.TrimPrefix(a, "after=")
		}
	}
	page, ok := r.pages[after]
	if !ok {
		return "", fmt.Errorf("no fixture page for cursor %q", after)
	}
	return page, r.err
}

func (r *ghFixtureRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "gh" {
		return "", errors.New("gh reached the runner unpinned")
	}
	return r.OSRunner.Run(ctx, name, args...)
}

func readIntakeFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(intakeFixtures, name)) //nolint:gosec // G304: name is a literal at every call site
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// baseIssueNode is issue_base.json with number and body replaced.
func baseIssueNode(t *testing.T, number int, body string) map[string]any {
	t.Helper()
	var node map[string]any
	if err := json.Unmarshal([]byte(readIntakeFixture(t, "issue_base.json")), &node); err != nil {
		t.Fatal(err)
	}
	node["number"] = number
	node["body"] = body
	return node
}

// intakePage wraps issue nodes in the query's response shape, with
// cameronsjo as both the owner and the viewer.
func intakePage(t *testing.T, ownerType string, next string, nodes ...map[string]any) string {
	t.Helper()
	return intakePageAs(t, "cameronsjo", ownerType, next, nodes...)
}

// intakePageAs is intakePage with gh authenticated as viewer.
func intakePageAs(t *testing.T, viewer, ownerType string, next string, nodes ...map[string]any) string {
	t.Helper()
	items := make([]any, len(nodes))
	for i, n := range nodes {
		items[i] = n
	}
	data, err := json.Marshal(map[string]any{"data": map[string]any{
		"viewer": map[string]any{"login": viewer},
		"repository": map[string]any{
			"owner": map[string]any{"__typename": ownerType, "login": "cameronsjo"},
			"issues": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": next != "", "endCursor": next},
				"nodes":    items,
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// intakeTestEnv is a fresh queue and a git checkout whose origin is origin.
func intakeTestEnv(t *testing.T, origin string, pages map[string]string) (module.Deps, *ghFixtureRunner, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		//nolint:gosec // G204: a fixed tool name with arguments this test constructed
		if out, err := osexec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run := &ghFixtureRunner{pages: pages}
	return module.Deps{Runner: run}, run, repo
}

// confirmRecorder is a test confirmer: it records each call's candidates and
// answers with answer (nil approves).
type confirmRecorder struct {
	answer error
	calls  [][]intakeCandidate
}

func (c *confirmRecorder) confirm(_ io.Reader, cands []intakeCandidate) error {
	c.calls = append(c.calls, cands)
	return c.answer
}

// noEnv is a getenv that finds nothing, so a test run inside a drain worker
// (FORGECTL_DRAIN_WORKER set) still runs intake.
func noEnv(string) string { return "" }

// runIntake runs intake with --json, a confirmer that approves, and no
// drain-worker marker.
func runIntake(t *testing.T, deps module.Deps, args ...string) (intakeResult, string, error) {
	t.Helper()
	return runIntakeWith(t, intakeDeps{Deps: deps, confirm: (&confirmRecorder{}).confirm, getenv: noEnv}, args...)
}

func runIntakeWith(t *testing.T, d intakeDeps, args ...string) (intakeResult, string, error) {
	t.Helper()
	out, err := runQueueCmd(t, newSurfaceIntakeGHCmdWith(d), append(args, "--json")...)
	var res intakeResult
	if out != "" {
		if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
			t.Fatalf("json: %v\n%s", jerr, out)
		}
	}
	return res, out, err
}

func names(items []intakeItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func skipReason(t *testing.T, res intakeResult, number int) string {
	t.Helper()
	for _, it := range res.Skipped {
		if it.Number == number {
			return it.Reason
		}
	}
	t.Fatalf("#%d not skipped: %+v", number, res)
	return ""
}

func queueRows(t *testing.T) []worker.QueueRow {
	t.Helper()
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

const forgectlOrigin = "https://github.com/cameronsjo/forgectl.git"

// capturedLabels makes the captured page's label eligible. exec:guided is a
// triage label a sweep applies, so it is not a default (security review I2);
// the capture predates the queue:drain default.
var capturedLabels = config.SurfaceIntakeConfig{Labels: []string{"queue:drain", "exec:guided"}}

// TestSurfaceIntakeCapturedPage runs intake against the live capture: two
// owner-authored issues the owner labeled exec:guided, read as the owner. Both
// are queued once, a second run queues nothing, and the query is pinned to
// github.com.
func TestSurfaceIntakeCapturedPage(t *testing.T) {
	deps, run, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": readIntakeFixture(t, "page_captured.json")})
	deps.Cfg.Surface.Intake = capturedLabels

	res, _, err := runIntake(t, deps, "--repo", repo, "--label", "exec:guided", "--harness", "codex", "--model", "gpt-5")
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if got := names(res.Enqueued); !slices.Equal(got, []string{"gh13-forgectl", "gh32-forgectl"}) || len(res.Skipped) != 0 {
		t.Fatalf("enqueued %v, skipped %+v", got, res.Skipped)
	}
	if res.GitHub != "cameronsjo/forgectl" || !slices.Equal(res.Authors, []string{"cameronsjo"}) || !slices.Equal(res.Labels, []string{"exec:guided"}) {
		t.Fatalf("result %+v", res)
	}
	rows := queueRows(t)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0]
	if r.Source != "gh:cameronsjo/forgectl#13" || r.Author != "cameronsjo" || r.Harness != "codex" || r.Model != "gpt-5" || r.State != worker.QueueQueued {
		t.Fatalf("row %+v", r)
	}
	if !strings.Contains(r.Brief, "Closes #13\n") || !strings.Contains(r.Brief, "Title: forgectl status: the forge cockpit (cross-project overview)") ||
		r.BriefSHA256 != res.Enqueued[0].BriefSHA256 {
		t.Fatalf("brief:\n%s", r.Brief)
	}

	// The pin: --hostname github.com on the argv and GH_HOST in the env.
	call := run.calls[0]
	if !slices.Contains(call, "--hostname") || call[slices.Index(call, "--hostname")+1] != "github.com" || run.envs[0]["GH_HOST"] != "github.com" {
		t.Fatalf("gh call not pinned: %v %v", call, run.envs[0])
	}
	if !slices.Contains(call, "labels[]=exec:guided") || slices.Contains(call, "labels[]=queue:drain") {
		t.Fatalf("--label did not narrow the query: %v", call)
	}

	// Idempotent: the same run again adds nothing and names each row's state.
	res, _, err = runIntake(t, deps, "--repo", repo, "--label", "exec:guided")
	if err != nil {
		t.Fatalf("second intake: %v", err)
	}
	if len(res.Enqueued) != 0 || skipReason(t, res, 13) != "already in the queue (state queued)" {
		t.Fatalf("second run %+v", res)
	}
	if len(queueRows(t)) != 2 {
		t.Fatal("the second run wrote rows")
	}
}

func TestSurfaceIntakeDryRunWritesNothing(t *testing.T) {
	deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": readIntakeFixture(t, "page_captured.json")})
	deps.Cfg.Surface.Intake = capturedLabels
	res, _, err := runIntake(t, deps, "--repo", repo, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Enqueued) != 2 || res.Enqueued[0].BriefSHA256 == "" {
		t.Fatalf("dry run %+v", res)
	}
	if rows := queueRows(t); len(rows) != 0 {
		t.Fatalf("dry run wrote %+v", rows)
	}
}

func TestSurfaceIntakeSkipsAndGoesOn(t *testing.T) {
	edited := baseIssueNode(t, 2, "edited after labeling")
	edited["lastEditedAt"] = "2026-10-01T11:00:00Z"
	outsider := baseIssueNode(t, 3, "from someone else")
	outsider["author"] = map[string]any{"__typename": "User", "login": "someone-else"}
	pr := baseIssueNode(t, 4, "a pull request")
	pr["__typename"] = "PullRequest"
	noEdit := baseIssueNode(t, 5, "no lastEditedAt in the response")
	delete(noEdit, "lastEditedAt")
	page := intakePage(t, "User", "",
		baseIssueNode(t, 1, "first, taken"),
		edited, outsider, pr, noEdit,
		baseIssueNode(t, 6, "too long "+strings.Repeat("x", worker.MaxLaunchBrief)),
		baseIssueNode(t, 7, "escape \x1b[2J in the body"),
		baseIssueNode(t, 8, "last, taken"),
	)
	deps, run, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
	res, _, err := runIntake(t, deps, "--repo", repo)
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	// The default eligible label is queue:drain alone (security review I2).
	var queried []string
	for _, a := range run.calls[0] {
		if strings.HasPrefix(a, "labels[]=") {
			queried = append(queried, a)
		}
	}
	if !slices.Equal(queried, []string{"labels[]=queue:drain"}) || !slices.Equal(res.Labels, []string{"queue:drain"}) {
		t.Fatalf("default labels: queried %v, result %v", queried, res.Labels)
	}
	if got := names(res.Enqueued); !slices.Equal(got, []string{"gh1-forgectl", "gh8-forgectl"}) {
		t.Fatalf("enqueued %v; skipped %+v", got, res.Skipped)
	}
	for number, want := range map[int]string{
		2: "its body was edited at 2026-10-01T11:00:00Z",
		3: `its author, user "someone-else"`,
		4: `it is a "PullRequest"`,
		5: "does not say whether its body was edited",
		6: "bytes, limit 65536",
		7: "a control or invisible character",
	} {
		if got := skipReason(t, res, number); !strings.Contains(got, want) {
			t.Errorf("#%d: reason %q, want %q", number, got, want)
		}
	}
	if len(queueRows(t)) != 2 {
		t.Fatal("a skipped issue was queued")
	}
}

// TestSurfaceIntakeViewerCase: the owner-is-viewer check compares logins
// case-insensitively, so gh authenticated as CameronSjo owns cameronsjo's
// repository.
func TestSurfaceIntakeViewerCase(t *testing.T) {
	page := intakePageAs(t, "CameronSjo", "User", "", baseIssueNode(t, 1, "one"))
	deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
	res, _, err := runIntake(t, deps, "--repo", repo)
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if got := names(res.Enqueued); !slices.Equal(got, []string{"gh1-forgectl"}) || !slices.Equal(res.Authors, []string{"cameronsjo"}) {
		t.Fatalf("result %+v", res)
	}
}

func TestSurfaceIntakeExistingRows(t *testing.T) {
	page := intakePage(t, "User", "", baseIssueNode(t, 1, "one"), baseIssueNode(t, 2, "two"))
	deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	// gh1-forgectl is taken by a forgectl checkout elsewhere (another owner's
	// repository of the same name); gh2-forgectl failed here earlier.
	if _, _, err := q.Enqueue("gh1-forgectl", "/elsewhere/forgectl", "another brief", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	top, err := worker.RepoTop(t.Context(), exec.OSRunner{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Enqueue("gh2-forgectl", top, "the old brief", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateIf("gh2-forgectl", func(worker.QueueRow) bool { return true }, time.Now(), func(r *worker.QueueRow) { r.State = worker.QueueFailed }); err != nil {
		t.Fatal(err)
	}
	res, _, err := runIntake(t, deps, "--repo", repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Enqueued) != 0 {
		t.Fatalf("enqueued %+v", res.Enqueued)
	}
	if got := skipReason(t, res, 1); !strings.Contains(got, "taken by a row for /elsewhere/forgectl") {
		t.Errorf("#1: %q", got)
	}
	if got := skipReason(t, res, 2); got != "already in the queue (state failed); dequeue it to retry" {
		t.Errorf("#2: %q", got)
	}
}

func TestSurfaceIntakeMaxPerRunAndPages(t *testing.T) {
	pages := map[string]string{
		"":      intakePage(t, "User", "cur1", baseIssueNode(t, 1, "one")),
		"cur1":  intakePage(t, "User", "", baseIssueNode(t, 2, "two"), baseIssueNode(t, 3, "three")),
		"other": "unused",
	}
	deps, run, repo := intakeTestEnv(t, forgectlOrigin, pages)
	two := 2
	deps.Cfg.Surface.Intake = config.SurfaceIntakeConfig{MaxPerRun: &two}
	res, _, err := runIntake(t, deps, "--repo", repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Enqueued); !slices.Equal(got, []string{"gh1-forgectl", "gh2-forgectl"}) || !strings.Contains(res.Stopped, "max_per_run (2) reached") {
		t.Fatalf("result %+v", res)
	}
	if len(run.calls) != 2 || !slices.Contains(run.calls[1], "after=cur1") {
		t.Fatalf("calls %v", run.calls)
	}
}

func TestSurfaceIntakeRefusesTheRun(t *testing.T) {
	captured := readIntakeFixture(t, "page_captured.json")
	cases := map[string]struct {
		origin string
		pages  map[string]string
		ghErr  error
		cfg    config.SurfaceIntakeConfig
		args   []string
		exit   int
		want   string
	}{
		"non-github remote": {
			origin: "https://gitlab.com/cameronsjo/forgectl.git", exit: exitUsage, want: `origin's host "gitlab.com" is not github.com`,
		},
		"owner is not the viewer": {
			// A clone of someone else's user-owned repository: its owner is
			// not the operator, so the default author list does not apply.
			pages: map[string]string{"": intakePageAs(t, "operator", "User", "", baseIssueNode(t, 1, "one"))},
			exit:  exitUsage, want: `the owner is "cameronsjo" and gh is authenticated as "operator"; set [surface.intake] authors`,
		},
		"captured page with no viewer": {
			pages: map[string]string{"": strings.Replace(captured, `"viewer":{"login":"cameronsjo"},`, "", 1)},
			exit:  exitFailed, want: "no viewer login",
		},
		"organization owner with no authors": {
			pages: map[string]string{"": intakePage(t, "Organization", "", baseIssueNode(t, 1, "one"))},
			exit:  exitUsage, want: "owned by an organization",
		},
		"owner differs from origin": {
			origin: "https://github.com/someone-else/forgectl.git", pages: map[string]string{"": captured},
			exit: exitUsage, want: `GitHub names the repository's owner "cameronsjo", origin names "someone-else"`,
		},
		"graphql error": {
			pages: map[string]string{"": readIntakeFixture(t, "error_not_found.json")}, ghErr: errors.New("exit status 1"),
			exit: exitFailed, want: "gh api graphql",
		},
		"graphql error with exit 0": {
			pages: map[string]string{"": readIntakeFixture(t, "error_undefined_field.json")},
			exit:  exitFailed, want: "GitHub returned 1 error(s)",
		},
		"label not eligible": {
			args: []string{"--label", "kind:bug"}, exit: exitUsage, want: "is not one of [surface.intake] labels",
		},
		"invalid config": {
			cfg:  config.SurfaceIntakeConfig{Authors: []string{"dependabot[bot]"}},
			exit: exitUsage, want: "[surface.intake] authors",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			origin := tc.origin
			if origin == "" {
				origin = forgectlOrigin
			}
			deps, run, repo := intakeTestEnv(t, origin, tc.pages)
			run.err = tc.ghErr
			deps.Cfg.Surface.Intake = tc.cfg
			_, _, err := runIntake(t, deps, append([]string{"--repo", repo}, tc.args...)...)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := ExitCode(err); got != tc.exit {
				t.Errorf("exit %d, want %d (%v)", got, tc.exit, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want %q", err, tc.want)
			}
			if len(queueRows(t)) != 0 {
				t.Error("a refused run wrote rows")
			}
		})
	}
}

func TestSurfaceIntakeStopsOnAFullQueue(t *testing.T) {
	page := intakePage(t, "User", "", baseIssueNode(t, 1, "one"), baseIssueNode(t, 2, "two"))
	deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	// Fill the queue in large rows, then in small ones, until a row of a
	// few hundred bytes no longer fits; an intake brief is larger.
	i := 0
	for _, size := range []int{worker.MaxLaunchBrief - 64, 300} {
		filler := strings.Repeat("f", size)
		for ; ; i++ {
			_, _, err := q.Enqueue(fmt.Sprintf("fill-%d", i), "/fill", filler+fmt.Sprint(i), "", time.Now())
			if errors.Is(err, worker.ErrQueueFull) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	res, _, err := runIntake(t, deps, "--repo", repo)
	if ExitCode(err) != exitFailed || !strings.Contains(res.Stopped, "the queue is full") {
		t.Fatalf("full queue: %v, %+v", err, res)
	}
}

// TestSurfaceIntakeQueuesOnlyAfterConfirmation: a real run hands the
// confirmer every candidate, with the labeling that admitted it, before any
// queue write, and the rows record labeler and labeled_at, which `surface
// queue --json` shows.
func TestSurfaceIntakeQueuesOnlyAfterConfirmation(t *testing.T) {
	titled := baseIssueNode(t, 2, "two")
	titled["title"] = "Second issue"
	page := intakePage(t, "User", "", baseIssueNode(t, 1, "one"), titled)
	deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
	rec := &confirmRecorder{}
	confirm := func(in io.Reader, cands []intakeCandidate) error {
		if rows := queueRows(t); len(rows) != 0 {
			t.Errorf("rows written before the confirmation: %+v", rows)
		}
		return rec.confirm(in, cands)
	}
	res, _, err := runIntakeWith(t, intakeDeps{Deps: deps, confirm: confirm, getenv: noEnv}, "--repo", repo)
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if len(rec.calls) != 1 || len(rec.calls[0]) != 2 {
		t.Fatalf("confirmer calls %+v, want one call with two candidates", rec.calls)
	}
	first := rec.calls[0][0]
	if first.Number != 1 || first.Name != "gh1-forgectl" || first.Title != "Tidy the queue listing" || first.Author != "cameronsjo" ||
		first.Labeler != "cameronsjo" || first.LabeledAt != "2026-10-01T10:00:00Z" || first.Source != "gh:cameronsjo/forgectl#1" ||
		first.BriefSHA256 == "" || first.BriefSHA256 != res.Enqueued[0].BriefSHA256 {
		t.Fatalf("candidate %+v", first)
	}
	if got := names(res.Enqueued); !slices.Equal(got, []string{"gh1-forgectl", "gh2-forgectl"}) {
		t.Fatalf("enqueued %v", got)
	}
	if res.Enqueued[0].Labeler != "cameronsjo" || res.Enqueued[0].LabeledAt != "2026-10-01T10:00:00Z" {
		t.Fatalf("result item %+v", res.Enqueued[0])
	}
	rows := queueRows(t)
	if len(rows) != 2 || rows[0].Labeler != "cameronsjo" || rows[0].LabeledAt != "2026-10-01T10:00:00Z" {
		t.Fatalf("rows %+v", rows)
	}
	out, err := runQueueCmd(t, newSurfaceQueueCmd(deps), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed queueResult
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if listed.Rows[0].Labeler != "cameronsjo" || listed.Rows[0].LabeledAt != "2026-10-01T10:00:00Z" {
		t.Fatalf("surface queue --json %+v", listed.Rows[0])
	}
	var shown strings.Builder
	// The gate refuses an escape in a title (CheckQueueBrief), so the list's
	// own sanitizing is pinned with a candidate built here.
	shownCands := append(slices.Clone(rec.calls[0]), intakeCandidate{Number: 9, Title: "Title with \x1b[2J an escape\nand a second line",
		Body: "Body with \x1b[2J an escape\n\nand a second paragraph " + strings.Repeat("x", 300)})
	if err := writeIntakeCandidates(&shown, shownCands); err != nil {
		t.Fatal(err)
	}
	// The URL and a body excerpt are what let a reader judge an issue whose
	// author and labeler read as the operator (cameronsjo/forgectl#1205).
	for _, want := range []string{"#1 Tidy the queue listing", "url https://github.com/cameronsjo/forgectl/issues/1\n", "body (3 chars): one\n",
		"chars): Body with \\x1b[2J an escape and a second paragraph x", "labeled by cameronsjo at 2026-10-01T10:00:00Z", "row gh1-forgectl, brief sha256 " + first.BriefSHA256} {
		if !strings.Contains(shown.String(), want) {
			t.Errorf("candidate list lacks %q:\n%s", want, shown.String())
		}
	}
	if strings.Contains(shown.String(), strings.Repeat("x", 300)) {
		t.Errorf("candidate list shows the whole body, want an excerpt:\n%s", shown.String())
	}
	if strings.Contains(shown.String(), "\x1b") || strings.Contains(shown.String(), "\nand a second line") {
		t.Errorf("candidate list carries a raw escape or a second title line:\n%q", shown.String())
	}
}

// TestSurfaceIntakeDeclinedQueuesNothing: any refusal from the confirmer
// queues nothing, exits non-zero, and reports each candidate as skipped.
func TestSurfaceIntakeDeclinedQueuesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		sentinel error
		exit     int
	}{
		"not yes":     {errIntakeNotConfirmed, exitFailed},
		"no terminal": {errIntakeNoTerminal, exitUsage},
	} {
		t.Run(name, func(t *testing.T) {
			page := intakePage(t, "User", "", baseIssueNode(t, 1, "one"))
			deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
			rec := &confirmRecorder{answer: WithExitCode(tc.sentinel, tc.exit)}
			res, _, err := runIntakeWith(t, intakeDeps{Deps: deps, confirm: rec.confirm, getenv: noEnv}, "--repo", repo)
			if ExitCode(err) != tc.exit || !errors.Is(err, tc.sentinel) {
				t.Fatalf("err %v (exit %d), want %v (exit %d)", err, ExitCode(err), tc.sentinel, tc.exit)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("confirmer called %d times", len(rec.calls))
			}
			if len(res.Enqueued) != 0 || skipReason(t, res, 1) != "not confirmed at a terminal" || !strings.Contains(res.Stopped, "nothing was queued") {
				t.Fatalf("result %+v", res)
			}
			if rows := queueRows(t); len(rows) != 0 {
				t.Fatalf("a declined run wrote %+v", rows)
			}
		})
	}
}

// TestSurfaceIntakeNeverPromptsWithoutCandidates: --dry-run, and a run the
// gate admits nothing in, never call the confirmer.
func TestSurfaceIntakeNeverPromptsWithoutCandidates(t *testing.T) {
	outsider := baseIssueNode(t, 3, "from someone else")
	outsider["author"] = map[string]any{"__typename": "User", "login": "someone-else"}
	for name, tc := range map[string]struct {
		page string
		args []string
	}{
		"dry run":           {intakePage(t, "User", "", baseIssueNode(t, 1, "one")), []string{"--dry-run"}},
		"nothing admitted":  {intakePage(t, "User", "", outsider), nil},
		"no labeled issues": {intakePage(t, "User", ""), nil},
	} {
		t.Run(name, func(t *testing.T) {
			deps, _, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": tc.page})
			rec := &confirmRecorder{answer: errors.New("the confirmer was called")}
			if _, _, err := runIntakeWith(t, intakeDeps{Deps: deps, confirm: rec.confirm, getenv: noEnv}, append([]string{"--repo", repo}, tc.args...)...); err != nil {
				t.Fatalf("intake: %v", err)
			}
			if len(rec.calls) != 0 {
				t.Fatalf("confirmer called with %+v", rec.calls)
			}
			if rows := queueRows(t); len(rows) != 0 {
				t.Fatalf("rows %+v", rows)
			}
		})
	}
}

// TestSurfaceIntakeRefusesInADrainWorker: FORGECTL_DRAIN_WORKER set to any
// non-empty value refuses with exit 2 before gh is called or anyone is
// asked, through the injected lookup and through the production command.
func TestSurfaceIntakeRefusesInADrainWorker(t *testing.T) {
	page := intakePage(t, "User", "", baseIssueNode(t, 1, "one"))
	check := func(t *testing.T, run *ghFixtureRunner, rec *confirmRecorder, err error) {
		t.Helper()
		if ExitCode(err) != exitUsage || !errors.Is(err, errIntakeInWorker) {
			t.Fatalf("err %v (exit %d), want errIntakeInWorker, exit 2", err, ExitCode(err))
		}
		if len(run.calls) != 0 {
			t.Fatalf("gh was called: %v", run.calls)
		}
		if rec != nil && len(rec.calls) != 0 {
			t.Fatal("the confirmer was called")
		}
		if rows := queueRows(t); len(rows) != 0 {
			t.Fatalf("rows %+v", rows)
		}
	}
	t.Run("injected", func(t *testing.T) {
		deps, run, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
		rec := &confirmRecorder{}
		getenv := func(k string) string {
			if k == "FORGECTL_DRAIN_WORKER" {
				return "yes-any-value"
			}
			return ""
		}
		_, _, err := runIntakeWith(t, intakeDeps{Deps: deps, confirm: rec.confirm, getenv: getenv}, "--repo", repo)
		check(t, run, rec, err)
	})
	t.Run("production", func(t *testing.T) {
		deps, run, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
		t.Setenv("FORGECTL_DRAIN_WORKER", "1")
		_, err := runQueueCmd(t, newSurfaceIntakeGHCmd(deps), "--repo", repo, "--json")
		check(t, run, nil, err)
	})
}

// TestSurfaceIntakeProductionRefusesAPipedYes: the production command, with
// "yes" piped into stdin, refuses as having no terminal and queues nothing.
func TestSurfaceIntakeProductionRefusesAPipedYes(t *testing.T) {
	page := intakePage(t, "User", "", baseIssueNode(t, 1, "one"))
	deps, run, repo := intakeTestEnv(t, forgectlOrigin, map[string]string{"": page})
	t.Setenv("FORGECTL_DRAIN_WORKER", "")
	cmd := newSurfaceIntakeGHCmd(deps)
	cmd.SetIn(strings.NewReader("yes\n"))
	_, err := runQueueCmd(t, cmd, "--repo", repo, "--json")
	if ExitCode(err) != exitUsage || !errors.Is(err, errIntakeNoTerminal) {
		t.Fatalf("err %v (exit %d), want errIntakeNoTerminal, exit 2", err, ExitCode(err))
	}
	if len(run.calls) == 0 {
		t.Fatal("gh was not read; the refusal should come at the confirmation")
	}
	if rows := queueRows(t); len(rows) != 0 {
		t.Fatalf("a piped yes queued %+v", rows)
	}
}

var sampleCandidates = []intakeCandidate{{
	Number: 7, Title: "Fix the thing", Author: "cameronsjo", Labeler: "cameronsjo",
	LabeledAt: "2026-10-01T10:00:00Z", Name: "gh7-forgectl", Source: "gh:cameronsjo/forgectl#7", BriefSHA256: "abc123",
}}

// askSample is intake's question about sampleCandidates.
func askSample(in io.Reader, out io.Writer) error { return askIntake(in, out, sampleCandidates) }

// TestIntakeTerminalNeedsBothTerminals: stdin must be a terminal and
// /dev/tty must open as one; the answer comes from the tty, never stdin.
func TestIntakeTerminalNeedsBothTerminals(t *testing.T) {
	stdinTerminal := func(v bool) func(io.Reader) bool { return func(io.Reader) bool { return v } }

	opened := false
	open := func(tty *fakeTTY, err error) func() (io.ReadWriteCloser, error) {
		return func() (io.ReadWriteCloser, error) {
			opened = true
			if err != nil {
				return nil, err
			}
			return tty, nil
		}
	}

	// stdin not a terminal: refused before /dev/tty is opened, whatever stdin holds.
	tty := &fakeTTY{in: strings.NewReader("yes\n")}
	err := terminalConfirm{stdinIsTerminal: stdinTerminal(false), openTTY: open(tty, nil)}.confirm(strings.NewReader("yes\n"), errIntakeNoTerminal, askSample)
	if ExitCode(err) != exitUsage || !errors.Is(err, errIntakeNoTerminal) || opened {
		t.Fatalf("stdin not a terminal: %v (exit %d), opened %v", err, ExitCode(err), opened)
	}
	if !strings.Contains(err.Error(), "run it from a plain terminal, or use --dry-run") {
		t.Fatalf("message %q", err)
	}

	// /dev/tty does not open as a terminal.
	err = terminalConfirm{stdinIsTerminal: stdinTerminal(true), openTTY: open(nil, errors.New("not a terminal"))}.confirm(nil, errIntakeNoTerminal, askSample)
	if ExitCode(err) != exitUsage || !errors.Is(err, errIntakeNoTerminal) {
		t.Fatalf("no tty: %v (exit %d)", err, ExitCode(err))
	}

	// Both terminals: the answer is the tty's, not stdin's.
	tty = &fakeTTY{in: strings.NewReader("no\n")}
	err = terminalConfirm{stdinIsTerminal: stdinTerminal(true), openTTY: open(tty, nil)}.confirm(strings.NewReader("yes\n"), errIntakeNoTerminal, askSample)
	if ExitCode(err) != exitFailed || !errors.Is(err, errIntakeNotConfirmed) || !tty.closed {
		t.Fatalf("tty says no, stdin says yes: %v (exit %d), closed %v", err, ExitCode(err), tty.closed)
	}
	tty = &fakeTTY{in: strings.NewReader("yes\n")}
	if err := (terminalConfirm{stdinIsTerminal: stdinTerminal(true), openTTY: open(tty, nil)}).confirm(strings.NewReader(""), errIntakeNoTerminal, askSample); err != nil {
		t.Fatalf("tty says yes: %v", err)
	}
	if !strings.Contains(tty.out.String(), "#7 Fix the thing") || !strings.Contains(tty.out.String(), `Type "yes" to queue them`) {
		t.Fatalf("the tty did not get the candidates and the question:\n%s", tty.out.String())
	}

	// The zero value refuses.
	if err := (terminalConfirm{}).confirm(nil, errIntakeNoTerminal, askSample); !errors.Is(err, errIntakeNoTerminal) {
		t.Fatalf("zero terminalConfirm: %v", err)
	}
}

// TestAskIntakeTakesOnlyYes: only a complete line that is exactly "yes",
// trimmed, confirms.
func TestAskIntakeTakesOnlyYes(t *testing.T) {
	for answer, want := range map[string]bool{
		"yes\n":        true,
		"  yes \t\n":   true,
		"yes\r\n":      true,
		"no\n":         false,
		"YES please\n": false,
		"YES\n":        false,
		"y\n":          false,
		"yes yes\n":    false,
		"\n":           false,
		"":             false, // end of input
		"yes":          false, // end of input before the newline
		"nope\nyes\n":  false, // only the first line counts
		strings.Repeat(" ", maxConfirmAnswer) + "yes\n": false,
	} {
		var out strings.Builder
		err := askIntake(strings.NewReader(answer), &out, sampleCandidates)
		if got := err == nil; got != want {
			t.Errorf("answer %q: confirmed %v, want %v (%v)", answer, got, want, err)
		}
		if err != nil && (ExitCode(err) != exitFailed || !errors.Is(err, errIntakeNotConfirmed)) {
			t.Errorf("answer %q: %v (exit %d), want errIntakeNotConfirmed, exit 1", answer, err, ExitCode(err))
		}
	}
}

// Blank-looking runes in an issue's title or body are written as escapes, so
// padding made of them cannot wrap into a fake labeled line
// (cameronsjo/forgectl#1215).
func TestEscapeInvisibleInvisibleRunes(t *testing.T) {
	for _, r := range []rune{0x2800, 0x3164, 0xffa0, 0x115f, 0x1160} {
		got := escapeInvisible("a" + string(r) + "b")
		want := fmt.Sprintf("a\\u%04Xb", r)
		if got != want {
			t.Errorf("escapeInvisible(%U) = %q, want %q", r, got, want)
		}
	}
	if got := intakeBodyExcerpt(strings.Repeat("⠀", 240)); strings.ContainsRune(got, 0x2800) {
		t.Errorf("excerpt still holds U+2800: %q", got)
	}
	if got := escapeInvisible("plain é text"); got != "plain é text" {
		t.Errorf("plain text changed: %q", got)
	}
}

// The rendered candidate block carries the escapes for a padded title, so the
// title wiring is pinned too (cameronsjo/forgectl#1215).
func TestWriteIntakeCandidatesEscapesATitleAndBody(t *testing.T) {
	pad := strings.Repeat("\u3164", 20)
	c := intakeCandidate{Title: "t" + pad + "\nurl x", Body: pad + " tail \U000e0041"}
	var out strings.Builder
	if err := writeIntakeCandidates(&out, []intakeCandidate{c}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// Runs of spaces collapse in the title as in the body, so padding cannot
	// push a fake line onto a new screen row.
	if strings.Contains(got, "  ") && strings.Contains(strings.SplitN(got, "\n", 3)[1], "     ") {
		t.Errorf("title keeps a run of spaces: %q", got)
	}
	if strings.ContainsRune(got, 0x3164) || strings.ContainsRune(got, 0xe0041) || !strings.Contains(got, `\u3164`) || !strings.Contains(got, `\U000E0041`) {
		t.Errorf("rendered block not escaped: %q", got)
	}
}

func TestWriteIntakeCandidatesCollapsesTitleSpaces(t *testing.T) {
	c := intakeCandidate{Title: "Fix typo" + strings.Repeat("\u3000", 40) + "      body (12 chars): fake", Body: "x"}
	var out strings.Builder
	if err := writeIntakeCandidates(&out, []intakeCandidate{c}); err != nil {
		t.Fatal(err)
	}
	if first := strings.SplitN(out.String(), "\n", 3)[1]; !strings.HasSuffix(first, "Fix typo body (12 chars): fake") {
		t.Errorf("title line = %q, want runs of spaces collapsed", first)
	}
}
