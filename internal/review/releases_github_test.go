package review

// Test plan for releases_github.go
//
// GhAPI.Get (Classification: subprocess boundary)
//   [x] argv is `gh api --method GET <path>`
//   [x] 404 → ErrAPINotFound; other HTTP status → APIStatusError carrying only
//       the code; stderr text (which can hold a token) never reaches the error
//
// Collect (Classification: I/O wrapper over a fake APIGetter)
//   [x] release-pr: picks the newest shaped release (skips beta and drafts),
//       counts unreleased, keeps same-repo release PRs, reads the toggle,
//       reads the gate reason from the Gate job's annotations, hashes the
//       base64 gate copy, reads the endpoint version
//   [x] a missing gate copy is GateCopyMissing, not an error
//   [x] a failed read is a categorical error on that repo only
//
// testflightReason (Classification: pure)
//   [x] paused / failed / uploaded / no-change
//
// Live fixture (Classification: recorded read-only run)
//   [x] testdata/releases_live.json replays through BuildReport
//   [ ] recording: FORGECTL_RECORD_RELEASES_FIXTURE=1 go test -run TestRecordReleasesFixture

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
)

func TestGhAPIGet(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[len(args)-1] {
		case "ok":
			return `{"a":1}`, nil
		case "missing":
			return "", &exec.CommandError{Name: "gh", Stderr: "gh: Not Found (HTTP 404)"}
		case "forbidden":
			return "", &exec.CommandError{Name: "gh", Stderr: "token ghs_SECRET rejected (HTTP 403)"}
		}
		return "", &exec.CommandError{Name: "gh", Stderr: "ghs_SECRET network down"}
	}}
	api := GhAPI{Run: fr}
	body, err := api.Get(context.Background(), "ok")
	if err != nil || string(body) != `{"a":1}` {
		t.Fatalf("Get ok = %q, %v", body, err)
	}
	if got := strings.Join(fr.Calls[0].Args, " "); got != "api --method GET ok" {
		t.Errorf("argv = %q", got)
	}
	if _, err := api.Get(context.Background(), "missing"); !errors.Is(err, ErrAPINotFound) {
		t.Errorf("404: err = %v, want ErrAPINotFound", err)
	}
	_, err = api.Get(context.Background(), "forbidden")
	var se *APIStatusError
	if !errors.As(err, &se) || se.Status != 403 {
		t.Errorf("403: err = %v, want APIStatusError 403", err)
	}
	_, other := api.Get(context.Background(), "down")
	for _, e := range []error{err, other} {
		if e == nil || strings.Contains(e.Error(), "SECRET") {
			t.Errorf("error %v leaks stderr", e)
		}
	}
}

// fakeAPI serves canned bodies by exact path; any other path is an error so
// a wrong URL shows up as a failed test, not a silent empty answer.
type fakeAPI map[string]any

func (f fakeAPI) Get(_ context.Context, p string) ([]byte, error) {
	v, ok := f[p]
	if !ok {
		return nil, fmt.Errorf("unexpected path %s", p)
	}
	if err, ok := v.(error); ok {
		return nil, err
	}
	if s, ok := v.(string); ok {
		return []byte(s), nil
	}
	return json.Marshal(v)
}

func contents(s string) map[string]string {
	return map[string]string{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(s))}
}

