package review

// Test plan for releases_releasable.go
//
// CommitFact.Releasable / NewCommitFact (Classification: pure parser)
//   [x] feat, fix, perf and any `!` marker are releasable; chore, ci, docs,
//       test, refactor, style, build and non-conventional subjects are not
//   [x] a BREAKING CHANGE footer makes any type releasable
//
// noReleasePRStall (Classification: stall rule)
//   [x] 23h no stall, exactly 24h no stall, 25h stall
//   [x] chore/ci-only sets never stall; feat! and a footer do
//   [x] an open release PR suppresses it; age is the oldest RELEASABLE commit
//
// releaseWorkflowStall (Classification: stall rule)
//   [x] 59m no stall, exactly 1h no stall, 61m stall, for waiting/queued/pending
//   [x] in_progress and completed runs never stall
//
// Derive wiring, registry field, collection, and the recorded 2026-10-04 state.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func commit(subject string, h float64) CommitFact {
	return CommitFact{At: hoursAgo(h), Subject: subject}
}

func TestCommitFactReleasable(t *testing.T) {
	cases := []struct {
		subject string
		want    bool
	}{
		{"feat: add menu", true},
		{"feat(menu,hub): add forgectl menu (#1016)", true},
		{"fix(redact): withhold credentials", true},
		{"perf: faster scan", true},
		{"FIX: shouty type", true},
		{"feat!: drop the old flag", true},
		{"refactor(api)!: rename Run", true},
		{"chore!: bump the floor", true},
		{"chore: update versions.json", false},
		{"ci(release): pin the action", false},
		{"docs: readme", false},
		{"test: add a case", false},
		{"refactor: tidy", false},
		{"style: gofmt", false},
		{"build: bump go", false},
		{"chore(main): release 0.25.0", false},
		{"Merge pull request #5 from x/y", false},
		{"feature: not a type", false},
		{"feat add menu", false},
		{"", false},
	}
	for _, c := range cases {
		if got := (CommitFact{Subject: c.subject}).Releasable(); got != c.want {
			t.Errorf("Releasable(%q) = %v, want %v", c.subject, got, c.want)
		}
	}
}

func TestNewCommitFact_BreakingFooter(t *testing.T) {
	at := hoursAgo(1)
	cases := []struct {
		name, msg  string
		footer     bool
		releasable bool
	}{
		{"plain chore", "chore: x\n\nbody", false, false},
		{"footer on chore", "chore: x\n\nbody\n\nBREAKING CHANGE: drops flag", true, true},
		{"hyphen footer", "docs: x\n\nBREAKING-CHANGE: y", true, true},
		{"footer text in the subject is not a footer", "chore: BREAKING CHANGE: x", false, false},
		{"lowercase is not a footer", "chore: x\n\nbreaking change: y", false, false},
		{"mid-line is not a footer", "chore: x\n\nthis is a BREAKING CHANGE: y", false, false},
	}
	for _, c := range cases {
		f := NewCommitFact(c.msg, at)
		if f.BreakingFooter != c.footer || f.Releasable() != c.releasable {
			t.Errorf("%s: footer=%v releasable=%v, want %v/%v", c.name, f.BreakingFooter, f.Releasable(), c.footer, c.releasable)
		}
	}
	long := NewCommitFact("feat: "+strings.Repeat("x", 500), at)
	if len(long.Subject) != 200 {
		t.Errorf("subject length = %d, want it capped at 200", len(long.Subject))
	}
}

