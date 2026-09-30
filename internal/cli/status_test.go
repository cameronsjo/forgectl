package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/status"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// okStatusSources returns four sources that each answer at once with one
// row of ordinary data.
func okStatusSources() statusSources {
	return statusSources{
		Git: func(context.Context) (statusGitJSON, []string, error) {
			return newStatusGit("/p", []projects.Project{
				{Name: "tidy", Dir: "/p/tidy", Status: projects.GitStatus{State: projects.StatusOK}},
				{Name: "busy", Dir: "/p/busy", Status: projects.GitStatus{State: projects.StatusOK, Modified: 2}},
			}), nil, nil
		},
		PRs: func(context.Context) (prDashJSON, []string, error) {
			return prDashJSON{
				ActiveReviews: []prDashReviewJSON{},
				AwaitingYou:   []prRowJSON{{Ref: "o/r#1", Title: "a title"}},
				YourOpen:      []prRowJSON{},
			}, nil, nil
		},
		Clean: func(context.Context) (statusCleanJSON, []string, error) {
			return statusCleanJSON{Root: "/p", TotalReclaimableBytes: 2048, Reclaimable: 1, Skipped: 1}, nil, nil
		},
		Bench: func(context.Context) (bench.Report, []string, error) {
			return bench.Report{
				Hearth:    bench.Component{Name: "hearth", State: bench.StateOK, Reason: "up"},
				Chronicle: bench.Component{Name: "chronicle", State: bench.StateUnavailable, Reason: "down"},
			}, nil, nil
		},
	}
}

