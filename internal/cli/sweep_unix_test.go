// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/sweep"
)

const sweepHead = "cccccccccccccccccccccccccccccccccccccccc"

var sweepNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// sweepEnv is attnEnv plus a briefs directory holding two workers.
type sweepEnv struct {
	attnEnv
	briefs string
}

func newSweepEnv(t *testing.T) sweepEnv {
	t.Helper()
	e := sweepEnv{attnEnv: newAttnEnv(t), briefs: filepath.Join(t.TempDir(), "briefs")}
	oldNow := attentionNow
	attentionNow = func() time.Time { return sweepNow }
	t.Cleanup(func() { attentionNow = oldNow })
	body := "# Brief\n\n## Where to work\n\n- `acme/widget`: repo `/x/widget`\n\n## Phase 1\n"
	for _, w := range []struct{ name, extra string }{{"cos-widget", ""}, {"cos-gadget", ""}} {
		dir := filepath.Join(e.briefs, w.name)
		b := body
		if w.name == "cos-gadget" {
			b = strings.ReplaceAll(body, "widget", "gadget")
		}
		put := func(file, content string, age time.Duration) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, file)
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			at := sweepNow.Add(-age)
			if err := os.Chtimes(p, at, at); err != nil {
				t.Fatal(err)
			}
		}
		put("brief.md", b, 5*time.Hour)
		put("picks.md", "PICKS-COMPLETE\n", 4*time.Hour)
		put("go.md", "GO\n", 3*time.Hour)
		if w.name == "cos-widget" {
			put("report.md", "done\nREPORT-COMPLETE\n", 2*time.Hour)
		}
	}
	return e
}

func sweepCfg(e sweepEnv) config.Config {
	return config.Config{Sweep: config.SweepConfig{
		Owner: "acme", Since: "2026-10-14", Briefs: e.briefs, Reviewers: []string{"pr-reviewer"},
	}}
}

// sweepRunner answers gh and herdr offline. agents are (pane, status,
// worktree) rows.
func sweepRunner(t *testing.T, agents ...[3]string) *exec.FakeRunner {
	t.Helper()
	var rows []map[string]any
	for _, a := range agents {
		rows = append(rows, map[string]any{
			"pane_id": a[0], "agent": "claude", "agent_status": a[1], "cwd": "/x/" + a[2],
			"tokens":        map[string]any{"cwd": "/x/" + a[2], "worktree": a[2]},
			"agent_session": map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude", "value": "sess-" + a[0][len(a[0])-1:]},
		})
	}
	herdrBody, _ := json.Marshal(map[string]any{"id": "x", "result": map[string]any{"agents": rows}})
	node := func(n int, review string) map[string]any {
		return map[string]any{
			"number": n, "title": fmt.Sprintf("title %d", n), "isDraft": false, "headRefOid": sweepHead, "mergeable": "MERGEABLE",
			"createdAt":  "2026-10-16T09:00:00Z",
			"repository": map[string]any{"nameWithOwner": "acme/widget"},
			"commits":    map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": "SUCCESS"}}}}},
			"reviews":    map[string]any{"nodes": []any{map[string]any{"body": review, "author": map[string]any{"login": "acme-owner"}}}},
		}
	}
	clean := "<!-- cadence-review: pr-reviewer head=" + sweepHead + " crit=0 imp=0 -->\n\nfine"
	gql, _ := json.Marshal(map[string]any{"data": map[string]any{"search": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{node(1, clean), node(2, "no marker here")},
	}}})
	merged := `[{"repository":{"name":"widget","nameWithOwner":"acme/widget"},"number":9,"title":"m","closedAt":"2026-10-16T11:00:00Z","author":{"login":"x","is_bot":false},"url":"u"}]`
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "herdr" && strings.Join(args, " ") == "agent list":
			return string(herdrBody), nil
		case name == "gh" && strings.Contains(strings.Join(args, " "), "viewer { login }"):
			return `{"data":{"viewer":{"login":"acme-owner"}}}`, nil
		case name == "gh" && len(args) > 1 && args[0] == "api" && args[1] == "graphql":
			return string(gql), nil
		case name == "gh" && len(args) > 1 && args[0] == "search" && args[1] == "prs":
			return merged, nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}}
}

