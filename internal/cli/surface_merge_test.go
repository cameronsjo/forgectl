package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// exitOf is the process exit code err gives: 0 for nil.
func exitOf(err error) int {
	if err == nil {
		return 0
	}
	return ExitCode(err)
}

// mergeCall is one call into the stubbed merge path.
type mergeCall struct {
	settings config.MergeSettings
	by       merge.By
	row      merge.Row
	dryRun   bool
}

func mergeTestDeps(row worker.Row, settings config.MergeSettings, out merge.Outcome, calls *[]mergeCall) mergeDeps {
	q := &worker.QueueRow{Name: row.Name, LaunchID: "launch-abc", State: worker.QueueReported}
	return mergeDeps{
		rows: func(context.Context, io.Writer, string, string) (worker.Row, *worker.QueueRow, error) {
			return row, q, nil
		},
		settings: func() config.MergeSettings { return settings },
		land: func() (landFunc, error) {
			return func(_ context.Context, s config.MergeSettings, by merge.By, r merge.Row, dryRun bool) merge.Outcome {
				*calls = append(*calls, mergeCall{s, by, r, dryRun})
				return out
			}, nil
		},
	}
}

func runMergeCmd(t *testing.T, d mergeDeps, args ...string) (string, error) {
	t.Helper()
	cmd := newSurfaceMergeCmdWith(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSurfaceMergeExitCodes(t *testing.T) {
	cases := map[string]struct {
		out      merge.Outcome
		code     int
		inErr    string
		inOutput string
	}{
		"merged":      {merge.Outcome{Result: merge.LandMerged, PR: 1204, Head: status1204Head, MergeCommit: "1111111111111111111111111111111111111111", AuditLine: "abc"}, 0, "", "merge commit: 1111111111111111111111111111111111111111"},
		"would merge": {merge.Outcome{Result: merge.LandWouldMerge, PR: 1204}, 0, "", ": would-merge"},
		"refused":     {merge.Outcome{Result: merge.LandRefused, PR: 1204, Reasons: []string{"the PR is a draft, expected ready for review"}}, 1, "merge refused (1 reasons above)", "  - the PR is a draft"},
		"unreadable":  {merge.Outcome{Result: merge.LandUnreadable, Reasons: []string{"GitHub could not be read: HTTP 502"}}, 1, "GitHub could not be read; nothing was merged", "HTTP 502"},
		"failed":      {merge.Outcome{Result: merge.LandFailed, PR: 1204, Reasons: []string{"gh pr merge failed: x"}}, 1, "the merge failed", ""},
		"unconfirmed": {merge.Outcome{Result: merge.LandUnconfirmed, PR: 1204}, 1, "could not be confirmed as this attempt's on the default branch", ""},
		"merged elsewhere": {merge.Outcome{Result: merge.LandMergedElsewhere, PR: 1204, MergeCommit: "1111111111111111111111111111111111111111"}, 1,
			"not by this attempt", ": merged-elsewhere"},
		"unknown": {merge.Outcome{Result: merge.LandUnknown, PR: 1204}, 1, "the PR may have merged: check it by hand", ": merge-unknown"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var calls []mergeCall
			out, err := runMergeCmd(t, mergeTestDeps(statusRow1204(), manualSettings(), c.out, &calls), "gh1175-forgectl")
			if got := exitOf(err); got != c.code {
				t.Fatalf("exit %d (%v); want %d", got, err, c.code)
			}
			if c.inErr != "" && !strings.Contains(err.Error(), c.inErr) {
				t.Fatalf("error %q; want %q", err, c.inErr)
			}
			if !strings.Contains(out, c.inOutput) {
				t.Fatalf("output %q; want %q", out, c.inOutput)
			}
			if len(calls) != 1 || calls[0].by != merge.ByCLI || calls[0].dryRun || calls[0].row.Name != "gh1175-forgectl" ||
				calls[0].row.QueueLaunchID != "launch-abc" || calls[0].row.GitHubRepoID != 1252924951 || calls[0].settings.Mode != config.MergeManual {
				t.Fatalf("calls %+v", calls)
			}
		})
	}
}

func TestSurfaceMergeJSONAndDryRun(t *testing.T) {
	var calls []mergeCall
	out, err := runMergeCmd(t, mergeTestDeps(statusRow1204(), manualSettings(), merge.Outcome{Result: merge.LandRefused, PR: 1204, Reasons: []string{"x"}}, &calls),
		"gh1175-forgectl", "--dry-run", "--json")
	if exitOf(err) != 1 {
		t.Fatalf("exit %d (%v)", exitOf(err), err)
	}
	var v map[string]any
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, k := range []string{"name", "repo", "dry_run", "mode", "result", "pr", "url", "head", "reasons", "merge_commit", "audit_line"} {
		if _, ok := v[k]; !ok {
			t.Errorf("--json lacks %q: %s", k, out)
		}
	}
	if v["dry_run"] != true || v["result"] != "refused" || v["mode"] != "manual" || len(calls) != 1 || !calls[0].dryRun {
		t.Fatalf("%s; calls %+v", out, calls)
	}
}

func TestSurfaceMergeUsage(t *testing.T) {
	var calls []mergeCall
	if _, err := runMergeCmd(t, mergeTestDeps(statusRow1204(), manualSettings(), merge.Outcome{}, &calls), "Not A Name"); exitOf(err) != 2 {
		t.Fatalf("a bad name: exit %d (%v)", exitOf(err), err)
	}
	row := statusRow1204()
	row.GitHubRepo, row.GitHubRepoID = "", 0
	if _, err := runMergeCmd(t, mergeTestDeps(row, manualSettings(), merge.Outcome{}, &calls), "gh1175-forgectl"); exitOf(err) != 2 || !errors.Is(err, merge.ErrNoRecordedRepo) {
		t.Fatalf("no recorded repository: exit %d (%v)", exitOf(err), err)
	}
	d := mergeTestDeps(statusRow1204(), manualSettings(), merge.Outcome{}, &calls)
	d.land = func() (landFunc, error) { return nil, errors.New("no state dir") }
	if _, err := runMergeCmd(t, d, "gh1175-forgectl"); exitOf(err) != 1 {
		t.Fatalf("the audit file cannot be opened: exit %d (%v)", exitOf(err), err)
	}
	if len(calls) != 0 {
		t.Fatalf("the merge path ran: %+v", calls)
	}
}

// TestGhInNeutralDir pins where gh pr merge runs: a new absolute temporary
// directory, not the process's working directory, removed after, through the
// host pin; and a runner that cannot choose a directory refuses.
func TestGhInNeutralDir(t *testing.T) {
	fake := &exec.FakeRunner{}
	gh := githubauth.Runner(fake, githubauth.DefaultHost)
	made := ""
	removed := ""
	mkdir := func(dir, pattern string) (string, error) {
		d, err := os.MkdirTemp(t.TempDir(), pattern)
		made = d
		return d, err
	}
	remove := func(d string) error { removed = d; return os.RemoveAll(d) } //nolint:gosec // G703: d is the directory the test just made under t.TempDir()
	args := []string{"pr", "merge", "1"}
	if err := ghInNeutralDir(context.Background(), gh, args, mkdir, remove); err != nil {
		t.Fatal(err)
	}
	last := fake.Last()
	cwd, _ := os.Getwd()
	if last.Name != "gh" || !slices.Equal(last.Args, args) || last.Dir != made || !filepath.IsAbs(last.Dir) || last.Dir == cwd || removed != made {
		t.Fatalf("call %+v; made %q removed %q cwd %q", last, made, removed, cwd)
	}
	if !strings.HasPrefix(filepath.Base(made), "forgectl-merge-") || last.Env["GH_HOST"] != githubauth.DefaultHost {
		t.Fatalf("dir %q, GH_HOST %q", made, last.Env["GH_HOST"])
	}
	if err := ghInNeutralDir(context.Background(), noDirRunner{}, args, mkdir, remove); !errors.Is(err, errNoDirRunner) {
		t.Fatalf("a runner with no directory path: %v", err)
	}
}

// noDirRunner is an exec.Runner with no RunWithEnvFilteredInDir.
type noDirRunner struct{ exec.Runner }

func TestRealLanderReadsWithNoCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	l, err := realLander(&exec.FakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Read == nil || l.Recheck == nil || l.Landed == nil || l.Merge == nil || l.Audit == nil || l.Now == nil || l.Sleep == nil || l.AuditCap != worker.MaxMergeAuditBytes {
		t.Fatalf("an unwired seam: %+v", l)
	}
}

func TestSurfaceAudit(t *testing.T) {
	line := func(pr int, result string, existing []byte) []byte {
		data, _, _, err := merge.AppendAudit(existing, merge.AuditLine{Time: "2026-10-09T21:00:00Z", Actor: merge.ByCLI, Repo: "cameronsjo/forgectl", RepoID: 1,
			PR: pr, Head: status1204Head, Result: result, Reasons: []string{"why \x1b[31m"}})
		if err != nil {
			t.Fatal(err)
		}
		return append(existing, data...)
	}
	file := line(1204, merge.AuditRefused, nil)
	file = line(1, merge.AuditRefused, file)
	file = line(1204, merge.AuditMerging, file)
	run := func(data []byte, args ...string) (string, error) {
		cmd := newSurfaceAuditCmdWith(func() ([]byte, error) { return data, nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := run(file, "--pr", "CameronSjo/Forgectl#1204")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Count(out, "line ") != 2 || !strings.Contains(out, "line 1 ") || !strings.Contains(out, "line 3 ") || !strings.Contains(out, "chain: holds over 3 lines") {
		t.Fatalf("output:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("an escape reached the terminal: %q", out)
	}
	out, err = run(file, "--pr", "cameronsjo/forgectl#1204", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Lines []struct {
			Line   int    `json:"line"`
			Hash   string `json:"hash"`
			Result string `json:"result"`
		} `json:"lines"`
		Chain struct {
			OK    bool `json:"ok"`
			Lines int  `json:"lines"`
		} `json:"chain"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.Lines) != 2 || !v.Chain.OK || v.Chain.Lines != 3 || v.Lines[1].Result != merge.AuditMerging || len(v.Lines[1].Hash) != 64 {
		t.Fatalf("%v: %s", err, out)
	}
	// A line removed from the middle breaks the chain: printed, then exit 1.
	parts := strings.SplitAfter(string(file), "\n")
	out, err = run([]byte(parts[0]+parts[2]), "--pr", "cameronsjo/forgectl#1204")
	if exitOf(err) != 1 || !strings.Contains(out, "chain: broken at line 2 of 2") {
		t.Fatalf("a broken chain: exit %d (%v): %s", exitOf(err), err, out)
	}
	if _, err := run(file, "--pr", "cameronsjo/forgectl"); exitOf(err) != 2 {
		t.Fatalf("no number: exit %d (%v)", exitOf(err), err)
	}
	if out, err := run(nil, "--pr", "a/b#1"); err != nil || !strings.Contains(out, "no audit lines for a/b#1") {
		t.Fatalf("no file: %v %q", err, out)
	}
	cmd := newSurfaceAuditCmdWith(func() ([]byte, error) { return nil, errors.New("symlink") })
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--pr", "a/b#1"})
	if err := cmd.Execute(); exitOf(err) != 1 {
		t.Fatalf("an unreadable file: exit %d (%v)", exitOf(err), err)
	}
}
