package bench

import (
	"context"
	"errors"
	osexec "os/exec"
	"runtime"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// fakeProber records every probed target and returns a canned code, failing the
// targets listed in fail.
type fakeProber struct {
	code int
	fail map[string]bool
	seen []string
}

func (f *fakeProber) Probe(_ context.Context, target string) (int, error) {
	f.seen = append(f.seen, target)
	if f.fail[target] {
		return 0, errors.New("connection refused")
	}
	return f.code, nil
}

func (f *fakeProber) probed(target string) bool {
	for _, s := range f.seen {
		if s == target {
			return true
		}
	}
	return false
}

const twoRunning = `{"Service":"caddy","State":"running","Health":"healthy"}
{"Service":"loki","State":"running"}`

func TestCheckHearth_OK(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return twoRunning, nil
	}}
	probe := &fakeProber{code: 200}

	c := checkHearth(context.Background(), cfg, runner, probe)

	if c.State != StateOK {
		t.Fatalf("state = %q, reason = %q; want ok", c.State, c.Reason)
	}
	if len(runner.Calls) != 1 || runner.Calls[0].Name != "docker" {
		t.Fatalf("first call = %+v; want a docker call", runner.Calls)
	}
	wantArgs := []string{"compose", "-p", "hearth", "ps", "--format", "json"}
	if !equalStr(runner.Calls[0].Args, wantArgs) {
		t.Errorf("docker args = %v, want %v", runner.Calls[0].Args, wantArgs)
	}
	for _, target := range []string{"http://hearth.localhost", "http://grafana.localhost", "tcp://localhost:16317"} {
		if !probe.probed(target) {
			t.Errorf("expected a probe of %q; probed %v", target, probe.seen)
		}
	}
}

func TestCheckHearth_DegradedOnProbeFailure(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return twoRunning, nil
	}}
	probe := &fakeProber{code: 200, fail: map[string]bool{"tcp://localhost:16317": true}}

	c := checkHearth(context.Background(), cfg, runner, probe)

	if c.State != StateDegraded {
		t.Fatalf("state = %q, reason = %q; want degraded", c.State, c.Reason)
	}
}

func TestCheckHearth_EndpointOverrideChangesProbe(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth", OTLPEndpoint: "http://collector:9999"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return twoRunning, nil
	}}
	probe := &fakeProber{code: 200}

	checkHearth(context.Background(), cfg, runner, probe)

	if !probe.probed("tcp://collector:9999") {
		t.Errorf("expected a probe of the overridden endpoint; probed %v", probe.seen)
	}
}

func TestCheckHearth_UnavailableWhenDockerFails(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", errors.New("docker: command not found")
	}}

	c := checkHearth(context.Background(), cfg, runner, &fakeProber{code: 200})

	if c.State != StateUnavailable {
		t.Fatalf("state = %q; want unavailable", c.State)
	}
}

func TestCheckHearth_UnavailableWhenNoContainers(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{HearthDir: "/x/hearth"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", nil
	}}

	c := checkHearth(context.Background(), cfg, runner, &fakeProber{code: 200})

	if c.State != StateUnavailable {
		t.Fatalf("state = %q; want unavailable", c.State)
	}
}

func TestCheckHearth_NotConfigured(t *testing.T) {
	t.Setenv("HEARTH_DIR", "")
	c := checkHearth(context.Background(), config.Config{}, &exec.FakeRunner{}, &fakeProber{code: 200})

	if c.State != StateNotConfigured {
		t.Fatalf("state = %q; want not-configured", c.State)
	}
}

const chronicleJSON = `{"events":5,"files":2,"sessions":3,"metrics_events":1,` +
	`"sources":[{"source":"active","files":2,"events":5}],` +
	`"last_sync":"2026-07-08T10:00:00Z","generated_at":"2026-07-08T10:05:00Z"}`

func TestCheckChronicle_OK(t *testing.T) {
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{ChronicleDir: "/x/chronicle"}}
	runner := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "uv" {
			return chronicleJSON, nil
		}
		return "", nil // launchctl: loaded
	}}

	c := checkChronicle(context.Background(), cfg, runner)

	if c.State != StateOK {
		t.Fatalf("state = %q, reason = %q; want ok", c.State, c.Reason)
	}
	wantArgs := []string{"--directory", "/x/chronicle", "run", "chronicle", "status", "--json"}
	if runner.Calls[0].Name != "uv" || !equalStr(runner.Calls[0].Args, wantArgs) {
		t.Errorf("chronicle call = %s %v, want uv %v", runner.Calls[0].Name, runner.Calls[0].Args, wantArgs)
	}
}