func sweepRun(t *testing.T, deps module.Deps, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newSweepCmd(deps)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func sweepLedger(t *testing.T, out string) sweep.Ledger {
	t.Helper()
	var l sweep.Ledger
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("sweep --json: %v\n%s", err, out)
	}
	return l
}

func TestSweepEmitsLedgerAndWritesTheFile(t *testing.T) {
	e := newSweepEnv(t)
	deps := module.Deps{Cfg: sweepCfg(e), Runner: sweepRunner(t, [3]string{"w1:p1", "idle", "cos-widget-4"})}
	out, errOut, err := sweepRun(t, deps, "--json")
	if err != nil {
		t.Fatalf("sweep: %v\n%s", err, errOut)
	}
	l := sweepLedger(t, out)
	if l.SchemaVersion != 2 || l.GeneratedAt != "2026-10-16T12:00:00Z" || len(l.Workers) != 2 || l.OpenPRs != 2 || l.Merged != 1 {
		t.Errorf("ledger = %+v", l)
	}
	states := map[int]string{}
	for _, p := range l.PRs {
		states[p.Number] = p.State
	}
	if states[1] != sweep.StateMarkerClean || states[2] != sweep.StateWaiting {
		t.Errorf("states = %v", states)
	}
	var review *sweep.Opinion
	for i, o := range l.Opinions {
		if o.ID == "worker:cos-widget:report" {
			review = &l.Opinions[i]
		}
	}
	if review == nil || review.Action != sweep.ActionReview || review.Target == nil || review.Target.Number != 2 {
		t.Errorf("report opinion = %+v", review)
	}
	file := filepath.Join(e.state, "cadence", "sweep.json")
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("sweep.json: %v %v", fi, err)
	}
	onDisk := readFileT(t, file)
	if sweepLedger(t, string(onDisk)).GeneratedAt != l.GeneratedAt {
		t.Errorf("the file and stdout disagree")
	}
}

// TestSweepFeedsTheAttentionQueue writes sweep.json with `sweep` and reads it
// back with `attention list`: the writer and T1's reader agree.
func TestSweepFeedsTheAttentionQueue(t *testing.T) {
	e := newSweepEnv(t)
	deps := module.Deps{Cfg: sweepCfg(e), Runner: sweepRunner(t, [3]string{"w1:p1", "blocked", "cos-gadget-2"})}
	if _, errOut, err := sweepRun(t, deps, "--json"); err != nil {
		t.Fatalf("sweep: %v\n%s", err, errOut)
	}
	l := attnList(t, deps)
	if l.Sources.Sweep != sourceOK || l.Skipped != 0 {
		t.Fatalf("sources = %+v skipped = %d", l.Sources, l.Skipped)
	}
	kinds := map[string]attention.Row{}
	for _, r := range l.Rows {
		kinds[r.Kind] = r
	}
	if w := kinds[attention.RowWorker]; w.Worker != "cos-gadget" || w.Tag != attention.BlockedByPrompt || w.Cost != attention.CostBlockedWorker || w.Pane != "w1:p1" {
		t.Errorf("worker row = %+v", w)
	}
	if p := kinds[attention.RowPR]; p.Tag != "acme/widget#1" || p.Cost != attention.CostMarkerClean || p.Worker != "cos-widget" {
		t.Errorf("pr row = %+v", p)
	}
}