func TestNoReleasePRStall(t *testing.T) {
	cases := []struct {
		name    string
		commits []CommitFact
		prs     []PRFact
		stall   bool
	}{
		{"feat 25h old, no PR", []CommitFact{commit("feat: a", 25)}, nil, true},
		{"feat 23h old, no PR", []CommitFact{commit("feat: a", 23)}, nil, false},
		{"feat exactly 24h old", []CommitFact{commit("feat: a", 24)}, nil, false},
		{"feat 25h old, release PR open", []CommitFact{commit("feat: a", 25)}, []PRFact{{Number: 9, CreatedAt: hoursAgo(2)}}, false},
		{"chore and ci only, 5 days old", []CommitFact{commit("chore: update versions.json", 120), commit("ci(release): x", 100)}, nil, false},
		{"feat! 25h old", []CommitFact{commit("feat!: drop flag", 25)}, nil, true},
		{"scoped bang on a refactor", []CommitFact{commit("refactor(api)!: rename", 30)}, nil, true},
		{"footer on a chore", []CommitFact{{At: hoursAgo(30), Subject: "chore: x", BreakingFooter: true}}, nil, true},
		{"old chore, fresh feat: age is the releasable one", []CommitFact{commit("chore: old", 200), commit("fix: new", 3)}, nil, false},
		{"fresh fix after an old feat: oldest releasable counts", []CommitFact{commit("feat: old", 40), commit("fix: new", 3)}, nil, true},
		{"nothing unreleased", nil, nil, false},
	}
	for _, c := range cases {
		f := RepoFacts{LastRelease: &ReleaseFact{Tag: "v0.24.0"}, UnreleasedCommits: c.commits, ReleasePRs: c.prs}
		got := noReleasePRStall(f, t0)
		if (len(got) == 1) != c.stall || len(got) > 1 {
			t.Errorf("%s: stalls = %q, want stall=%v", c.name, got, c.stall)
		}
		if c.stall && len(got) > 0 && !strings.HasPrefix(got[0], StallNoReleasePR+": ") {
			t.Errorf("%s: stall %q does not lead with %s", c.name, got[0], StallNoReleasePR)
		}
	}
}

func TestReleaseWorkflowStall(t *testing.T) {
	run := func(id int64, status string, age time.Duration) RunFact {
		return RunFact{ID: id, Event: "push", Status: status, CreatedAt: t0.Add(-age)}
	}
	cases := []struct {
		name  string
		runs  []RunFact
		stall bool
	}{
		{"waiting 61m", []RunFact{run(1, "waiting", 61*time.Minute)}, true},
		{"waiting 59m", []RunFact{run(1, "waiting", 59*time.Minute)}, false},
		{"waiting exactly 1h", []RunFact{run(1, "waiting", time.Hour)}, false},
		{"queued 61m", []RunFact{run(1, "queued", 61*time.Minute)}, true},
		{"queued 59m", []RunFact{run(1, "queued", 59*time.Minute)}, false},
		{"pending 61m", []RunFact{run(1, "pending", 61*time.Minute)}, true},
		{"in_progress for 5h", []RunFact{run(1, "in_progress", 5*time.Hour)}, false},
		{"completed 5h ago", []RunFact{run(1, "completed", 5*time.Hour)}, false},
		{"fresh run beside a stuck one", []RunFact{run(2, "queued", 5*time.Minute), run(1, "waiting", 4*24*time.Hour)}, true},
		{"none", nil, false},
	}
	for _, c := range cases {
		got := releaseWorkflowStall(".github/workflows/release-please.yml", c.runs, t0)
		if (len(got) == 1) != c.stall || len(got) > 1 {
			t.Errorf("%s: stalls = %q, want stall=%v", c.name, got, c.stall)
		}
		if c.stall && len(got) > 0 && !strings.HasPrefix(got[0], StallReleaseWorkflowStuck+": release-please.yml run ") {
			t.Errorf("%s: stall %q", c.name, got[0])
		}
	}
	// The oldest stuck run is the one named.
	got := releaseWorkflowStall("x/release-please.yml", []RunFact{run(2, "queued", 3*time.Hour), run(1, "waiting", 4*24*time.Hour)}, t0)
	if len(got) != 1 || !strings.Contains(got[0], "run 1 has been waiting for 4d") {
		t.Errorf("stalls = %q, want run 1 waiting 4d", got)
	}
}