func forgectlAPI() fakeAPI {
	return fakeAPI{
		"repos/cameronsjo/forgectl/releases?per_page=100": []map[string]any{
			{"tag_name": "beta", "published_at": "2026-09-30T10:00:00Z", "prerelease": true},
			{"tag_name": "v0.20.0", "published_at": "2026-09-30T11:00:00Z", "draft": true},
			{"tag_name": "v0.19.0", "published_at": "2026-09-30T02:48:28Z"},
			{"tag_name": "v0.18.0", "published_at": "2026-09-08T22:43:26Z"},
		},
		"repos/cameronsjo/forgectl/compare/v0.19.0...main?per_page=100": map[string]any{
			"ahead_by": 2,
			"commits": []map[string]any{
				{"commit": map[string]any{"message": "chore: bump deps", "committer": map[string]any{"date": "2026-09-30T03:00:00Z"}}},
				{"commit": map[string]any{"message": "refactor: tidy\n\nBREAKING CHANGE: drops the old flag", "committer": map[string]any{"date": "2026-09-30T04:00:00Z"}}},
			},
		},
		"repos/cameronsjo/forgectl/pulls?state=open&base=main&per_page=100": []map[string]any{
			{"number": 752, "title": "chore(main): release 0.20.0", "created_at": "2026-09-30T03:00:00Z", "head": map[string]any{"repo": map[string]any{"full_name": "cameronsjo/forgectl"}}},
			{"number": 800, "title": "chore(main): release 9.9.9", "created_at": "2026-09-30T03:00:00Z", "head": map[string]any{"repo": map[string]any{"full_name": "stranger/forgectl"}}},
			{"number": 801, "title": "feat: thing", "created_at": "2026-09-30T03:00:00Z", "head": map[string]any{"repo": map[string]any{"full_name": "cameronsjo/forgectl"}}},
		},
		"repos/cameronsjo/forgectl/contents/.github/scripts/ship-gate.sh?ref=main": contents("#!/usr/bin/env bash\n"),
		"repos/cameronsjo/forgectl/actions/variables?per_page=100": map[string]any{
			"variables": []map[string]any{{"name": "RELEASE_APP_ID", "value": "1"}, {"name": "SHIP_NIGHTLY", "value": "on"}},
		},
		"repos/cameronsjo/forgectl/actions/workflows/ship.yml/runs?per_page=50&exclude_pull_requests=true": map[string]any{
			"workflow_runs": []map[string]any{
				{"id": 3, "event": "workflow_dispatch", "status": "completed", "conclusion": "success", "created_at": "2026-09-30T03:56:19Z"},
				{"id": 2, "event": "schedule", "status": "completed", "conclusion": "success", "created_at": "2026-09-29T11:00:00Z"},
				{"id": 1, "event": "schedule", "status": "completed", "conclusion": "failure", "created_at": "2026-09-28T11:00:00Z"},
			},
		},
		"repos/cameronsjo/forgectl/actions/workflows/release-please.yml/runs?status=waiting&per_page=100&exclude_pull_requests=true": map[string]any{
			"workflow_runs": []map[string]any{{"id": 91, "event": "push", "status": "waiting", "created_at": "2026-09-30T11:30:00Z"}},
		},
		"repos/cameronsjo/forgectl/actions/workflows/release-please.yml/runs?status=queued&per_page=100&exclude_pull_requests=true":  map[string]any{"workflow_runs": []any{}},
		"repos/cameronsjo/forgectl/actions/workflows/release-please.yml/runs?status=pending&per_page=100&exclude_pull_requests=true": map[string]any{"workflow_runs": []any{}},
		"repos/cameronsjo/forgectl/actions/runs/3/jobs":                   map[string]any{"jobs": []map[string]any{{"id": 30, "name": "Gate", "conclusion": "success"}, {"id": 31, "name": "Merge", "conclusion": "skipped"}}},
		"repos/cameronsjo/forgectl/actions/runs/2/jobs":                   map[string]any{"jobs": []map[string]any{{"id": 20, "name": "Gate", "conclusion": "success"}}},
		"repos/cameronsjo/forgectl/actions/runs/1/jobs":                   map[string]any{"jobs": []map[string]any{{"id": 10, "name": "Gate", "conclusion": "failure"}}},
		"repos/cameronsjo/forgectl/check-runs/30/annotations?per_page=50": []map[string]any{{"message": "ship-gate ci-red: PR #752: check build-test is in_progress/null"}},
		"repos/cameronsjo/forgectl/check-runs/20/annotations?per_page=50": []map[string]any{{"message": "ship-gate no-pr: no release PR"}},
		"repos/cameronsjo/forgectl/check-runs/10/annotations?per_page=50": []map[string]any{{"message": "Process completed with exit code 1."}, {"message": "ship-gate error: bad commits shape"}},
		"repos/cameronsjo/homebrew-tap/contents/Casks/forgectl.rb":        contents("cask \"forgectl\" do\n  version \"0.19.0\"\nend\n"),
	}
}

