package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const (
	status1204Head = "3afe70e8bff88488d3aebd691ffb943829bf676c"
	status1204Base = "a398e7258d0a755b14f2599afb277b7b8132b86e"
)

func mergeFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "surface", "merge", "testdata", name)) //nolint:gosec // G304: name is a literal at every call site
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// statusGH answers status's reads for #1204 from the merge package's
// captured fixtures. fail makes every gh call fail.
func statusGH(t *testing.T, fail bool) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if fail {
			return "", errors.New("HTTP 502")
		}
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "viewer { login databaseId }"):
			return mergeFixture(t, "discover_1204.json"), nil
		case strings.Contains(j, "reviewThreads(first: 100)"):
			return mergeFixture(t, "pr_1204.json"), nil
		case strings.Contains(j, "checkSuites(first: 50)"):
			return mergeFixture(t, "checks_1204.json"), nil
		case strings.Contains(j, "compare/"+status1204Base+"..."+status1204Head):
			return mergeFixture(t, "compare_1204.json"), nil
		case strings.Contains(j, "compare/"):
			return mergeFixture(t, "compare_ahead.json"), nil
		case strings.Contains(j, "git/trees/"):
			ref := j[strings.Index(j, "git/trees/")+len("git/trees/"):]
			sha, dir, _ := strings.Cut(ref, ":")
			return mergeFixture(t, "tree_"+sha[:8]+"_"+strings.ReplaceAll(dir, "/", "_")+".json"), nil
		}
		t.Fatalf("unexpected call %s %s", name, j)
		return "", nil
	}}
}

func statusRow1204() worker.Row {
	return worker.Row{
		Name: "gh1175-forgectl", Branch: "worker/gh1175-forgectl", BranchFrom: worker.BranchNew, Stage: worker.StageLaunched,
		Base: "2e469107d375d1977085887813de99ce18bc91bf", GitHubRepo: "cameronsjo/forgectl", GitHubRepoID: 1252924951,
		LaunchID: "launch-abc", Transcript: "/t/session.jsonl",
	}
}