func TestCheckChronicle_DegradedWhenDaemonUnloaded(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent check is darwin-only")
	}
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{ChronicleDir: "/x/chronicle"}}
	runner := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "uv" {
			return chronicleJSON, nil
		}
		return "", errors.New("could not find service") // launchctl: not loaded
	}}

	c := checkChronicle(context.Background(), cfg, runner)

	if c.State != StateDegraded {
		t.Fatalf("state = %q; want degraded", c.State)
	}
}

func TestCheckChronicle_UnavailableWhenStatusFails(t *testing.T) {
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{ChronicleDir: "/x/chronicle"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", errors.New("uv: no such directory")
	}}

	c := checkChronicle(context.Background(), cfg, runner)

	if c.State != StateUnavailable {
		t.Fatalf("state = %q; want unavailable", c.State)
	}
}

func TestCheckChronicle_DegradedOnBadJSON(t *testing.T) {
	t.Setenv("CHRONICLE_DIR", "")
	cfg := config.Config{Bench: config.BenchConfig{ChronicleDir: "/x/chronicle"}}
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "not json at all", nil
	}}

	c := checkChronicle(context.Background(), cfg, runner)

	if c.State != StateDegraded {
		t.Fatalf("state = %q; want degraded", c.State)
	}
}

func TestCheckChronicle_NotConfigured(t *testing.T) {
	t.Setenv("CHRONICLE_DIR", "")
	if _, err := osexec.LookPath("chronicle"); err == nil {
		t.Skip("chronicle is on PATH here; the not-configured branch is unreachable")
	}
	c := checkChronicle(context.Background(), config.Config{}, &exec.FakeRunner{})

	if c.State != StateNotConfigured {
		t.Fatalf("state = %q; want not-configured", c.State)
	}
}

func TestCheckFlux_NotConfiguredWhenBoardEnvUnset(t *testing.T) {
	t.Setenv("FLUX_DIR", "")
	t.Setenv("CADENCE_KANBAN", "")
	c := checkFlux(context.Background(), &exec.FakeRunner{})

	if c.State != StateNotConfigured {
		t.Fatalf("state = %q; want not-configured", c.State)
	}
}

func TestCheckFlux_OK(t *testing.T) {
	if _, err := osexec.LookPath("flux"); err != nil {
		t.Skip("flux not on PATH")
	}
	t.Setenv("FLUX_DIR", "/board")
	runner := &exec.FakeRunner{}

	c := checkFlux(context.Background(), runner)

	if c.State != StateOK {
		t.Fatalf("state = %q, reason = %q; want ok", c.State, c.Reason)
	}
	if runner.Calls[0].Name != "flux" || !equalStr(runner.Calls[0].Args, []string{"ready"}) {
		t.Errorf("flux call = %s %v, want flux [ready]", runner.Calls[0].Name, runner.Calls[0].Args)
	}
}

func TestCheckFlux_DegradedWhenBoardUnreachable(t *testing.T) {
	if _, err := osexec.LookPath("flux"); err != nil {
		t.Skip("flux not on PATH")
	}
	t.Setenv("FLUX_DIR", "/board")
	runner := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", errors.New("no board at /board")
	}}

	c := checkFlux(context.Background(), runner)

	if c.State != StateDegraded {
		t.Fatalf("state = %q; want degraded", c.State)
	}
}

func TestParseComposePS(t *testing.T) {
	cases := []struct {
		name              string
		in                string
		wantTotal, wantUp int
	}{
		{"ndjson all running", twoRunning, 2, 2},
		{"ndjson mixed", `{"Service":"a","State":"running"}` + "\n" + `{"Service":"b","State":"exited"}`, 2, 1},
		{"json array", `[{"Service":"a","State":"running"},{"Service":"b","State":"running"}]`, 2, 2},
		{"empty", "", 0, 0},
		{"whitespace", "  \n  ", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, up, err := parseComposePS(tc.in)
			if err != nil {
				t.Fatalf("parseComposePS: %v", err)
			}
			if total != tc.wantTotal || up != tc.wantUp {
				t.Errorf("got total=%d up=%d, want total=%d up=%d", total, up, tc.wantTotal, tc.wantUp)
			}
		})
	}
}

func TestOTLPTarget(t *testing.T) {
	cases := map[string]string{
		"http://localhost:16317":   "tcp://localhost:16317",
		"localhost:16317":          "tcp://localhost:16317",
		"https://collector:4317":   "tcp://collector:4317",
		"http://127.0.0.1:16317/v1": "tcp://127.0.0.1:16317",
	}
	for in, want := range cases {
		if got := otlpTarget(in); got != want {
			t.Errorf("otlpTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