func TestCollect_ReleasePR(t *testing.T) {
	reg := Registry{Version: 1, Repos: []RegistryEntry{releasePR}}
	facts := Collect(context.Background(), forgectlAPI(), reg)
	f := facts["forgectl"]
	if len(f.Errors) != 0 {
		t.Fatalf("errors = %q", f.Errors)
	}
	if f.LastRelease == nil || f.LastRelease.Tag != "v0.19.0" {
		t.Errorf("last release = %+v, want v0.19.0", f.LastRelease)
	}
	if f.Unreleased == nil || *f.Unreleased != 2 || f.OldestUnreleasedAt == nil || f.OldestUnreleasedAt.Hour() != 3 {
		t.Errorf("unreleased = %v oldest = %v", f.Unreleased, f.OldestUnreleasedAt)
	}
	if len(f.UnreleasedCommits) != 2 || f.UnreleasedCommits[0].Releasable() || !f.UnreleasedCommits[1].Releasable() || !f.UnreleasedCommits[1].BreakingFooter {
		t.Errorf("unreleased log = %+v, want chore (not releasable) then a refactor with a BREAKING CHANGE footer", f.UnreleasedCommits)
	}
	if len(f.PendingReleaseRuns) != 1 || f.PendingReleaseRuns[0].ID != 91 || f.PendingReleaseRuns[0].Status != "waiting" {
		t.Errorf("pending release runs = %+v, want only the waiting run 91", f.PendingReleaseRuns)
	}
	if len(f.ReleasePRs) != 1 || f.ReleasePRs[0].Number != 752 {
		t.Errorf("release PRs = %+v, want only #752", f.ReleasePRs)
	}
	if f.Toggle == nil || f.Toggle.Value != "on" || !f.Toggle.Set {
		t.Errorf("toggle = %+v", f.Toggle)
	}
	reasons := []string{}
	for _, r := range f.Runs {
		reasons = append(reasons, r.Reason)
	}
	if strings.Join(reasons, ",") != "ci-red,no-pr,error" {
		t.Errorf("reasons = %q, want ci-red,no-pr,error", reasons)
	}
	if f.GateCopySHA256 != SHA256Hex([]byte("#!/usr/bin/env bash\n")) {
		t.Errorf("gate sha = %s (trailing newline must survive)", f.GateCopySHA256)
	}
	if len(f.Endpoints) != 1 || f.Endpoints[0].Version != "0.19.0" {
		t.Errorf("endpoints = %+v", f.Endpoints)
	}
}

func TestCollect_MissingGateAndFailedRead(t *testing.T) {
	api := forgectlAPI()
	api["repos/cameronsjo/forgectl/contents/.github/scripts/ship-gate.sh?ref=main"] = ErrAPINotFound
	api["repos/cameronsjo/forgectl/actions/variables?per_page=100"] = &APIStatusError{Status: 500}
	f := Collect(context.Background(), api, Registry{Version: 1, Repos: []RegistryEntry{releasePR}})["forgectl"]
	if !f.GateCopyMissing || f.GateCopySHA256 != "" {
		t.Errorf("missing gate: missing=%v sha=%q", f.GateCopyMissing, f.GateCopySHA256)
	}
	if strings.Join(f.Errors, ";") != "variables: HTTP 500" || f.Toggle != nil {
		t.Errorf("errors = %q toggle = %+v, want one categorical variables error", f.Errors, f.Toggle)
	}
	if row := Derive(releasePR, f, canon, t0); row.State != StateUnknown {
		t.Errorf("state = %s, want unknown", row.State)
	}
}

