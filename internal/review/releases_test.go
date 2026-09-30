package review

// Test plan for releases.go
//
// ParseRegistry (Classification: hostile-input parser)
//   [x] Happy: the real registry shape parses; continuous/dormant are untracked
//   [x] Unhappy: bad version, empty list, out-of-charset repo, unknown class,
//       release-pr with no entrypoint, manual-cut with one, traversal in an
//       endpoint path, duplicate repo
//
// TagMatches / EndpointVersion / GateReason / Age (Classification: pure parsers)
//   [x] table-driven happy and unhappy rows each
//
// Derive (Classification: stall rules — the decision core)
//   [x] Negative control: toggle on + last run 27h old stalls; the same facts
//       with the toggle paused do not
//   [x] Enabled with a fresh run is ok; enabled with no runs stalls
//   [x] Same non-quiet reason on the last 2 scheduled runs stalls; no-pr twice,
//       two different reasons, or a dispatch run between them do not
//   [x] half-shipped stalls even while paused
//   [x] Endpoint behind > 24h stalls; behind < 24h is shown, not stalled
//   [x] Gate copy drift and missing copy stall; no canonical hash is unknown
//   [x] Any read error makes the row unknown
//   [x] Manual-cut: state manual, gate due clamps to the last cut
//
// BuildReport / Report.Failing
//   [x] Footer holds continuous/dormant; a tracked repo with no facts is unknown

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func hoursAgo(h float64) time.Time { return t0.Add(-time.Duration(h * float64(time.Hour))) }

func intp(n int) *int { return &n }

const registryYAML = `
version: 1
repos:
  - repo: forgectl
    branch: main
    class: release-pr
    entrypoint: .github/workflows/ship.yml
    tag_pattern: v-semver
    human_gates: []
    endpoints:
      - kind: homebrew-cask
        repo: homebrew-tap
        path: Casks/forgectl.rb
  - repo: gatehouse
    branch: main
    class: testflight
    entrypoint: .github/workflows/testflight.yml
    tag_pattern: none
    human_gates: [App Store release]
    endpoints:
      - kind: testflight
  - repo: herdr
    branch: master
    class: manual-cut
    entrypoint: null
    tag_pattern: fork-suffix
    human_gates: [upstream sync, fork cut]
    upstream: herdrdev/herdr
  - repo: cadence
    branch: main
    class: continuous
    tag_pattern: v-semver
    human_gates: [milestone tag]
`

func TestParseRegistry_Happy(t *testing.T) {
	reg, err := ParseRegistry([]byte(registryYAML))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if len(reg.Repos) != 4 {
		t.Fatalf("repos = %d, want 4", len(reg.Repos))
	}
	tracked := map[string]bool{}
	for _, e := range reg.Repos {
		tracked[e.Repo] = e.Tracked()
	}
	if !tracked["forgectl"] || !tracked["gatehouse"] || !tracked["herdr"] || tracked["cadence"] {
		t.Errorf("tracked = %v", tracked)
	}
	if got := reg.Repos[2].UpstreamSlug(); got != "herdrdev/herdr" {
		t.Errorf("UpstreamSlug = %q", got)
	}
}

func TestParseRegistry_Unhappy(t *testing.T) {
	entry := func(body string) string {
		return "version: 1\nrepos:\n  - " + strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n    ") + "\n"
	}
	good := "repo: x\nbranch: main\nclass: release-pr\nentrypoint: .github/workflows/ship.yml\ntag_pattern: v-semver"
	cases := map[string]string{
		"bad version":         "version: 2\nrepos:\n  - repo: x\n",
		"no repos":            "version: 1\nrepos: []\n",
		"repo charset":        entry(strings.Replace(good, "repo: x", "repo: 'x;rm'", 1)),
		"unknown class":       entry(strings.Replace(good, "release-pr", "nightly", 1)),
		"no entrypoint":       entry(strings.Replace(good, "entrypoint: .github/workflows/ship.yml", "entrypoint: null", 1)),
		"entrypoint escape":   entry(strings.Replace(good, ".github/workflows/ship.yml", ".github/workflows/../x.yml", 1)),
		"manual with entry":   entry(strings.Replace(good, "release-pr", "manual-cut", 1)),
		"unknown tag pattern": entry(strings.Replace(good, "v-semver", "calver", 1)),
		"endpoint traversal":  entry(good + "\nendpoints:\n  - kind: scoop\n    repo: scoop-bucket\n    path: ../secrets"),
		"endpoint kind":       entry(good + "\nendpoints:\n  - kind: npm\n    repo: x\n    path: y"),
		"upstream charset":    entry(good + "\nupstream: 'a/b/c'"),
		"duplicate repo":      entry(good) + "  - " + strings.ReplaceAll(good, "\n", "\n    ") + "\n",
		"branch traversal":    entry(strings.Replace(good, "branch: main", "branch: a/../b", 1)),
		"yaml is not a map":   "- just\n- a list\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRegistry([]byte(raw)); err == nil {
				t.Errorf("ParseRegistry accepted %q", raw)
			}
		})
	}
}