func TestSweepExitsTwoNamingTheMissingKey(t *testing.T) {
	e := newSweepEnv(t)
	full := sweepCfg(e).Sweep
	for key, mut := range map[string]func(*config.SweepConfig){
		"sweep.owner":     func(c *config.SweepConfig) { c.Owner = "" },
		"sweep.since":     func(c *config.SweepConfig) { c.Since = "" },
		"sweep.briefs":    func(c *config.SweepConfig) { c.Briefs = "" },
		"sweep.reviewers": func(c *config.SweepConfig) { c.Reviewers = nil },
	} {
		c := full
		mut(&c)
		run := sweepRunner(t)
		deps := module.Deps{Cfg: config.Config{Sweep: c}, Runner: run}
		_, _, err := sweepRun(t, deps, "--json")
		if len(run.Calls) != 0 {
			t.Errorf("%s: sweep ran %d commands before refusing", key, len(run.Calls))
		}
		if _, statErr := os.Stat(filepath.Join(e.state, "cadence", "sweep.json")); statErr == nil {
			t.Errorf("%s: sweep wrote a ledger before refusing", key)
		}
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v", key, err)
			continue
		}
		var ec interface{ ExitCode() int }
		if !errors.As(err, &ec) || ec.ExitCode() != exitUsage {
			t.Errorf("%s: exit code = %v, want %d", key, err, exitUsage)
		}
	}
	// With nothing configured and no flags, the first missing key is named.
	_, _, err := sweepRun(t, module.Deps{Runner: sweepRunner(t)}, "--json")
	if err == nil || !strings.Contains(err.Error(), "sweep.owner") {
		t.Errorf("empty config: %v", err)
	}
}

func TestSweepFlagsOverrideConfig(t *testing.T) {
	e := newSweepEnv(t)
	cfg := sweepCfg(e)
	cfg.Sweep.Owner = "wrong-owner"
	cfg.Sweep.Since = "2020-01-01"
	run := sweepRunner(t)
	deps := module.Deps{Cfg: cfg, Runner: run}
	if _, errOut, err := sweepRun(t, deps, "--json", "--owner", "acme", "--since", "2026-10-14", "--briefs", e.briefs); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	var sawOwner, sawSince bool
	for _, c := range run.Calls {
		j := strings.Join(c.Args, " ")
		sawOwner = sawOwner || strings.Contains(j, "--owner acme") || strings.Contains(j, "user:acme")
		sawSince = sawSince || strings.Contains(j, ">=2026-10-14")
		if strings.Contains(j, "wrong-owner") || strings.Contains(j, "2020-01-01") {
			t.Errorf("config value used despite the flag: %s", j)
		}
	}
	if !sawOwner || !sawSince {
		t.Errorf("flags not used: owner=%v since=%v", sawOwner, sawSince)
	}
}