// Both rules reach the row: state stalled, named in Stalls, and the failing
// verdict that --fail-on-stall reads. A read error still wins as unknown.
func TestDerive_ReleaseMachineryRules(t *testing.T) {
	stuck := healthy("on")
	stuck.PendingReleaseRuns = []RunFact{{ID: 5, Event: "push", Status: "waiting", CreatedAt: hoursAgo(2)}}
	row := Derive(releasePR, stuck, canon, t0)
	if row.State != StateStalled || len(row.Stalls) != 1 || !strings.HasPrefix(row.Stalls[0], StallReleaseWorkflowStuck) {
		t.Errorf("stuck workflow: state=%s stalls=%q", row.State, row.Stalls)
	}

	nopr := healthy("on")
	nopr.UnreleasedCommits = []CommitFact{commit("fix: x", 25)}
	row = Derive(releasePR, nopr, canon, t0)
	if row.State != StateStalled || len(row.Stalls) != 1 || !strings.HasPrefix(row.Stalls[0], StallNoReleasePR) {
		t.Errorf("no release PR: state=%s stalls=%q", row.State, row.Stalls)
	}
	rep := BuildReport(Registry{Version: 1, Repos: []RegistryEntry{releasePR}}, map[string]RepoFacts{"forgectl": nopr}, canon, t0)
	if !rep.Failing() {
		t.Error("a no-release-pr stall must fail --fail-on-stall")
	}

	// Rules apply whether or not the nightly toggle is on.
	paused := Derive(releasePR, withToggle(nopr, "off"), canon, t0)
	if paused.State != StateStalled {
		t.Errorf("paused repo with releasable commits and no PR: state=%s, want stalled", paused.State)
	}

	nopr.Errors = []string{"compare: HTTP 500"}
	if row := Derive(releasePR, nopr, canon, t0); row.State != StateUnknown {
		t.Errorf("read error: state=%s, want unknown", row.State)
	}

	// Only release-pr repos are judged.
	tf := RegistryEntry{Repo: "app", Branch: "main", Class: ClassTestflight, Entrypoint: ".github/workflows/testflight.yml", TagPattern: "none"}
	f := RepoFacts{Repo: "app", Toggle: &ToggleFact{Value: "off"}, UnreleasedCommits: []CommitFact{commit("feat: x", 99)}, PendingReleaseRuns: stuck.PendingReleaseRuns}
	if row := Derive(tf, f, canon, t0); len(row.Stalls) != 0 {
		t.Errorf("testflight stalls = %q, want none", row.Stalls)
	}
}

func withToggle(f RepoFacts, v string) RepoFacts {
	tg := *f.Toggle
	tg.Value = v
	f.Toggle = &tg
	return f
}

func TestReleaseWorkflowFile(t *testing.T) {
	cases := []struct {
		entry    RegistryEntry
		want     string
		explicit bool
	}{
		{RegistryEntry{Repo: "forgectl"}, ".github/workflows/release-please.yml", false},
		{RegistryEntry{Repo: "yaae"}, ".github/workflows/release-please.yml", false},
		{RegistryEntry{Repo: "obsidi-claude"}, ".github/workflows/release-please.yml", false},
		{RegistryEntry{Repo: "cadence-hooks"}, ".github/workflows/prepare-release.yml", false},
		{RegistryEntry{Repo: "cadence-hooks", ReleaseWorkflow: ".github/workflows/custom.yml"}, ".github/workflows/custom.yml", true},
	}
	for _, c := range cases {
		if got, explicit := ReleaseWorkflowFile(c.entry); got != c.want || explicit != c.explicit {
			t.Errorf("%s: %q/%v, want %q/%v", c.entry.Repo, got, explicit, c.want, c.explicit)
		}
	}
}

// A commit whose committer date predates the last release (a rebase or sync)
// is due from the release, not from its own date.
func TestNoReleasePRStall_ClampsToLastRelease(t *testing.T) {
	f := RepoFacts{
		LastRelease:       &ReleaseFact{Tag: "v1.0.0", At: hoursAgo(2)},
		UnreleasedCommits: []CommitFact{commit("fix: rebased", 500)},
	}
	if got := noReleasePRStall(f, t0); len(got) != 0 {
		t.Errorf("stalls = %q, want none: the release was 2h ago", got)
	}
	f.LastRelease.At = hoursAgo(30)
	if got := noReleasePRStall(f, t0); len(got) != 1 {
		t.Errorf("stalls = %q, want one: the release was 30h ago", got)
	}
}