// runStatus executes the status command over src and returns stdout,
// stderr and the RunE error.
func runStatus(t *testing.T, src statusSources, args ...string) (string, string, error) {
	t.Helper()
	cmd := newStatusCmdForSources(src, theme.Theme{})
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestStatusJSON_EverySectionHasTheFullEnvelope(t *testing.T) {
	out, _, err := runStatus(t, okStatusSources(), "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("stdout is not one JSON object of sections: %v\n%s", err, out)
	}
	if len(raw) != 4 {
		t.Errorf("got %d top-level keys, want exactly git/prs/clean/bench: %s", len(raw), out)
	}
	for _, key := range []string{"git", "prs", "clean", "bench"} {
		sec, ok := raw[key]
		if !ok {
			t.Errorf("section %q missing", key)
			continue
		}
		if len(sec) != 4 {
			t.Errorf("%s has %d fields, want state/error/notes/data: %v", key, len(sec), sec)
		}
		if got := string(sec["state"]); got != `"ok"` {
			t.Errorf("%s.state = %s, want \"ok\"", key, got)
		}
		if got := strings.TrimSpace(string(sec["notes"])); got != "[]" {
			t.Errorf("%s.notes = %s, want []", key, got)
		}
		if got := strings.TrimSpace(string(sec["data"])); got == "null" || got == "" {
			t.Errorf("%s.data = %s, want an object", key, got)
		}
	}
}

func TestStatusJSON_SectionDataShapes(t *testing.T) {
	out, _, err := runStatus(t, okStatusSources(), "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var got statusReportJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	g := got.Git.Data
	if g == nil || g.Total != 2 || g.Clean != 1 || g.Dirty != 1 || len(g.Projects) != 2 {
		t.Errorf("git data = %+v, want 2 projects, 1 clean, 1 dirty", g)
	}
	if p := got.PRs.Data; p == nil || len(p.AwaitingYou) != 1 || p.AwaitingYou[0].Ref != "o/r#1" {
		t.Errorf("prs data = %+v, want the pr dash document", p)
	}
	if c := got.Clean.Data; c == nil || c.TotalReclaimableBytes != 2048 || c.Skipped != 1 {
		t.Errorf("clean data = %+v", c)
	}
	if b := got.Bench.Data; b == nil || b.Chronicle.State != bench.StateUnavailable {
		t.Errorf("bench data = %+v, want the bench report", b)
	}
	for _, key := range []string{`"total_reclaimable_bytes"`, `"not_a_repo"`, `"awaiting_you"`, `"hearth"`} {
		if !strings.Contains(out, key) {
			t.Errorf("wire lacks %s", key)
		}
	}
}

// failingStatusSources degrades prs with a note, fails git with an error,
// panics in clean and stalls bench past the deadline.
func failingStatusSources(t *testing.T) statusSources {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		return statusGitJSON{}, nil, errors.New("projects directory not found")
	}
	inner := src.PRs
	src.PRs = func(ctx context.Context) (prDashJSON, []string, error) {
		d, _, err := inner(ctx)
		return d, []string{"your-open: query failed"}, err
	}
	src.Clean = func(context.Context) (statusCleanJSON, []string, error) {
		panic("walk exploded")
	}
	src.Bench = func(context.Context) (bench.Report, []string, error) {
		<-release
		return bench.Report{}, nil, nil
	}
	return src
}

func TestStatusJSON_AFailedSourceDegradesOnlyItsSection(t *testing.T) {
	out, stderr, err := runStatus(t, failingStatusSources(t), "--json", "--timeout", "50ms")
	if err != nil {
		t.Fatalf("a failed section must not fail the command: %v", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty: the report is the verdict", stderr)
	}
	var got statusReportJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Git.State != status.StateFailed || got.Git.Data != nil || got.Git.Error != "projects directory not found" {
		t.Errorf("git = %+v, want failed with the source error and no data", got.Git)
	}
	if got.PRs.State != status.StateDegraded || got.PRs.Data == nil || len(got.PRs.Notes) != 1 {
		t.Errorf("prs = %+v, want degraded with data and the note", got.PRs)
	}
	if got.Clean.State != status.StateFailed || got.Clean.Error != "source panicked" {
		t.Errorf("clean = %+v, want failed \"source panicked\"", got.Clean)
	}
	if got.Bench.State != status.StateFailed || got.Bench.Error != "timed out after 50ms" {
		t.Errorf("bench = %+v, want failed on its deadline", got.Bench)
	}
	if !strings.Contains(out, `"data": null`) {
		t.Errorf("a failed section must encode data as null:\n%s", out)
	}
}

func TestStatus_StrictExitsOneAfterWritingTheReport(t *testing.T) {
	out, stderr, err := runStatus(t, failingStatusSources(t), "--json", "--strict", "--timeout", "50ms")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v (code %d), want exit 1 under --strict", err, ExitCode(err))
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("--strict must still write the report:\n%s", out)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty under --json", stderr)
	}
	if _, _, err := runStatus(t, okStatusSources(), "--json", "--strict"); err != nil {
		t.Errorf("--strict with every section ok: %v, want exit 0", err)
	}
	degraded := okStatusSources()
	degraded.Clean = func(context.Context) (statusCleanJSON, []string, error) {
		return statusCleanJSON{}, []string{"partial"}, nil
	}
	if _, _, err := runStatus(t, degraded, "--json", "--strict"); err == nil || ExitCode(err) != 1 {
		t.Errorf("--strict with one degraded section: err = %v, want exit 1", err)
	}
}

func TestStatus_RefusesANonPositiveTimeout(t *testing.T) {
	called := false
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		called = true
		return statusGitJSON{}, nil, nil
	}
	for _, v := range []string{"0s", "-1s"} {
		out, _, err := runStatus(t, src, "--timeout", v)
		if err == nil || !strings.Contains(err.Error(), "--timeout") {
			t.Errorf("--timeout %s: err = %v, want a refusal naming --timeout", v, err)
		}
		if out != "" {
			t.Errorf("--timeout %s wrote %q before refusing", v, out)
		}
	}
	if called {
		t.Error("a source ran despite the refused --timeout")
	}
}

