package cli

// Test plan for review_releases.go
//
// newReviewReleasesCmd (Classification: command wiring)
//   [x] --json emits the report; --fail-on-stall exits with errReleasesStalled
//       when a read fails (fail closed) and nil when every row is healthy
//   [x] the table names the failed read on an `unknown` line
//   [x] the registry path comes from --registry, else $FORGECTL_RELEASE_REGISTRY
//   [x] the report carries both canonical hashes: the scripts beside the
//       registry, or --gate-sha256 / --lint-sha256
//
// resolveCanonicalHash (Classification: config resolution)
//   [x] for the gate and the lint: flag beats env beats the file beside the
//       registry; bad hex is refused; no source at all yields "" (rows then
//       read unknown)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/review"
)

type mapAPI map[string]string

func (m mapAPI) Get(_ context.Context, p string) ([]byte, error) {
	if body, ok := m[p]; ok {
		return []byte(body), nil
	}
	return nil, &review.APIStatusError{Status: 500}
}

const radarRegistry = `version: 1
repos:
  - repo: artificer
    branch: main
    class: manual-cut
    entrypoint: null
    tag_pattern: v-semver
    human_gates: [hand tag]
  - repo: cadence
    branch: main
    class: continuous
    tag_pattern: v-semver
    human_gates: [milestone tag]
`

var radarNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func healthyRadarAPI() mapAPI {
	return mapAPI{
		"repos/cameronsjo/artificer/releases?per_page=100":               `[{"tag_name":"v0.27.0","published_at":"2026-09-27T16:09:57Z"}]`,
		"repos/cameronsjo/artificer/compare/v0.27.0...main?per_page=100": `{"ahead_by":0,"commits":[]}`,
	}
}