// An App token cannot read variables (403): the toggle is inferred from the
// newest scheduled run, and with none to read the row fails closed.
func TestCollect_ToggleFromRuns(t *testing.T) {
	reg := Registry{Version: 1, Repos: []RegistryEntry{releasePR}}
	api := forgectlAPI()
	api["repos/cameronsjo/forgectl/actions/variables?per_page=100"] = &APIStatusError{Status: 403}
	f := Collect(context.Background(), api, reg)["forgectl"]
	if len(f.Errors) != 0 || f.Toggle == nil || f.Toggle.Value != "on" || f.Toggle.Source != "runs" {
		t.Errorf("no-pr scheduled run: toggle = %+v errors = %q, want on from runs", f.Toggle, f.Errors)
	}

	api["repos/cameronsjo/forgectl/check-runs/20/annotations?per_page=50"] = []map[string]any{{"message": "ship-gate paused: SHIP_NIGHTLY is not on"}}
	f = Collect(context.Background(), api, reg)["forgectl"]
	if f.Toggle == nil || f.Toggle.Value != "off" {
		t.Errorf("paused scheduled run: toggle = %+v, want off", f.Toggle)
	}
	if row := Derive(releasePR, f, SHA256Hex([]byte("#!/usr/bin/env bash\n")), t0); row.State != StatePaused {
		t.Errorf("state = %s (stalls %q errors %q), want paused", row.State, row.Stalls, row.Errors)
	}

	api["repos/cameronsjo/forgectl/actions/workflows/ship.yml/runs?per_page=50&exclude_pull_requests=true"] = map[string]any{"workflow_runs": []any{}}
	f = Collect(context.Background(), api, reg)["forgectl"]
	if f.Toggle != nil || len(f.Errors) != 1 || !strings.HasPrefix(f.Errors[0], "toggle:") {
		t.Errorf("no scheduled run: toggle = %+v errors = %q, want a toggle error", f.Toggle, f.Errors)
	}
}

// Missing data is never healthy: no shaped release, and a completed ship
// run whose gate left no reason, are both named errors.
func TestCollect_MissingDataFailsClosed(t *testing.T) {
	reg := Registry{Version: 1, Repos: []RegistryEntry{releasePR}}
	api := forgectlAPI()
	api["repos/cameronsjo/forgectl/releases?per_page=100"] = []map[string]any{{"tag_name": "beta", "published_at": "2026-09-30T10:00:00Z"}}
	api["repos/cameronsjo/forgectl/check-runs/30/annotations?per_page=50"] = []map[string]any{}
	f := Collect(context.Background(), api, reg)["forgectl"]
	want := "releases: none matching v-semver in the newest 1;run 3: gate wrote no reason"
	if strings.Join(f.Errors, ";") != want {
		t.Errorf("errors = %q, want %q", f.Errors, want)
	}
	api["repos/cameronsjo/forgectl/actions/runs/3/jobs"] = map[string]any{"jobs": []map[string]any{{"id": 31, "name": "Merge", "conclusion": "skipped"}}}
	f = Collect(context.Background(), api, reg)["forgectl"]
	if !strings.Contains(strings.Join(f.Errors, ";"), "run 3: no Gate job") {
		t.Errorf("errors = %q, want a missing Gate job error", f.Errors)
	}
}