func runStatusCmd(t *testing.T, d statusDeps, args ...string) (string, error) {
	t.Helper()
	cmd := newSurfaceStatusCmdWith(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func statusTestDeps(t *testing.T, row worker.Row, gh *exec.FakeRunner, settings config.MergeSettings) statusDeps {
	q := &worker.QueueRow{Name: row.Name, LaunchID: "launch-abc", State: worker.QueueReported}
	return statusDeps{
		rows: func(context.Context, io.Writer, string, string) (worker.Row, *worker.QueueRow, error) {
			return row, q, nil
		},
		settings: func() config.MergeSettings { return settings },
		reader:   func() merge.Reader { return merge.Reader{GH: gh} },
		usage: func(_ context.Context, transcript string) *statusUsage {
			if transcript != "/t/session.jsonl" {
				t.Errorf("priced %q", transcript)
			}
			return &statusUsage{CostUSD: 1.25, ByModel: json.RawMessage(`{"claude-opus-5-5":{"costUsd":1.25}}`)}
		},
	}
}

func manualSettings() config.MergeSettings {
	return config.MergeSettings{
		Mode: config.MergeManual, Approvers: []string{config.MergeApproverCadenceReview}, MarkerAuthorID: 4084915,
		RequiredReviewers: []string{"polish"}, Method: config.MergeMethodSquash,
		Repos: []config.MergeRepo{{Name: "cameronsjo/forgectl", Workflow: ".github/workflows/ci.yml", RequiredChecks: []string{"build-test", "lint"}, Paths: []string{"docs/**"}}},
	}
}

func TestSurfaceStatusJSON(t *testing.T) {
	out, err := runStatusCmd(t, statusTestDeps(t, statusRow1204(), statusGH(t, false), manualSettings()), "gh1175-forgectl", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Name string `json:"name"`
		Repo string `json:"repo"`
		PR   *struct {
			Number           int    `json:"number"`
			URL              string `json:"url"`
			State            string `json:"state"`
			IsDraft          bool   `json:"isDraft"`
			HeadSha          string `json:"headSha"`
			BaseRef          string `json:"baseRef"`
			Mergeable        string `json:"mergeable"`
			MergeStateStatus string `json:"mergeStateStatus"`
			ChangedFiles     int    `json:"changedFiles"`
		} `json:"pr"`
		Checks  []map[string]string `json:"checks"`
		Reviews []struct {
			Reviewer string `json:"reviewer"`
			Sha      string `json:"sha"`
			Crit     int    `json:"crit"`
			Imp      int    `json:"imp"`
			URL      string `json:"url"`
		} `json:"reviews"`
		Policy struct {
			Mode    string   `json:"mode"`
			Verdict string   `json:"verdict"`
			Reasons []string `json:"reasons"`
		} `json:"policy"`
		Usage *struct {
			CostUSD float64        `json:"costUsd"`
			ByModel map[string]any `json:"byModel"`
		} `json:"usage"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if v.Name != "gh1175-forgectl" || v.Repo != "cameronsjo/forgectl" || v.PR == nil || v.PR.Number != 1204 || v.PR.HeadSha != status1204Head ||
		v.PR.State != "MERGED" || v.PR.BaseRef != "main" || v.PR.ChangedFiles != 4 || !strings.HasSuffix(v.PR.URL, "/pull/1204") {
		t.Fatalf("pr %+v in %s", v.PR, out)
	}
	if len(v.Checks) != 6 || v.Checks[0]["workflow"] == "" || v.Checks[0]["event"] != "pull_request" {
		t.Fatalf("checks %+v", v.Checks)
	}
	if len(v.Reviews) != 1 || v.Reviews[0].Reviewer != "chief-of-staff" || v.Reviews[0].Sha != status1204Head || v.Reviews[0].URL == "" {
		t.Fatalf("reviews %+v", v.Reviews)
	}
	if v.Policy.Mode != "manual" || v.Policy.Verdict != "refuse" || !strings.Contains(strings.Join(v.Policy.Reasons, "\n"), "the PR is MERGED") {
		t.Fatalf("policy %+v", v.Policy)
	}
	if v.Usage == nil || v.Usage.CostUSD != 1.25 {
		t.Fatalf("usage %+v", v.Usage)
	}
	for _, key := range []string{`"name"`, `"repo"`, `"pr"`, `"checks"`, `"reviews"`, `"policy"`, `"usage"`, `"isDraft"`, `"mergeStateStatus"`, `"costUsd"`, `"byModel"`} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON lacks %s", key)
		}
	}
}

func TestSurfaceStatusText(t *testing.T) {
	out, err := runStatusCmd(t, statusTestDeps(t, statusRow1204(), statusGH(t, false), manualSettings()), "gh1175-forgectl")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cameronsjo/forgectl#1204 MERGED head 3afe70e8bff8", "checks: ", "markers: chief-of-staff 3afe70e8bff8 crit=0 imp=0", "policy: manual, refuse", "  - the PR is MERGED", "cost: $1.25"} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}

func TestSurfaceStatusOutcomes(t *testing.T) {
	t.Run("mode off is a verdict, exit 0", func(t *testing.T) {
		off := config.MergeSettings{Mode: config.MergeOff, OffReason: "[surface.merge] is not set"}
		out, err := runStatusCmd(t, statusTestDeps(t, statusRow1204(), statusGH(t, false), off), "gh1175-forgectl", "--json")
		if err != nil || !strings.Contains(out, `"verdict": "off"`) || !strings.Contains(out, "is not set") {
			t.Fatalf("%v: %s", err, out)
		}
	})
	t.Run("GitHub unreadable exits 1", func(t *testing.T) {
		_, err := runStatusCmd(t, statusTestDeps(t, statusRow1204(), statusGH(t, true), manualSettings()), "gh1175-forgectl")
		if err == nil || ExitCode(err) != 1 || !errors.Is(err, merge.ErrRead) {
			t.Fatalf("err %v, exit %d", err, ExitCode(err))
		}
	})
	t.Run("a row with no recorded repository exits 2", func(t *testing.T) {
		row := statusRow1204()
		row.GitHubRepo, row.GitHubRepoID = "", 0
		_, err := runStatusCmd(t, statusTestDeps(t, row, statusGH(t, false), manualSettings()), "gh1175-forgectl")
		if ExitCode(err) != exitUsage || !strings.Contains(err.Error(), "records no GitHub repository") {
			t.Fatalf("err %v, exit %d", err, ExitCode(err))
		}
	})
	t.Run("a bad name exits 2", func(t *testing.T) {
		_, err := runStatusCmd(t, statusTestDeps(t, statusRow1204(), statusGH(t, false), manualSettings()), "Bad Name")
		if ExitCode(err) != exitUsage {
			t.Fatalf("err %v, exit %d", err, ExitCode(err))
		}
	})
	t.Run("no PR on the branch", func(t *testing.T) {
		row := statusRow1204()
		row.Name, row.Branch = "other", "worker/other"
		out, err := runStatusCmd(t, statusTestDeps(t, row, statusGH(t, false), manualSettings()), "other", "--json")
		if err != nil || !strings.Contains(out, `"pr": null`) || !strings.Contains(out, "no PR: merge: no pull request on the worker's branch") || !strings.Contains(out, `"verdict": "refuse"`) {
			t.Fatalf("%v: %s", err, out)
		}
		text, err := runStatusCmd(t, statusTestDeps(t, row, statusGH(t, false), manualSettings()), "other")
		if err != nil || !strings.Contains(text, "no PR") {
			t.Fatalf("%v: %s", err, text)
		}
	})
	t.Run("no transcript, no usage", func(t *testing.T) {
		row := statusRow1204()
		row.Transcript = ""
		d := statusTestDeps(t, row, statusGH(t, false), manualSettings())
		d.usage = func(context.Context, string) *statusUsage { t.Fatal("priced with no transcript"); return nil }
		out, err := runStatusCmd(t, d, "gh1175-forgectl", "--json")
		if err != nil || !strings.Contains(out, `"usage": null`) {
			t.Fatalf("%v: %s", err, out)
		}
	})
}

func TestPriceTranscript(t *testing.T) {
	ctx := context.Background()
	found := func(string) (string, error) { return "/opt/bin/cadence-hooks", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	answer := func(out string, err error) *exec.FakeRunner {
		return &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, err }}
	}
	good := answer(`{"costUsd":2.5,"byModel":{"claude-opus-5-5":{"costUsd":2.5}},"unpricedModels":[]}`, nil)
	u := priceTranscript(ctx, good, found, "/t/s.jsonl")
	if u == nil || u.CostUSD != 2.5 || !strings.Contains(string(u.ByModel), "claude-opus-5-5") {
		t.Fatalf("good: %+v", u)
	}
	if c := good.Calls[0]; c.Name != "/opt/bin/cadence-hooks" || strings.Join(c.Args, " ") != "metrics price --transcript /t/s.jsonl --json" {
		t.Fatalf("call %+v", c)
	}
	for name, run := range map[string]*exec.FakeRunner{
		"exit 1":        answer("", errors.New("exit status 1")),
		"not JSON":      answer("nope", nil),
		"no costUsd":    answer(`{"byModel":{}}`, nil),
		"negative cost": answer(`{"costUsd":-1,"byModel":{}}`, nil),
		"byModel array": answer(`{"costUsd":1,"byModel":[1]}`, nil),
	} {
		if u := priceTranscript(ctx, run, found, "/t/s.jsonl"); u != nil {
			t.Errorf("%s: %+v, want nil", name, u)
		}
	}
	never := answer("", nil)
	if u := priceTranscript(ctx, never, missing, "/t/s.jsonl"); u != nil || len(never.Calls) != 0 {
		t.Fatalf("not on PATH: %+v, %d calls", u, len(never.Calls))
	}
}