func TestParseRegistry_ReleaseWorkflow(t *testing.T) {
	entry := func(extra string) string {
		return "version: 1\nrepos:\n  - repo: x\n    branch: main\n    class: release-pr\n    entrypoint: .github/workflows/ship.yml\n    tag_pattern: v-semver\n" + extra
	}
	reg, err := ParseRegistry([]byte(entry("    release_workflow: .github/workflows/prepare-release.yml\n")))
	if err != nil || reg.Repos[0].ReleaseWorkflow != ".github/workflows/prepare-release.yml" {
		t.Fatalf("valid release_workflow: %+v, %v", reg.Repos, err)
	}
	for name, raw := range map[string]string{
		"traversal":   entry("    release_workflow: .github/workflows/../x.yml\n"),
		"bare name":   entry("    release_workflow: release-please.yml\n"),
		"wrong class": "version: 1\nrepos:\n  - repo: x\n    branch: main\n    class: manual-cut\n    tag_pattern: none\n    release_workflow: .github/workflows/r.yml\n",
	} {
		if _, err := ParseRegistry([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCollect_ReleaseMachinery(t *testing.T) {
	reg := Registry{Version: 1, Repos: []RegistryEntry{releasePR}}
	const wfPath = "repos/cameronsjo/forgectl/actions/workflows/release-please.yml/runs?status=waiting&per_page=100&exclude_pull_requests=true"

	// An unnamed default workflow the repo does not have is not an error.
	api := forgectlAPI()
	api[wfPath] = ErrAPINotFound
	f := Collect(context.Background(), api, reg)["forgectl"]
	if len(f.Errors) != 0 || len(f.PendingReleaseRuns) != 0 {
		t.Errorf("default workflow 404: errors=%q runs=%+v, want neither", f.Errors, f.PendingReleaseRuns)
	}

	// A workflow the registry names must exist.
	named := releasePR
	named.ReleaseWorkflow = ".github/workflows/release-please.yml"
	f = Collect(context.Background(), api, Registry{Version: 1, Repos: []RegistryEntry{named}})["forgectl"]
	if strings.Join(f.Errors, ";") != "release workflow runs: HTTP 404" {
		t.Errorf("named workflow 404: errors = %q", f.Errors)
	}

	// A failed read is categorical and makes the row unknown.
	api[wfPath] = &APIStatusError{Status: 500}
	f = Collect(context.Background(), api, reg)["forgectl"]
	if row := Derive(releasePR, f, SHA256Hex([]byte("#!/usr/bin/env bash\n")), t0); row.State != StateUnknown {
		t.Errorf("500: state=%s, want unknown", row.State)
	}

	// cadence-hooks reads prepare-release.yml.
	hooks := releasePR
	hooks.Repo = "cadence-hooks"
	hapi := fakeAPI{}
	for k, v := range forgectlAPI() {
		hapi[strings.Replace(k, "cameronsjo/forgectl/", "cameronsjo/cadence-hooks/", 1)] = v
	}
	const hooksWf = "repos/cameronsjo/cadence-hooks/actions/workflows/"
	for _, st := range []string{"waiting", "queued", "pending"} {
		delete(hapi, hooksWf+"release-please.yml/runs?status="+st+"&per_page=100&exclude_pull_requests=true")
		hapi[hooksWf+"prepare-release.yml/runs?status="+st+"&per_page=100&exclude_pull_requests=true"] = map[string]any{"workflow_runs": []any{}}
	}
	hapi[hooksWf+"prepare-release.yml/runs?status=queued&per_page=100&exclude_pull_requests=true"] = map[string]any{
		"workflow_runs": []map[string]any{{"id": 7, "event": "push", "status": "queued", "created_at": "2026-09-30T09:00:00Z"}},
	}
	f = Collect(context.Background(), hapi, Registry{Version: 1, Repos: []RegistryEntry{hooks}})["cadence-hooks"]
	if len(f.PendingReleaseRuns) != 1 || f.PendingReleaseRuns[0].ID != 7 {
		t.Errorf("cadence-hooks runs = %+v errors = %q, want prepare-release.yml run 7", f.PendingReleaseRuns, f.Errors)
	}
}

// A page of commits shorter than ahead_by with nothing releasable on it hides
// the newer commits, so the row fails closed; a releasable commit on the page
// settles the question.
func TestCollect_TruncatedCompareFailsClosed(t *testing.T) {
	reg := Registry{Version: 1, Repos: []RegistryEntry{releasePR}}
	api := forgectlAPI()
	const cmpPath = "repos/cameronsjo/forgectl/compare/v0.19.0...main?per_page=100"
	api[cmpPath] = map[string]any{"ahead_by": 150, "commits": []map[string]any{
		{"commit": map[string]any{"message": "chore: a", "committer": map[string]any{"date": "2026-09-30T03:00:00Z"}}},
	}}
	f := Collect(context.Background(), api, reg)["forgectl"]
	if strings.Join(f.Errors, ";") != "compare: read 1 of 150 unreleased commits, none releasable" {
		t.Errorf("errors = %q", f.Errors)
	}
	api[cmpPath] = map[string]any{"ahead_by": 150, "commits": []map[string]any{
		{"commit": map[string]any{"message": "fix: a", "committer": map[string]any{"date": "2026-09-30T03:00:00Z"}}},
	}}
	if f := Collect(context.Background(), api, reg)["forgectl"]; len(f.Errors) != 0 {
		t.Errorf("releasable on a short page: errors = %q, want none", f.Errors)
	}
}

// TestReleaseStuckFixture replays testdata/release_stuck_20261004.json: the
// state forgectl was in on 2026-10-04, rebuilt from live read-only gh api
// calls. feat(menu,hub) and fix(redact) were unreleased since v0.24.0, with
// no release PR, while a Release Please run sat `waiting` since
// 2026-09-30T19:12Z and cancelled every later run. The radar reported ok.
//
// The commit subjects, dates, and last release are the live values. The
// release PR (#1032) and the next push were not yet open at the captured
// instant, so they are absent. The waiting run's ID is a placeholder; its
// start time comes from the incident record. The commits' committer dates
// are 2026-10-03T20:13Z and 20:33Z, so no-release-pr crosses its 24h window at
// 2026-10-04T20:13Z: the capture instant stalls on the stuck run alone, and a
// replay 8h later stalls on both.
func TestReleaseStuckFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/release_stuck_20261004.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx liveFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	entry := RegistryEntry{Repo: "forgectl", Branch: "main", Class: ClassReleasePR, Entrypoint: ".github/workflows/ship.yml", TagPattern: "v-semver"}
	reg := Registry{Version: 1, Repos: []RegistryEntry{entry}}
	f := fx.Facts["forgectl"]

	at := BuildReport(reg, fx.Facts, fx.CanonicalGate, fx.CapturedAt)
	row := at.Rows[0]
	if row.State != StateStalled || !at.Failing() || len(row.Errors) != 0 {
		t.Fatalf("captured instant: state=%s failing=%v errors=%q stalls=%q, want stalled", row.State, at.Failing(), row.Errors, row.Stalls)
	}
	if len(row.Stalls) != 1 || !strings.HasPrefix(row.Stalls[0], "release-workflow-stuck: release-please.yml run 3 has been waiting for 3d") {
		t.Errorf("captured instant stalls = %q", row.Stalls)
	}

	later := BuildReport(reg, fx.Facts, fx.CanonicalGate, fx.CapturedAt.Add(8*time.Hour)).Rows[0]
	if len(later.Stalls) != 2 || !strings.HasPrefix(later.Stalls[0], "no-release-pr: 2 releasable commit(s) since v0.24.0, oldest 25h ago") || !strings.HasPrefix(later.Stalls[1], "release-workflow-stuck") {
		t.Errorf("+8h stalls = %q, want no-release-pr and release-workflow-stuck", later.Stalls)
	}

	// Negative controls on the same facts: with the stuck run gone the
	// captured instant is ok, and a release PR silences no-release-pr.
	f.PendingReleaseRuns = nil
	fixed := map[string]RepoFacts{"forgectl": f}
	if r := BuildReport(reg, fixed, fx.CanonicalGate, fx.CapturedAt).Rows[0]; r.State != StateOK {
		t.Errorf("no stuck run at capture: state=%s stalls=%q, want ok", r.State, r.Stalls)
	}
	f.ReleasePRs = []PRFact{{Number: 1032, CreatedAt: fx.CapturedAt}}
	fixed["forgectl"] = f
	if r := BuildReport(reg, fixed, fx.CanonicalGate, fx.CapturedAt.Add(8*time.Hour)).Rows[0]; r.State != StateOK {
		t.Errorf("release PR open at +8h: state=%s stalls=%q, want ok", r.State, r.Stalls)
	}
}

// scrubCommit keeps exactly what the releasable rule reads.
func TestScrubCommit_KeepsTheVerdict(t *testing.T) {
	for _, subject := range []string{"feat(secret-project): x", "fix!: y", "chore(private): z", "docs: w", "not conventional"} {
		in := CommitFact{At: t0, Subject: subject}
		out := scrubCommit(in)
		if out.Releasable() != in.Releasable() || strings.Contains(out.Subject, "secret") || strings.Contains(out.Subject, "private") {
			t.Errorf("scrubCommit(%q) = %q (releasable %v, was %v)", subject, out.Subject, out.Releasable(), in.Releasable())
		}
	}
}
