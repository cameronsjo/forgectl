//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func runIntake(t *testing.T, deps module.Deps, args ...string) (intakeResult, string, error) {
	t.Helper()
	out, err := runQueueCmd(t, newSurfaceIntakeGHCmd(deps), append(args, "--json")...)
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