func writeRegistry(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "release-rhythm.yaml")
	if err := os.WriteFile(p, []byte(radarRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runRadar(t *testing.T, api review.APIGetter, args ...string) (string, error) {
	t.Helper()
	cmd := newReviewReleasesCmd(api, func() time.Time { return radarNow })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestReviewReleases_JSONHealthy(t *testing.T) {
	reg := writeRegistry(t)
	out, err := runRadar(t, healthyRadarAPI(), "--registry", reg, "--json", "--fail-on-stall", "--gate-sha256", strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("healthy radar with --fail-on-stall: %v\n%s", err, out)
	}
	var rep review.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output is not a report: %v\n%s", err, out)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].State != review.StateManual || rep.FailingRows {
		t.Errorf("rows = %+v stalled = %v", rep.Rows, rep.FailingRows)
	}
	if len(rep.Footer) != 1 || rep.Footer[0].Repo != "cadence" {
		t.Errorf("footer = %+v", rep.Footer)
	}
}

func TestReviewReleases_FailClosedOnReadError(t *testing.T) {
	reg := writeRegistry(t)
	out, err := runRadar(t, mapAPI{}, "--registry", reg, "--fail-on-stall")
	if !errors.Is(err, errReleasesStalled) {
		t.Fatalf("err = %v, want errReleasesStalled", err)
	}
	if !strings.Contains(out, "unknown artificer: releases: HTTP 500") {
		t.Errorf("table does not name the failed read:\n%s", out)
	}
	// Without --fail-on-stall the same report exits 0.
	if _, err := runRadar(t, mapAPI{}, "--registry", reg); err != nil {
		t.Errorf("without --fail-on-stall: %v", err)
	}
}

func TestReviewReleases_RegistryFromEnv(t *testing.T) {
	t.Setenv(envReleaseRegistry, writeRegistry(t))
	if _, err := runRadar(t, healthyRadarAPI(), "--json"); err != nil {
		t.Errorf("registry from env: %v", err)
	}
	t.Setenv(envReleaseRegistry, filepath.Join(t.TempDir(), "absent.yaml"))
	if _, err := runRadar(t, healthyRadarAPI()); err == nil {
		t.Errorf("a missing registry must fail")
	}
}

func TestResolveCanonicalHash(t *testing.T) {
	for _, c := range []struct{ name, env, rel, file string }{
		{"gate", envGateSHA256, canonicalGateRel, "ship-gate.sh"},
		{"lint", envLintSHA256, canonicalLintRel, "lint-release-env.sh"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			regPath := filepath.Join(dir, "docs", "release-rhythm.yaml")
			a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
			resolve := func(flag string) (string, error) {
				return resolveCanonicalHash(c.name, flag, c.env, c.rel, regPath)
			}

			t.Setenv(c.env, "")
			if got, err := resolve(""); err != nil || got != "" {
				t.Errorf("no source: %q, %v; want empty", got, err)
			}
			script := filepath.Join(dir, "scripts", "release", c.file)
			if err := os.MkdirAll(filepath.Dir(script), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(script, []byte(c.name+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, _ := resolve(""); got != review.SHA256Hex([]byte(c.name+"\n")) {
				t.Errorf("file beside registry: %q", got)
			}
			t.Setenv(c.env, b)
			if got, _ := resolve(""); got != b {
				t.Errorf("env: %q, want %q", got, b)
			}
			if got, _ := resolve(strings.ToUpper(a)); got != a {
				t.Errorf("flag: %q, want %q", got, a)
			}
			if _, err := resolve("xyz"); err == nil || !strings.Contains(err.Error(), "canonical "+c.name+" sha256") {
				t.Errorf("bad hex: err = %v, want a refusal naming the %s", err, c.name)
			}
		})
	}
}

// The command reads both canonical scripts beside the registry into the
// report, and each flag replaces its file.
func TestReviewReleases_CanonicalHashesInReport(t *testing.T) {
	t.Setenv(envGateSHA256, "")
	t.Setenv(envLintSHA256, "")
	dir := t.TempDir()
	reg := filepath.Join(dir, "docs", "release-rhythm.yaml")
	for p, body := range map[string]string{
		reg: radarRegistry,
		filepath.Join(dir, "scripts", "release", "ship-gate.sh"):        "gate\n",
		filepath.Join(dir, "scripts", "release", "lint-release-env.sh"): "lint\n",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report := func(args ...string) review.Report {
		t.Helper()
		out, err := runRadar(t, healthyRadarAPI(), append([]string{"--registry", reg, "--json"}, args...)...)
		if err != nil {
			t.Fatalf("radar: %v\n%s", err, out)
		}
		var rep review.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("output is not a report: %v\n%s", err, out)
		}
		return rep
	}
	rep := report()
	if rep.CanonicalGate != review.SHA256Hex([]byte("gate\n")) || rep.CanonicalLint != review.SHA256Hex([]byte("lint\n")) {
		t.Errorf("beside the registry: gate=%q lint=%q", rep.CanonicalGate, rep.CanonicalLint)
	}
	c := strings.Repeat("c", 64)
	if rep := report("--lint-sha256", c); rep.CanonicalLint != c || rep.CanonicalGate != review.SHA256Hex([]byte("gate\n")) {
		t.Errorf("--lint-sha256: gate=%q lint=%q, want the flag for the lint only", rep.CanonicalGate, rep.CanonicalLint)
	}
}

// The table shows each column, the stall and unknown notes, and the footer,
// and a hostile tag cannot put an escape sequence on the terminal.
func TestRenderReleasesTable(t *testing.T) {
	at := radarNow.Add(-30 * time.Hour)
	n := 3
	rep := review.Report{
		GeneratedAt: radarNow,
		Rows: []review.Row{{
			Repo: "forgectl", Class: "release-pr", State: review.StateStalled,
			LastRelease: "v0.19.0\x1b[31m", LastReleaseAt: &at, Unreleased: &n,
			ReleasePR: &review.PRFact{Number: 752, CreatedAt: at}, ReleasePRCount: 2,
			LastRun:    &review.RunFact{Status: "completed", Conclusion: "success", CreatedAt: at, Reason: "ci-red"},
			HumanGates: []string{},
			Endpoints:  []review.EndpointStatus{{EndpointFact: review.EndpointFact{Kind: "scoop", Version: "0.18.0"}, Behind: true}},
			GateCopy:   "drift",
			LintCopy:   "missing",
			Stalls:     []string{"gate copy drifted from the canonical ship-gate.sh"},
			Errors:     []string{"runs: HTTP 403"},
		}},
		Footer: []review.FooterEntry{{Repo: "cadence", Class: "continuous", HumanGates: []string{"milestone tag"}}},
	}
	var out bytes.Buffer
	if err := renderReleasesTable(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"#752 (30h) +1", "30h ago success ci-red", "scoop 0.18.0 behind", "GATE COPY  LINT COPY", "missing",
		"stall   forgectl: gate copy drifted", "unknown forgectl: runs: HTTP 403", "not tracked: cadence (continuous; milestone tag)"} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape byte reached the table:\n%q", got)
	}
}