func TestStatusText_FailedAndDegradedSectionsRender(t *testing.T) {
	out, _, err := runStatus(t, failingStatusSources(t), "--timeout", "50ms")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{
		"✗ git    failed: projects directory not found",
		"! prs    0 active review(s), 1 awaiting you, 0 open by you",
		"    note: your-open: query failed",
		"✗ clean  failed: source panicked",
		"✗ bench  failed: timed out after 50ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusText_OKLayout(t *testing.T) {
	out, _, err := runStatus(t, okStatusSources())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{
		`✓ git    2 project(s) under "/p": 1 clean, 1 dirty, 0 ahead, 0 unknown`,
		`    "busy"  [2 modified]`,
		"    o/r#1  a title",
		`✓ clean  2.0 KiB reclaimable across 1 target(s), 1 skipped, under "/p"`,
		"✓ bench  hearth ok, chronicle unavailable",
		"    ✗ chronicle — down",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"tidy"`) {
		t.Errorf("a clean project was listed:\n%s", out)
	}
	if strings.Contains(out, "hearth — up") {
		t.Errorf("an ok bench component got a reason line:\n%s", out)
	}
}

func TestStatusText_EscapesAndCapsEveryUntrustedField(t *testing.T) {
	const esc = "\x1b[2J"
	const bidi = "\u202e"
	long := strings.Repeat("t", 4*statusTitleMaxRunes)
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		return newStatusGit("/p"+esc, []projects.Project{
			{Name: "evil" + esc + bidi + "\nforged", Dir: "/p/evil", Status: projects.GitStatus{}},
		}), nil, nil
	}
	src.PRs = func(context.Context) (prDashJSON, []string, error) {
		return prDashJSON{AwaitingYou: []prRowJSON{{Ref: "o/r#1" + esc, Title: "t" + esc + bidi + "\n" + long}}}, nil, nil
	}
	src.Clean = func(context.Context) (statusCleanJSON, []string, error) {
		return statusCleanJSON{Root: "/c" + bidi}, nil, nil
	}
	src.Bench = func(context.Context) (bench.Report, []string, error) {
		return bench.Report{
			Hearth:    bench.Component{Name: "hearth" + esc, State: bench.State("odd" + esc), Reason: "r" + esc + "\n" + strings.Repeat("r", 4*statusReasonMaxRunes)},
			Chronicle: bench.Component{Name: "chronicle", State: bench.StateOK},
		}, nil, nil
	}
	out, _, err := runStatus(t, src)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.ContainsAny(out, "\x1b\u202e") {
		t.Errorf("text carries a raw control:\n%q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "forged") || strings.HasPrefix(line, long[:10]) || strings.HasPrefix(line, "rrrr") {
			t.Errorf("an embedded newline forged a line: %q", line)
		}
		if n := len([]rune(line)); n > 400 {
			t.Errorf("line of %d runes escaped its cap: %q", n, line)
		}
	}
	if strings.Contains(out, strings.Repeat("t", statusTitleMaxRunes+1)) {
		t.Errorf("a PR title ran past its %d-rune cap:\n%s", statusTitleMaxRunes, out)
	}
	if strings.Contains(out, strings.Repeat("r", statusReasonMaxRunes+1)) {
		t.Errorf("a bench reason ran past its %d-rune cap:\n%s", statusReasonMaxRunes, out)
	}
	if !strings.Contains(out, "[status unknown]") {
		t.Errorf("an unknown tree must say so, never read as clean:\n%s", out)
	}
}

func TestStatusText_ListsAreCapped(t *testing.T) {
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		var ps []projects.Project
		for range statusProjectRowsMax + 3 {
			ps = append(ps, projects.Project{Name: "d", Status: projects.GitStatus{State: projects.StatusOK, Untracked: 1}})
		}
		return newStatusGit("/p", ps), nil, nil
	}
	src.PRs = func(context.Context) (prDashJSON, []string, error) {
		rows := make([]prRowJSON, statusPRRowsMax+2)
		for i := range rows {
			rows[i] = prRowJSON{Ref: "o/r#1"}
		}
		return prDashJSON{AwaitingYou: rows}, nil, nil
	}
	out, _, err := runStatus(t, src)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got := strings.Count(out, `    "d"  [1 untracked]`); got != statusProjectRowsMax {
		t.Errorf("listed %d project rows, want %d", got, statusProjectRowsMax)
	}
	if !strings.Contains(out, "… 3 more (status --json lists every project)") {
		t.Errorf("project overflow not reported:\n%s", out)
	}
	if got := strings.Count(out, "    o/r#1  "); got != statusPRRowsMax {
		t.Errorf("listed %d PR rows, want %d", got, statusPRRowsMax)
	}
	if !strings.Contains(out, "… 2 more (pr dash)") {
		t.Errorf("PR overflow not reported:\n%s", out)
	}
}

func TestNewStatusGit_CountsEveryState(t *testing.T) {
	g := newStatusGit("/p", []projects.Project{
		{Status: projects.GitStatus{State: projects.StatusOK}},
		{Status: projects.GitStatus{State: projects.StatusOK, Modified: 1, Ahead: 2}},
		{Status: projects.GitStatus{State: projects.StatusOK, Ahead: 1}},
		{Status: projects.GitStatus{State: projects.StatusOK, Untracked: 3}},
		{Status: projects.GitStatus{State: projects.StatusNotRepo}},
		{Status: projects.GitStatus{}},
	})
	want := statusGitJSON{Root: "/p", Total: 6, Clean: 1, Dirty: 2, Ahead: 2, Unknown: 1, NotARepo: 1}
	if len(g.Projects) != 6 {
		t.Errorf("projects = %d rows, want every row listed", len(g.Projects))
	}
	g.Projects = nil
	if !reflect.DeepEqual(g, want) {
		t.Errorf("counts = %+v, want %+v", g, want)
	}
}

// TestStatus_DefaultGitSourceReadsTheProjectsRoot drives the shipped git
// source against a temp projects root, so the wiring (not just the fold) is
// covered: a plain directory under the root is a not-a-repo row and no git
// runs for it.
func TestStatus_DefaultGitSourceReadsTheProjectsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "plain"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROJECTS_DIR", root)
	runner := &exec.FakeRunner{}
	src := defaultStatusSources(module.Deps{Runner: runner})
	g, notes, err := src.Git(t.Context())
	if err != nil || len(notes) != 0 {
		t.Fatalf("git source: %v %v", err, notes)
	}
	if g.Root != root || g.Total != 1 || g.NotARepo != 1 {
		t.Errorf("git = %+v, want one not-a-repo row under %s", g, root)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("git ran for a plain directory: %+v", runner.Calls)
	}
}

// TestStatus_DefaultGitSourceFailsOnAMissingRoot pins the git source to
// local discovery: a projects root that does not exist fails the section with
// the discovery error, and nothing is spawned in its place.
func TestStatus_DefaultGitSourceFailsOnAMissingRoot(t *testing.T) {
	t.Setenv("PROJECTS_DIR", filepath.Join(t.TempDir(), "absent"))
	runner := &exec.FakeRunner{}
	s := status.Collect(t.Context(), time.Second, defaultStatusSources(module.Deps{Runner: runner}).Git)
	if s.State != status.StateFailed || !strings.Contains(s.Error, "projects directory not found") {
		t.Errorf("section = %+v, want failed on the missing root", s)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("a missing root still spawned: %+v", runner.Calls)
	}
}

// TestRenderStatus_ASectionWithNoDataPrintsAsFailed covers the render guard
// directly: status.Collect never pairs a non-failed state with nil data, but
// the renderer must not dereference nil if a section ever arrives that way.
func TestRenderStatus_ASectionWithNoDataPrintsAsFailed(t *testing.T) {
	var r statusReportJSON
	r.Git = status.Section[statusGitJSON]{State: status.StateOK}
	r.PRs = status.Section[prDashJSON]{State: status.StateDegraded, Notes: []string{"n"}}
	r.Clean = status.Section[statusCleanJSON]{State: status.StateOK}
	r.Bench = status.Section[bench.Report]{State: status.StateOK}
	var buf bytes.Buffer
	renderStatus(&buf, r, theme.Theme{}.Marks())
	if got := strings.Count(buf.String(), "failed: "); got != 4 {
		t.Errorf("printed %d failed sections, want 4:\n%s", got, buf.String())
	}
}

// TestStatus_SectionsRunConcurrently gives two sources a rendezvous: each
// signals its start and waits for the other's. Run one after the other, the
// first would time out waiting and fail its section.
func TestStatus_SectionsRunConcurrently(t *testing.T) {
	gitStarted, prsStarted := make(chan struct{}), make(chan struct{})
	meet := func(mine, theirs chan struct{}) error {
		close(mine)
		select {
		case <-theirs:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other section never started")
		}
	}
	src := okStatusSources()
	inGit, inPRs := src.Git, src.PRs
	src.Git = func(ctx context.Context) (statusGitJSON, []string, error) {
		if err := meet(gitStarted, prsStarted); err != nil {
			return statusGitJSON{}, nil, err
		}
		return inGit(ctx)
	}
	src.PRs = func(ctx context.Context) (prDashJSON, []string, error) {
		if err := meet(prsStarted, gitStarted); err != nil {
			return prDashJSON{}, nil, err
		}
		return inPRs(ctx)
	}
	r := collectStatus(t.Context(), src, 30*time.Second)
	if r.Git.State != status.StateOK || r.PRs.State != status.StateOK {
		t.Fatalf("git = %+v, prs = %+v; want both ok when run concurrently", r.Git, r.PRs)
	}
}

// TestStatus_DefaultCleanSourceNeverDeletes runs the shipped clean source
// over a real temp tree and checks every file survives, while the section
// still reports what an apply would reclaim.
func TestStatus_DefaultCleanSourceNeverDeletes(t *testing.T) {
	root := t.TempDir()
	files := []string{
		filepath.Join(root, "proj", "node_modules", "a.js"),
		filepath.Join(root, "proj", "dist", "b.js"),
		filepath.Join(root, "other", "target", "c.o"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	deps.Cfg.Clean.DefaultRoot = root
	c, notes, err := defaultStatusSources(deps).Clean(t.Context())
	if err != nil || len(notes) != 0 {
		t.Fatalf("clean source: %v %v", err, notes)
	}
	if c.Reclaimable != 3 || c.TotalReclaimableBytes != 30 {
		t.Errorf("clean = %+v, want 3 targets and 30 bytes reclaimable", c)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("status deleted %s: %v", f, err)
		}
	}
}

// TestStatus_DefaultPRsSourceReadsTheDashboard drives the shipped prs source
// through a fake gh, so a source that stopped calling Dash (or dropped its
// rows) goes red.
func TestStatus_DefaultPRsSourceReadsTheDashboard(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("HOME", cfgHome)
	search := "[" + prSearchRow("cameronsjo/forgectl", 42) + "]"
	d, notes, err := defaultStatusSources(module.Deps{Runner: dashRunner(search)}).PRs(t.Context())
	if err != nil || len(notes) != 0 {
		t.Fatalf("prs source: %v %v", err, notes)
	}
	if len(d.AwaitingYou) != 1 || d.AwaitingYou[0].Ref != "cameronsjo/forgectl#42" {
		t.Errorf("awaiting_you = %+v, want the searched PR", d.AwaitingYou)
	}
	if len(d.YourOpen) != 1 || d.ActiveReviews == nil {
		t.Errorf("your_open = %+v, active_reviews = %v; want the dash document", d.YourOpen, d.ActiveReviews)
	}
}
