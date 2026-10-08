package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// cockpitRecorder is a statusTUIRuntime whose terminal checks answer as told
// and whose cockpit records what it was opened with.
type cockpitRecorder struct {
	stdinTTY, stdoutTTY bool
	opened              int
	opts                tui.CockpitOptions
	inCockpit           func(opts tui.CockpitOptions) tui.Action
	ran                 [][]string
}

func (c *cockpitRecorder) runtime() statusTUIRuntime {
	return statusTUIRuntime{
		stdinIsTerminal:  func(io.Reader) bool { return c.stdinTTY },
		stdoutIsTerminal: func(io.Writer) bool { return c.stdoutTTY },
		run: func(_ context.Context, opts tui.CockpitOptions) (tui.Action, error) {
			c.opened++
			c.opts = opts
			if c.inCockpit != nil {
				return c.inCockpit(opts), nil
			}
			return tui.Action{}, nil
		},
		runVerb: func(_ *cobra.Command, _ theme.Theme, argv []string) error {
			c.ran = append(c.ran, argv)
			return nil
		},
	}
}

func runStatusTUI(t *testing.T, src statusSources, rec *cockpitRecorder, args ...string) (string, string, error) {
	t.Helper()
	cmd := newStatusCmdWith(src, theme.Theme{}, rec.runtime())
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestStatusTUI_NeedsATerminalOnBothEnds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stdin, stdout bool
	}{
		{"no stdin terminal", false, true},
		{"no stdout terminal", true, false},
		{"neither", false, false},
	} {
		rec := &cockpitRecorder{stdinTTY: tc.stdin, stdoutTTY: tc.stdout}
		out, _, err := runStatusTUI(t, okStatusSources(), rec, "--tui")
		if err == nil {
			t.Errorf("%s: no error; the cockpit opened %d time(s)", tc.name, rec.opened)
			continue
		}
		if !errors.Is(err, errStatusTUINeedsTerminal) || ExitCode(err) == 0 {
			t.Errorf("%s: err = %v, want the terminal refusal with a non-zero exit", tc.name, err)
		}
		if !strings.Contains(err.Error(), "--json") {
			t.Errorf("%s: refusal %q does not point at --json", tc.name, err)
		}
		if rec.opened != 0 || out != "" {
			t.Errorf("%s: the cockpit opened (%d) or a report printed (%q)", tc.name, rec.opened, out)
		}
	}
}

// TestStatusTUI_RefusesUnderGoTest is the production gate itself: go test's
// stdin and stdout are not terminals.
func TestStatusTUI_RefusesUnderGoTest(t *testing.T) {
	_, _, err := runStatus(t, okStatusSources(), "--tui")
	if !errors.Is(err, errStatusTUINeedsTerminal) {
		t.Errorf("err = %v, want the terminal refusal", err)
	}
}

func TestStatusTUI_ConflictsWithJSONAndStrict(t *testing.T) {
	for _, args := range [][]string{{"--tui", "--json"}, {"--tui", "--strict"}} {
		rec := &cockpitRecorder{stdinTTY: true, stdoutTTY: true}
		_, _, err := runStatusTUI(t, okStatusSources(), rec, args...)
		if err == nil || ExitCode(err) == 0 {
			t.Errorf("%v: err = %v, want a refusal", args, err)
		}
		if rec.opened != 0 {
			t.Errorf("%v: the cockpit opened", args)
		}
	}
}

func TestStatusTUI_OpensWithFourSectionsAndOnlyGitOnTheTimer(t *testing.T) {
	rec := &cockpitRecorder{stdinTTY: true, stdoutTTY: true}
	out, _, err := runStatusTUI(t, okStatusSources(), rec, "--tui")
	if err != nil || rec.opened != 1 {
		t.Fatalf("err = %v, opened = %d; want the cockpit opened once", err, rec.opened)
	}
	if out != "" {
		t.Errorf("the text report printed under --tui: %q", out)
	}
	var names []string
	for _, s := range rec.opts.Sources {
		names = append(names, s.Name)
		if s.Auto != (s.Name == "git") {
			t.Errorf("%s: Auto = %v; only git refreshes on the timer", s.Name, s.Auto)
		}
	}
	if strings.Join(names, " ") != "git prs clean bench" {
		t.Errorf("sections = %v, want git prs clean bench in that order", names)
	}
	if rec.opts.BuildArgv == nil {
		t.Error("the cockpit got no argv builder: a PR row would bypass the hub's tree check")
	}
	for _, s := range rec.opts.Sources {
		sec, done := s.Load(context.Background())
		if done != nil {
			<-done
		}
		if sec.Name != s.Name || sec.State != tui.CockpitOK || sec.Headline == "" {
			t.Errorf("%s loaded %+v, want an ok section with its headline", s.Name, sec)
		}
	}
}