// A testflight repo: the newest upload record is the release, the compare
// base is its SHA, and run reasons come from jobs and artifacts.
func TestCollect_Testflight(t *testing.T) {
	sha := strings.Repeat("a", 40)
	e := RegistryEntry{Repo: "app", Branch: "main", Class: ClassTestflight, Entrypoint: ".github/workflows/testflight.yml", TagPattern: "none"}
	api := fakeAPI{
		"repos/cameronsjo/app/actions/artifacts?name=testflight-upload&per_page=50": map[string]any{"artifacts": []map[string]any{
			{"created_at": "2026-09-29T11:12:26Z", "workflow_run": map[string]any{"id": 2, "head_sha": sha}},
			{"created_at": "2026-06-01T11:12:26Z", "expired": true, "workflow_run": map[string]any{"id": 1, "head_sha": sha}},
		}},
		"repos/cameronsjo/app/compare/" + sha + "...main?per_page=100": map[string]any{"ahead_by": 0, "commits": []any{}},
		"repos/cameronsjo/app/actions/variables?per_page=100":          map[string]any{"variables": []map[string]any{{"name": "TESTFLIGHT_NIGHTLY", "value": "on"}}},
		"repos/cameronsjo/app/actions/workflows/testflight.yml/runs?per_page=50&exclude_pull_requests=true": map[string]any{"workflow_runs": []map[string]any{
			{"id": 3, "event": "workflow_dispatch", "status": "in_progress", "conclusion": nil, "created_at": "2026-09-30T11:00:00Z"},
			{"id": 2, "event": "schedule", "status": "completed", "conclusion": "success", "created_at": "2026-09-29T11:04:15Z"},
		}},
		"repos/cameronsjo/app/actions/runs/2/jobs": map[string]any{"jobs": []map[string]any{{"id": 20, "name": "Archive, verify and upload", "conclusion": "success"}}},
	}
	f := Collect(context.Background(), api, Registry{Version: 1, Repos: []RegistryEntry{e}})["app"]
	if len(f.Errors) != 0 {
		t.Fatalf("errors = %q", f.Errors)
	}
	if f.LastRelease == nil || f.LastRelease.Tag != "upload@aaaaaaa" || f.LastRelease.SHA != sha {
		t.Errorf("last release = %+v", f.LastRelease)
	}
	if len(f.Runs) != 2 || f.Runs[0].Reason != "" || f.Runs[0].Conclusion != "" || f.Runs[1].Reason != "uploaded" {
		t.Errorf("runs = %+v, want in-progress unread and the scheduled run uploaded", f.Runs)
	}
}