func TestTagMatches(t *testing.T) {
	cases := []struct {
		pattern, tag string
		want         bool
	}{
		{"v-semver", "v0.19.0", true},
		{"v-semver", "0.19.0", false},
		{"v-semver", "v0.19.0-rc.1", false},
		{"bare-semver", "1.2.0", true},
		{"bare-semver", "beta", false},
		{"fork-suffix", "v0.9.1-palette.1", true},
		{"fork-suffix", "v0.9.1", false},
		{"component-semver", "envctl-v0.1.1", true},
		{"tf-date", "tf-20260928-1445", true},
		{"tf-date", "tf-2026-09-28", false},
		{"none", "v1.0.0", false},
		{"unknown", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := TagMatches(c.pattern, c.tag); got != c.want {
			t.Errorf("TagMatches(%q, %q) = %v, want %v", c.pattern, c.tag, got, c.want)
		}
	}
}

func TestEndpointVersion(t *testing.T) {
	cases := []struct {
		name, kind, content, want string
	}{
		{"cask", "homebrew-cask", "cask \"forgectl\" do\n  version \"0.19.0\"\n  sha256 \"x\"\n", "0.19.0"},
		{"formula fork suffix", "homebrew-formula", "class Herdr < Formula\n  version \"0.9.1-palette.1\"\n", "0.9.1-palette.1"},
		{"formula url only", "homebrew-formula", "  url \"https://x/v1.0.0.tar.gz\"\n", ""},
		{"scoop", "scoop", `{"version": "0.117.0", "url": "x"}`, "0.117.0"},
		{"scoop not json", "scoop", "not json", ""},
		{"testflight has none", "testflight", "version \"1\"", ""},
	}
	for _, c := range cases {
		if got := EndpointVersion(c.kind, []byte(c.content)); got != c.want {
			t.Errorf("%s: EndpointVersion = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGateReason(t *testing.T) {
	cases := []struct {
		name string
		msgs []string
		want string
	}{
		{"notice", []string{"ship-gate ci-red: PR #752: check build-test is in_progress/null"}, "ci-red"},
		{"error after runner noise", []string{"Process completed with exit code 1.", "ship-gate half-shipped: v1.2.3 has no release"}, "half-shipped"},
		{"paused", []string{"ship-gate paused: SHIP_NIGHTLY is not on"}, "paused"},
		{"unknown reason word", []string{"ship-gate shipped: x"}, ""},
		{"not anchored", []string{"note: ship-gate go: x"}, ""},
		{"none", nil, ""},
	}
	for _, c := range cases {
		if got := GateReason(c.msgs); got != c.want {
			t.Errorf("%s: GateReason = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReleaseVersion(t *testing.T) {
	for tag, want := range map[string]string{
		"v0.19.0": "0.19.0", "1.2.0": "1.2.0", "v0.9.1-palette.1": "0.9.1-palette.1", "envctl-v0.1.1": "0.1.1",
	} {
		if got := releaseVersion(tag); got != want {
			t.Errorf("releaseVersion(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestAge(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Hour:       "0m",
		45 * time.Minute: "45m",
		5 * time.Hour:    "5h",
		47 * time.Hour:   "47h",
		72 * time.Hour:   "3d",
	}
	for d, want := range cases {
		if got := Age(t0, t0.Add(-d)); got != want {
			t.Errorf("Age(%v) = %q, want %q", d, got, want)
		}
	}
}

// releasePR is a registry entry with one cask endpoint.
var releasePR = RegistryEntry{
	Repo: "forgectl", Branch: "main", Class: ClassReleasePR,
	Entrypoint: ".github/workflows/ship.yml", TagPattern: "v-semver",
	Endpoints: []Endpoint{{Kind: "homebrew-cask", Repo: "homebrew-tap", Path: "Casks/forgectl.rb"}},
}

const canon = "d0c096465e86259f8b429668f90cdf3bc3c71a49a02c286ceea49341697266e0"

// healthy returns facts under which releasePR is ok; each case bends one.
func healthy(toggle string) RepoFacts {
	return RepoFacts{
		Repo:        "forgectl",
		LastRelease: &ReleaseFact{Tag: "v0.19.0", At: hoursAgo(30)},
		Unreleased:  intp(3),
		Toggle:      &ToggleFact{Name: "SHIP_NIGHTLY", Value: toggle, Set: toggle != ""},
		Runs: []RunFact{
			{ID: 3, Event: "schedule", Status: "completed", Conclusion: "success", CreatedAt: hoursAgo(1), Reason: "no-pr"},
			{ID: 2, Event: "schedule", Status: "completed", Conclusion: "success", CreatedAt: hoursAgo(25), Reason: "go"},
		},
		Endpoints:      []EndpointFact{{Kind: "homebrew-cask", Repo: "homebrew-tap", Path: "Casks/forgectl.rb", Version: "0.19.0"}},
		GateCopySHA256: canon,
	}
}

func TestDerive_NegativeControl_NoRunIn26h(t *testing.T) {
	stale := func(toggle string) RepoFacts {
		f := healthy(toggle)
		f.Runs = []RunFact{{ID: 9, Event: "schedule", Status: "completed", Conclusion: "success", CreatedAt: hoursAgo(27), Reason: "no-pr"}}
		return f
	}
	on := Derive(releasePR, stale("on"), canon, t0)
	if on.State != StateStalled || len(on.Stalls) != 1 || !strings.Contains(on.Stalls[0], "no ship run in 26h") {
		t.Errorf("toggle on, last run 27h: state=%s stalls=%q, want one no-run stall", on.State, on.Stalls)
	}
	for _, toggle := range []string{"off", ""} {
		paused := Derive(releasePR, stale(toggle), canon, t0)
		if paused.State != StatePaused || len(paused.Stalls) != 0 {
			t.Errorf("toggle %q, last run 27h: state=%s stalls=%q, want paused and no stalls", toggle, paused.State, paused.Stalls)
		}
	}
	// Boundary: 25h is inside the window.
	f := stale("on")
	f.Runs[0].CreatedAt = hoursAgo(25)
	if row := Derive(releasePR, f, canon, t0); row.State != StateOK {
		t.Errorf("last run 25h: state=%s stalls=%q, want ok", row.State, row.Stalls)
	}
}

func TestDerive_Rules(t *testing.T) {
	sched := func(id int64, h float64, reason string) RunFact {
		return RunFact{ID: id, Event: "schedule", Status: "completed", Conclusion: "success", CreatedAt: hoursAgo(h), Reason: reason}
	}
	cases := []struct {
		name      string
		bend      func(*RepoFacts)
		canonical string
		state     string
		stall     string // substring of the one expected stall, "" for none
	}{
		{"healthy", func(*RepoFacts) {}, canon, StateOK, ""},
		{"no runs at all", func(f *RepoFacts) { f.Runs = nil }, canon, StateStalled, "no ship run on record"},
		{"same ci-red twice", func(f *RepoFacts) {
			f.Runs = []RunFact{sched(3, 1, "ci-red"), sched(2, 25, "ci-red")}
		}, canon, StateStalled, "reason ci-red on the last 2 scheduled runs"},
		{"same ci-red twice with a dispatch between", func(f *RepoFacts) {
			f.Runs = []RunFact{sched(4, 1, "ci-red"),
				{ID: 3, Event: "workflow_dispatch", Status: "completed", Conclusion: "success", CreatedAt: hoursAgo(5), Reason: "go"},
				sched(2, 25, "ci-red")}
		}, canon, StateStalled, "reason ci-red"},
		{"no-pr twice is quiet", func(f *RepoFacts) {
			f.Runs = []RunFact{sched(3, 1, "no-pr"), sched(2, 25, "no-pr")}
		}, canon, StateOK, ""},
		{"two different reasons", func(f *RepoFacts) {
			f.Runs = []RunFact{sched(3, 1, "ci-red"), sched(2, 25, "not-verified")}
		}, canon, StateOK, ""},
		{"half-shipped", func(f *RepoFacts) { f.Runs[0].Reason = "half-shipped" }, canon, StateStalled, "half-shipped"},
		{"half-shipped while paused", func(f *RepoFacts) {
			f.Toggle.Value = "off"
			f.Runs[0].Reason = "half-shipped"
		}, canon, StateStalled, "half-shipped"},
		{"endpoint behind 30h", func(f *RepoFacts) { f.Endpoints[0].Version = "0.18.0" }, canon, StateStalled, "homebrew-cask Casks/forgectl.rb is at 0.18.0"},
		{"endpoint missing 30h", func(f *RepoFacts) { f.Endpoints[0].Version = "" }, canon, StateStalled, "is at none"},
		{"endpoint behind 2h", func(f *RepoFacts) {
			f.LastRelease.At = hoursAgo(2)
			f.Endpoints[0].Version = "0.18.0"
		}, canon, StateOK, ""},
		{"gate drift", func(f *RepoFacts) { f.GateCopySHA256 = strings.Repeat("0", 64) }, canon, StateStalled, "gate copy drifted"},
		{"gate missing", func(f *RepoFacts) { f.GateCopySHA256, f.GateCopyMissing = "", true }, canon, StateStalled, "missing"},
		{"no canonical hash", func(*RepoFacts) {}, "", StateUnknown, ""},
		{"read error", func(f *RepoFacts) { f.Errors = []string{"runs: HTTP 403"} }, canon, StateUnknown, ""},
		{"read error beats a stall", func(f *RepoFacts) {
			f.Errors = []string{"variables: HTTP 403"}
			f.Runs = nil
		}, canon, StateUnknown, "no ship run on record"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := healthy("on")
			c.bend(&f)
			row := Derive(releasePR, f, c.canonical, t0)
			if row.State != c.state {
				t.Errorf("state = %s, want %s (stalls %q, errors %q)", row.State, c.state, row.Stalls, row.Errors)
			}
			switch {
			case c.stall == "" && len(row.Stalls) != 0:
				t.Errorf("stalls = %q, want none", row.Stalls)
			case c.stall != "" && (len(row.Stalls) != 1 || !strings.Contains(row.Stalls[0], c.stall)):
				t.Errorf("stalls = %q, want one containing %q", row.Stalls, c.stall)
			}
		})
	}
}

// Testflight reasons: uploaded and no-change are quiet, failed twice stalls.
func TestDerive_TestflightRepeat(t *testing.T) {
	e := RegistryEntry{Repo: "app", Branch: "main", Class: ClassTestflight, Entrypoint: ".github/workflows/testflight.yml", TagPattern: "none"}
	run := func(id int64, h float64, reason string) RunFact {
		return RunFact{ID: id, Event: "schedule", Status: "completed", Conclusion: "success", CreatedAt: hoursAgo(h), Reason: reason}
	}
	for _, c := range []struct {
		a, b  string
		state string
	}{
		{"uploaded", "uploaded", StateOK},
		{"no-change", "no-change", StateOK},
		{"failed", "failed", StateStalled},
		{"failed", "uploaded", StateOK},
	} {
		f := RepoFacts{Repo: "app", LastRelease: &ReleaseFact{Tag: "upload@abc", At: hoursAgo(2)},
			Toggle: &ToggleFact{Name: "TESTFLIGHT_NIGHTLY", Value: "on", Set: true},
			Runs:   []RunFact{run(2, 1, c.a), run(1, 25, c.b)}}
		if row := Derive(e, f, "", t0); row.State != c.state {
			t.Errorf("%s,%s: state = %s (stalls %q errors %q), want %s", c.a, c.b, row.State, row.Stalls, row.Errors, c.state)
		}
	}
}

func TestDerive_EndpointBehindFlag(t *testing.T) {
	f := healthy("on")
	f.LastRelease.At = hoursAgo(2)
	f.Endpoints[0].Version = "0.18.0"
	row := Derive(releasePR, f, canon, t0)
	if len(row.Endpoints) != 1 || !row.Endpoints[0].Behind {
		t.Errorf("endpoints = %+v, want one marked behind", row.Endpoints)
	}
	f.Endpoints[0].Version = "0.19.0"
	if row := Derive(releasePR, f, canon, t0); row.Endpoints[0].Behind {
		t.Errorf("v0.19.0 vs 0.19.0 marked behind")
	}
}

func TestDerive_ManualCut(t *testing.T) {
	e := RegistryEntry{Repo: "herdr", Branch: "master", Class: ClassManualCut, TagPattern: "fork-suffix",
		HumanGates: []string{"upstream sync", "fork cut"}, Upstream: "herdrdev/herdr"}
	cut := hoursAgo(24 * 11)
	older := hoursAgo(24 * 20) // merged after the cut, dated before it
	f := RepoFacts{Repo: "herdr", LastRelease: &ReleaseFact{Tag: "v0.9.1-palette.1", At: cut},
		Unreleased: intp(138), OldestUnreleasedAt: &older}
	row := Derive(e, f, "", t0)
	if row.State != StateManual || len(row.Stalls) != 0 {
		t.Fatalf("state=%s stalls=%q, want manual with no stalls", row.State, row.Stalls)
	}
	if row.HumanGateDueSince == nil || !row.HumanGateDueSince.Equal(cut) {
		t.Errorf("due since = %v, want clamped to the cut %v", row.HumanGateDueSince, cut)
	}
	up := hoursAgo(24 * 2)
	f = RepoFacts{Repo: "herdr", LastRelease: &ReleaseFact{Tag: "v0.9.1-palette.1", At: cut}, Unreleased: intp(0), UpstreamDueSince: &up}
	if row := Derive(e, f, "", t0); row.HumanGateDueSince == nil || !row.HumanGateDueSince.Equal(up) {
		t.Errorf("due since = %v, want upstream %v", row.HumanGateDueSince, up)
	}
	f = RepoFacts{Repo: "herdr", LastRelease: &ReleaseFact{Tag: "v0.9.1-palette.1", At: cut}, Unreleased: intp(0)}
	if row := Derive(e, f, "", t0); row.HumanGateDueSince != nil {
		t.Errorf("due since = %v, want nil when nothing waits", row.HumanGateDueSince)
	}
}

func TestBuildReport(t *testing.T) {
	reg, err := ParseRegistry([]byte(registryYAML))
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]RepoFacts{"forgectl": healthy("on")}
	rep := BuildReport(reg, facts, canon, t0)
	if len(rep.Rows) != 3 || len(rep.Footer) != 1 || rep.Footer[0].Repo != "cadence" {
		t.Fatalf("rows=%d footer=%+v", len(rep.Rows), rep.Footer)
	}
	if rep.Rows[0].State != StateOK {
		t.Errorf("forgectl state = %s", rep.Rows[0].State)
	}
	if rep.Rows[1].State != StateUnknown || rep.Rows[1].Errors[0] != "no facts collected" {
		t.Errorf("gatehouse with no facts = %+v, want unknown", rep.Rows[1])
	}
	if !rep.FailingRows || !rep.Failing() {
		t.Errorf("a report with an unknown row must fail")
	}
	rep = BuildReport(reg, map[string]RepoFacts{
		"forgectl":  healthy("on"),
		"gatehouse": {Repo: "gatehouse", Toggle: &ToggleFact{Name: "TESTFLIGHT_NIGHTLY"}},
		"herdr":     {Repo: "herdr"},
	}, canon, t0)
	if rep.Failing() {
		t.Errorf("healthy/paused/manual report fails: %+v", rep.Rows)
	}
}