// TestStatusTUI_TimeoutIsPerSection: --timeout bounds each cockpit loader as
// it bounds each text section, and the loader stays tracked past it.
func TestStatusTUI_TimeoutIsPerSection(t *testing.T) {
	src := okStatusSources()
	release := make(chan struct{})
	src.Bench = func(context.Context) (bench.Report, []string, error) {
		<-release
		return bench.Report{}, nil, nil
	}
	rec := &cockpitRecorder{stdinTTY: true, stdoutTTY: true}
	if _, _, err := runStatusTUI(t, src, rec, "--tui", "--timeout", "50ms"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	sec, done := rec.opts.Sources[statusIdxBench].Load(context.Background())
	if time.Since(start) > 5*time.Second || sec.State != tui.CockpitFailed || sec.Error != "timed out after 50ms" {
		t.Fatalf("bench = %+v, want failed on its own 50ms deadline", sec)
	}
	if done == nil {
		close(release)
		t.Fatal("the loader returned no done channel: the cockpit could not keep the section busy")
	}
	select {
	case <-done:
		t.Fatal("done closed while the bench source was still running")
	default:
	}
	close(release)
	<-done
	if git, _ := rec.opts.Sources[statusIdxGit].Load(context.Background()); git.State != tui.CockpitOK {
		t.Errorf("git = %+v; one stalled section must not touch another", git)
	}
}

func TestStatusTUI_RunsThePRTheOperatorChose(t *testing.T) {
	rec := &cockpitRecorder{stdinTTY: true, stdoutTTY: true, inCockpit: func(tui.CockpitOptions) tui.Action {
		return tui.Action{Kind: tui.ActionRunVerb, Argv: []string{"pr", "o/r#1"}}
	}}
	if _, _, err := runStatusTUI(t, okStatusSources(), rec, "--tui"); err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 1 || strings.Join(rec.ran[0], " ") != "pr o/r#1" {
		t.Errorf("ran = %v, want [pr o/r#1] after the cockpit closed", rec.ran)
	}
	quiet := &cockpitRecorder{stdinTTY: true, stdoutTTY: true}
	if _, _, err := runStatusTUI(t, okStatusSources(), quiet, "--tui"); err != nil || len(quiet.ran) != 0 {
		t.Errorf("a plain quit ran %v (err %v), want nothing", quiet.ran, err)
	}
}

// TestStatusTUI_HeadlinesMatchTheTextView is the parity pin: the cockpit and
// the text view print the same headline for the same report, for ok,
// degraded and failed sections.
func TestStatusTUI_HeadlinesMatchTheTextView(t *testing.T) {
	for name, src := range map[string]statusSources{"ok": okStatusSources(), "failing": failingStatusSources(t)} {
		r := collectStatus(context.Background(), src, 50*time.Millisecond)
		text := statusTextHeadlines(t, r)
		snap := cockpitSnapshot(r)
		if len(snap.Sections) != len(text) {
			t.Fatalf("%s: cockpit has %d sections, text view %d", name, len(snap.Sections), len(text))
		}
		for i, sec := range snap.Sections {
			cockpit := sec.Headline
			if sec.State == tui.CockpitFailed {
				cockpit = "failed: " + sec.Error // the cockpit's own failed line
			}
			if cockpit != text[i] {
				t.Errorf("%s: %s cockpit headline %q != text %q", name, sec.Name, cockpit, text[i])
			}
		}
	}
}

func TestCockpitSnapshot_Rows(t *testing.T) {
	src := okStatusSources()
	src.Git = func(context.Context) (statusGitJSON, []string, error) {
		return newStatusGit("/p", []projects.Project{
			{Name: "tidy", Dir: "/p/tidy", Status: projects.GitStatus{State: projects.StatusOK}},
			{Name: "plain", Dir: "/p/plain", Status: projects.GitStatus{State: projects.StatusNotRepo}},
			{Name: "odd", Dir: "/p/odd", Status: projects.GitStatus{State: projects.StatusUnknown}},
		}), nil, nil
	}
	src.PRs = func(context.Context) (prDashJSON, []string, error) {
		return prDashJSON{
			ActiveReviews: []prDashReviewJSON{{Ref: "o/r#7", Phase: "running"}},
			AwaitingYou:   []prRowJSON{{Ref: "o/r#1", Title: "a"}},
			YourOpen:      []prRowJSON{{Ref: "o/r#2", Title: "b"}},
		}, nil, nil
	}
	snap := cockpitSnapshot(collectStatus(context.Background(), src, time.Second))
	git := snap.Sections[statusIdxGit].Rows
	if len(git) != 2 || git[0].Kind != tui.CockpitRowProject || git[0].Path != "/p/tidy" || git[1].Detail != "[status unknown]" {
		t.Errorf("git rows = %+v, want tidy and odd (a plain directory is not a row)", git)
	}
	prs := snap.Sections[statusIdxPRs].Rows
	if len(prs) != 3 || prs[0].Ref != "o/r#1" || prs[1].Ref != "o/r#2" || prs[0].Kind != tui.CockpitRowPR || prs[2].Kind != tui.CockpitRowInfo {
		t.Errorf("prs rows = %+v, want awaiting, yours (both PR rows), then the active review as info", prs)
	}
	if b := snap.Sections[statusIdxBench].Rows; len(b) != 2 || b[1].Detail != "unavailable — down" {
		t.Errorf("bench rows = %+v", b)
	}
	if c := snap.Sections[statusIdxClean]; len(c.Rows) != 0 || c.Headline == "" {
		t.Errorf("clean = %+v, want a headline and no rows", c)
	}
}

// TestStatusTUI_AFailingChosenVerbIsRenderedOnce drives the production
// runVerb (deferHubVerb) through execDispatch and fang with a verb that
// fails. The chosen verb must run after status's own fang frame has
// returned: run from inside status's RunE, the inner fang renders the error
// and the outer renders it again.
//
// Mutation that turns it red: make deferHubVerb call runHubVerb directly.
func TestStatusTUI_AFailingChosenVerbIsRenderedOnce(t *testing.T) {
	rt := productionStatusTUIRuntime()
	rt.stdinIsTerminal = func(io.Reader) bool { return true }
	rt.stdoutIsTerminal = func(io.Writer) bool { return true }
	rt.run = func(context.Context, tui.CockpitOptions) (tui.Action, error) {
		return tui.Action{Kind: tui.ActionRunVerb, Argv: []string{"boom"}}, nil
	}
	root := &cobra.Command{Use: "forgectl", SilenceUsage: true}
	root.AddCommand(newStatusCmdWith(okStatusSources(), theme.Theme{}, rt))
	ran := 0
	root.AddCommand(&cobra.Command{Use: "boom", RunE: func(*cobra.Command, []string) error {
		ran++
		return WithExitCode(errors.New("boom failed"), 3)
	}})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := execDispatch(context.Background(), module.Deps{}, root, []string{"status", "--tui"}, theme.Theme{})
	if ran != 1 {
		t.Fatalf("the chosen verb ran %d times, want 1", ran)
	}
	if got := ExitCode(err); got != 3 {
		t.Errorf("exit code = %d (err %v), want the verb's own 3", got, err)
	}
	if n := strings.Count(strings.ToLower(stderr.String()), "boom failed"); n != 1 {
		t.Errorf("the verb's error was rendered %d times, want once:\n%s", n, stderr.String())
	}
}

// TestDeferHubVerb_OffTheDispatchPathPrintsTheInvocation: with no
// deferred-verb slot in the context, nothing runs, and the invocation is
// echoed the way the hub echoes one it runs: through tui.DisplayArgv, so an
// argument with a space reads as one quoted argument (forgectl#1003 item 3).
//
// Mutation that turns it red: print through hubDollarLine, which joins the
// elements plainly.
func TestDeferHubVerb_OffTheDispatchPathPrintsTheInvocation(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	argv := []string{"pr", "o/r#1 two"}
	if err := deferHubVerb(cmd, theme.Theme{}, argv); err != nil {
		t.Fatal(err)
	}
	const want = "$ forgectl pr 'o/r#1 two'"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want the quoted invocation %q", stderr.String(), want)
	}
}

// TestDeferHubVerb_ACommandNeverExecutedPrintsTheInvocation: a root that was
// never executed has no context at all; deferring must fall back, not panic.
func TestDeferHubVerb_ACommandNeverExecutedPrintsTheInvocation(t *testing.T) {
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	if err := deferHubVerb(cmd, theme.Theme{}, []string{"pr", "o/r#1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "$ forgectl pr o/r#1") {
		t.Errorf("stderr = %q, want the invocation", stderr.String())
	}
}