func TestSweepRejectsBadFlagValues(t *testing.T) {
	e := newSweepEnv(t)
	deps := module.Deps{Cfg: sweepCfg(e), Runner: sweepRunner(t)}
	for _, args := range [][]string{{"--owner", "-x y"}, {"--since", "yesterday"}, {"--briefs", "relative"}} {
		_, _, err := sweepRun(t, deps, append([]string{"--json"}, args...)...)
		var ec interface{ ExitCode() int }
		if err == nil || !errors.As(err, &ec) || ec.ExitCode() != exitUsage {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

// TestSweepLeavesTheDeskUntouched mirrors the attention test: a hand-dropped
// item, a changed item and a queued item all stay byte-identical, and the
// queued item's signal pane blocks its worker.
func TestSweepLeavesTheDeskUntouched(t *testing.T) {
	e := newSweepEnv(t)
	for _, sub := range []string{"pending", "running", "done", "skipped"} {
		if err := os.MkdirAll(filepath.Join(e.desk, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const queued = "# WHAT: fix the widget\n# WHY: test\necho fix\n"
	writeDeskFile(t, filepath.Join(e.desk, "pending", "01-hand.sh"), "# WHAT: by hand\necho hi\n")
	writeDeskFile(t, filepath.Join(e.desk, "pending", "02-changed.sh"), "# WHAT: edited\necho changed\n")
	writeDeskFile(t, filepath.Join(e.desk, "pending", "02-changed.meta.json"),
		`{"added_at":"2026-10-01T00:00:00Z","sha256":"`+sha("the bytes it was queued with")+`","kind":"script"}`)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "03-fix.sh"), queued)
	writeDeskFile(t, filepath.Join(e.desk, "pending", "03-fix.meta.json"),
		`{"added_at":"2026-10-16T11:00:00Z","sha256":"`+sha(queued)+`","kind":"script","signal_pane":"w1:p1"}`)
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, d := range []string{e.desk, filepath.Join(e.desk, "pending")} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	deps := module.Deps{Cfg: sweepCfg(e), Runner: sweepRunner(t, [3]string{"w1:p1", "working", "cos-gadget-2"})}

	before := deskSnapshot(t, e.desk)
	out, errOut, err := sweepRun(t, deps, "--json")
	if err != nil {
		t.Fatalf("sweep: %v\n%s", err, errOut)
	}
	if after := deskSnapshot(t, e.desk); after != before {
		t.Fatalf("sweep changed the desk:\nbefore\n%s\nafter\n%s", before, after)
	}
	l := sweepLedger(t, out)
	for _, w := range l.Workers {
		if w.Name == "cos-gadget" && w.BlockedBy != attention.BlockedByDesk {
			t.Errorf("cos-gadget blockedBy = %q, want desk (its pane queued 03-fix)", w.BlockedBy)
		}
	}
	if l.Sources.Desk != sourceOK {
		t.Errorf("desk source = %q", l.Sources.Desk)
	}
}

func TestSweepKeepsTheOldFileWhenGitHubFails(t *testing.T) {
	e := newSweepEnv(t)
	good := module.Deps{Cfg: sweepCfg(e), Runner: sweepRunner(t)}
	if _, _, err := sweepRun(t, good, "--json"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.state, "cadence", "sweep.json")
	before := readFileT(t, file)
	failing := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" {
			return "", errors.New("gh: HTTP 502")
		}
		return "", errors.New("down")
	}}
	_, _, err := sweepRun(t, module.Deps{Cfg: sweepCfg(e), Runner: failing}, "--json")
	if err == nil {
		t.Fatal("a failed GitHub read was reported as success")
	}
	if after := readFileT(t, file); !bytes.Equal(before, after) {
		t.Error("a failed run replaced the last good ledger")
	}
}

func TestSweepSurvivesHerdrBeingDown(t *testing.T) {
	e := newSweepEnv(t)
	run := sweepRunner(t)
	inner := run.RunFunc
	run.RunFunc = func(name string, args []string) (string, error) {
		if name == "herdr" {
			return "", errors.New("herdr: not running")
		}
		return inner(name, args)
	}
	out, errOut, err := sweepRun(t, module.Deps{Cfg: sweepCfg(e), Runner: run}, "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	l := sweepLedger(t, out)
	if l.Sources.Herdr != "unavailable" {
		t.Errorf("herdr source = %q", l.Sources.Herdr)
	}
	for _, w := range l.Workers {
		if w.Pane != "" || w.BlockedBy != "" {
			t.Errorf("worker %+v has herdr data with herdr down", w)
		}
	}
}

func TestSweepTextSummary(t *testing.T) {
	e := newSweepEnv(t)
	deps := module.Deps{Cfg: sweepCfg(e), Runner: sweepRunner(t)}
	out, _, err := sweepRun(t, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 workers") || !strings.Contains(out, "1 marker-clean") || strings.Contains(out, "{") {
		t.Errorf("summary = %q", out)
	}
	if _, err := os.Stat(filepath.Join(e.state, "cadence", "sweep.json")); err != nil {
		t.Errorf("the text form must still write the ledger: %v", err)
	}
}

func readFileT(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p) //nolint:gosec // G304: a test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return data
}