func TestTestflightReason(t *testing.T) {
	s := func(v string) *string { return &v }
	arts := []ghArtifact{{}}
	arts[0].WorkflowRun.ID = 7
	cases := []struct {
		name string
		run  RunFact
		jobs []ghJob
		want string
	}{
		{"all skipped", RunFact{ID: 1, Conclusion: "success"}, []ghJob{{Conclusion: s("skipped")}}, "paused"},
		{"failed", RunFact{ID: 1, Conclusion: "failure"}, []ghJob{{Conclusion: s("failure")}}, "failed"},
		{"uploaded", RunFact{ID: 7, Conclusion: "success"}, []ghJob{{Conclusion: s("success")}}, "uploaded"},
		{"no change", RunFact{ID: 8, Conclusion: "success"}, []ghJob{{Conclusion: s("success")}}, "no-change"},
	}
	for _, c := range cases {
		if got := testflightReason(c.run, c.jobs, arts); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// liveFixture is testdata/releases_live.json: the facts one read-only run
// against the live repos collected, with the clock and canonical hash it
// ran under. Facts hold only the fields the radar uses, which is all that
// is stored for the private repos.
type liveFixture struct {
	CapturedAt    time.Time            `json:"captured_at"`
	CanonicalGate string               `json:"canonical_gate_sha256"`
	Facts         map[string]RepoFacts `json:"facts"`
}

const (
	fixturePath  = "testdata/releases_live.json"
	registryPath = "testdata/release-rhythm.yaml"
)

func TestLiveFixtureReplays(t *testing.T) {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fx liveFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	rep := BuildReport(reg, fx.Facts, fx.CanonicalGate, fx.CapturedAt)
	if len(rep.Rows) != 9 || len(rep.Footer) != 5 {
		t.Fatalf("rows=%d footer=%d, want 9 and 5", len(rep.Rows), len(rep.Footer))
	}
	byRepo := map[string]Row{}
	for _, r := range rep.Rows {
		if len(r.Errors) > 0 {
			t.Errorf("%s: errors in the capture: %q", r.Repo, r.Errors)
		}
		byRepo[r.Repo] = r
	}
	// What the capture showed: every vendored gate matches, forgectl and
	// cadence-hooks are on, obsidi-claude and yaae are paused, herdr is a
	// manual cut, and cadence-hooks' Scoop bucket trails its release.
	for _, repo := range []string{"forgectl", "cadence-hooks", "obsidi-claude", "yaae"} {
		if byRepo[repo].GateCopy != "match" {
			t.Errorf("%s gate copy = %q, want match", repo, byRepo[repo].GateCopy)
		}
	}
	if byRepo["obsidi-claude"].State != StatePaused || byRepo["yaae"].State != StatePaused {
		t.Errorf("obsidi-claude/yaae = %s/%s, want paused", byRepo["obsidi-claude"].State, byRepo["yaae"].State)
	}
	if byRepo["herdr"].State != StateManual {
		t.Errorf("herdr = %s, want manual", byRepo["herdr"].State)
	}
	hooks := byRepo["cadence-hooks"]
	if len(hooks.Endpoints) != 2 || !hooks.Endpoints[1].Behind {
		t.Errorf("cadence-hooks endpoints = %+v, want scoop behind", hooks.Endpoints)
	}
	// Replayed 26h later with nothing new, the enabled repos stall and the
	// paused ones do not: the negative control, on real data.
	later := BuildReport(reg, fx.Facts, fx.CanonicalGate, fx.CapturedAt.Add(27*time.Hour))
	for _, r := range later.Rows {
		wantStall := r.Repo == "forgectl" || r.Repo == "cadence-hooks"
		gotStall := false
		for _, s := range r.Stalls {
			gotStall = gotStall || strings.Contains(s, "no ship run in 26h")
		}
		if (r.Repo == "forgectl" || r.Repo == "cadence-hooks" || r.Repo == "obsidi-claude" || r.Repo == "yaae") && gotStall != wantStall {
			t.Errorf("%s +27h: no-run stall = %v, want %v (stalls %q)", r.Repo, gotStall, wantStall, r.Stalls)
		}
	}
}

// TestRecordReleasesFixture re-records testdata/releases_live.json and
// testdata/release-rhythm.yaml from the live repos with the caller's gh
// auth. Read-only; opt-in:
//
//	FORGECTL_RECORD_RELEASES_FIXTURE=1 \
//	FORGECTL_RECORD_REGISTRY=~/Projects/cadence-ecosystem/docs/release-rhythm.yaml \
//	go test ./internal/review -run TestRecordReleasesFixture
//
// forgectl is public and the registry names private repos, so every private
// repo (and private upstream) is renamed to an alias, its SHAs and run IDs
// are replaced, and the registry copy keeps only the fields the radar reads.
func TestRecordReleasesFixture(t *testing.T) {
	if os.Getenv("FORGECTL_RECORD_RELEASES_FIXTURE") != "1" {
		t.Skip("set FORGECTL_RECORD_RELEASES_FIXTURE=1 to record from the live repos")
	}
	src := os.Getenv("FORGECTL_RECORD_REGISTRY")
	reg, err := LoadRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := os.ReadFile(filepath.Join(filepath.Dir(src), "..", "scripts", "release", "ship-gate.sh")) //nolint:gosec // G304: an opt-in recorder; the developer names the registry
	if err != nil {
		t.Fatalf("canonical gate beside the registry: %v", err)
	}
	ctx := context.Background()
	api := GhAPI{Run: githubauth.Runner(exec.OSRunner{}, "")}
	alias := privateAliases(ctx, t, api, reg)
	facts := Collect(ctx, api, reg)

	fx := liveFixture{
		CapturedAt:    time.Now().UTC().Truncate(time.Second),
		CanonicalGate: SHA256Hex(gate),
		Facts:         map[string]RepoFacts{},
	}
	n := 0
	for real, f := range facts {
		if a, ok := alias[real]; ok {
			n++
			f = scrubFacts(f, a, n)
		}
		fx.Facts[f.Repo] = f
	}
	out, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	renamed := Registry{Version: 1}
	for _, e := range reg.Repos {
		if a, ok := alias[e.Repo]; ok {
			e.Repo = a
		}
		if a, ok := alias[e.Upstream]; ok {
			e.Upstream = a
		}
		gates := make([]string, 0, len(e.HumanGates))
		for _, g := range e.HumanGates {
			for real, a := range alias {
				g = strings.ReplaceAll(g, real, a)
			}
			gates = append(gates, g)
		}
		e.HumanGates = gates
		renamed.Repos = append(renamed.Repos, e)
	}
	y, err := yaml.Marshal(renamed)
	if err != nil {
		t.Fatal(err)
	}
	head := "# Recorded by TestRecordReleasesFixture from the release-rhythm registry.\n" +
		"# Radar fields only; private repos renamed. Do not edit by hand.\n"
	if err := os.WriteFile(registryPath, append([]byte(head), y...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// privateAliases maps each private registry repo and private upstream to
// an alias.
func privateAliases(ctx context.Context, t *testing.T, api APIGetter, reg Registry) map[string]string {
	t.Helper()
	alias := map[string]string{}
	isPrivate := func(slug string) bool {
		body, err := api.Get(ctx, "repos/"+slug)
		if err != nil {
			t.Fatalf("visibility of %s: %v", slug, err)
		}
		var r struct {
			Private *bool `json:"private"`
		}
		if err := json.Unmarshal(body, &r); err != nil || r.Private == nil {
			t.Fatalf("visibility of %s: unexpected shape", slug)
		}
		return *r.Private
	}
	for _, e := range reg.Repos {
		if _, done := alias[e.Repo]; !done && isPrivate(RegistryOwner+"/"+e.Repo) {
			alias[e.Repo] = fmt.Sprintf("private-%s-%d", e.Class, len(alias)+1)
		}
		if e.Upstream != "" && !strings.Contains(e.Upstream, "/") && isPrivate(e.UpstreamSlug()) {
			alias[e.Upstream] = fmt.Sprintf("private-upstream-%d", len(alias)+1)
		}
	}
	return alias
}

// scrubFacts renames a private repo's facts and replaces its identifiers.
func scrubFacts(f RepoFacts, alias string, n int) RepoFacts {
	f.Repo = alias
	if f.LastRelease != nil {
		rel := *f.LastRelease
		if rel.SHA != "" {
			rel.SHA = fmt.Sprintf("%040x", n)
			if strings.HasPrefix(rel.Tag, "upload@") {
				rel.Tag = "upload@" + rel.SHA[:7]
			}
		}
		f.LastRelease = &rel
	}
	runs := make([]RunFact, len(f.Runs))
	for i, r := range f.Runs {
		r.ID = int64(i + 1)
		runs[i] = r
	}
	f.Runs = runs
	// A private repo's commit subjects are private: keep the type and any
	// breaking marker, which are all the releasable rule reads.
	log := make([]CommitFact, len(f.UnreleasedCommits))
	for i, c := range f.UnreleasedCommits {
		log[i] = scrubCommit(c)
	}
	f.UnreleasedCommits = log
	pending := make([]RunFact, len(f.PendingReleaseRuns))
	for i, r := range f.PendingReleaseRuns {
		r.ID = int64(1000 + i)
		pending[i] = r
	}
	f.PendingReleaseRuns = pending
	return f
}

// scrubCommit reduces a subject to its type and bang, which is all the
// releasable rule reads; scope and description may name private work.
func scrubCommit(c CommitFact) CommitFact {
	subject := "redacted"
	if m := reConventional.FindStringSubmatch(c.Subject); m != nil {
		subject = strings.ToLower(m[1]) + m[3] + ": redacted"
	}
	c.Subject = subject
	return c
}
