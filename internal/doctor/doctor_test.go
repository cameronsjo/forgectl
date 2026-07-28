package doctor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
)

// redirectConfigDir points os.UserConfigDir() at a fresh temp dir for the
// test (mirrors internal/config's own test helper of the same name) — every
// check that resolves a config-derived path needs this so tests never touch
// the real machine's forgectl config dir.
func redirectConfigDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
}

// fakeLookPath returns a LookPath stub that resolves only the names in
// found, erroring for everything else — the seam every PATH-presence check
// (checkBinary, checkGh, checkForgectlVersion's brew probe) goes through.
func fakeLookPath(found ...string) func(string) (string, error) {
	set := make(map[string]bool, len(found))
	for _, n := range found {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

func fakeProber(code int) bench.Prober {
	return &stubProber{code: code}
}

type stubProber struct{ code int }

func (s *stubProber) Probe(_ context.Context, _ string) (int, error) { return s.code, nil }

func TestCheckClaude(t *testing.T) {
	redirectConfigDir(t)
	// FORGECTL_CLAUDE_BIN outranks [launch.defaults].binary_path (see
	// launch.ClaudePath's precedence order) — clear it so an operator's
	// ambient override never leaks into this test's expectations.
	t.Setenv("FORGECTL_CLAUDE_BIN", "")
	dir := t.TempDir()
	claudePath := dir + "/claude"
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	d := Deps{Cfg: config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{BinaryPath: claudePath}}}}
	check := checkClaude(d)
	if check.State != StateOK {
		t.Errorf("state = %q, detail = %q; want ok", check.State, check.Detail)
	}

	d.Cfg.Launch.Defaults.BinaryPath = dir + "/does-not-exist"
	check = checkClaude(d)
	if check.State != StateFail || check.Hint == "" {
		t.Errorf("missing claude: state = %q, hint = %q; want fail with a hint", check.State, check.Hint)
	}
}

func TestCheckConfig(t *testing.T) {
	redirectConfigDir(t)
	dir, err := config.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir[:len(dir)-len("/config.toml")], 0o755); err != nil {
		t.Fatal(err)
	}

	// No file at all: valid (built-in defaults).
	check := checkConfig(Deps{})
	if check.State != StateOK {
		t.Errorf("no config file: state = %q, want ok", check.State)
	}

	// Malformed TOML: fail, with a hint.
	if err := os.WriteFile(dir, []byte("not [ valid toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	check = checkConfig(Deps{})
	if check.State != StateFail || check.Hint == "" {
		t.Errorf("malformed config: state = %q, hint = %q; want fail with a hint", check.State, check.Hint)
	}
}

func TestCheckLogPath(t *testing.T) {
	redirectConfigDir(t)

	check := checkLogPath(Deps{Cfg: config.Config{LogFile: "-"}})
	if check.State != StateOK || check.Detail == "" {
		t.Errorf("stderr log: state = %q, detail = %q; want ok with detail", check.State, check.Detail)
	}

	check = checkLogPath(Deps{Cfg: config.Config{}})
	if check.State != StateOK {
		t.Errorf("auto log path: state = %q, detail = %q; want ok", check.State, check.Detail)
	}
}

func TestCheckBinary(t *testing.T) {
	d := Deps{LookPath: fakeLookPath("tmux")}

	if check := checkBinary(d, "tmux", "install tmux"); check.State != StateOK {
		t.Errorf("tmux present: state = %q, want ok", check.State)
	}
	if check := checkBinary(d, "ghostty", "install ghostty"); check.State != StateWarn || check.Hint != "install ghostty" {
		t.Errorf("ghostty absent: state = %q, hint = %q; want warn with the given hint", check.State, check.Hint)
	}
}

func TestCheckGh(t *testing.T) {
	// gh missing entirely.
	d := Deps{LookPath: fakeLookPath()}
	if check := checkGh(context.Background(), d); check.State != StateFail {
		t.Errorf("gh absent: state = %q, want fail", check.State)
	}

	// gh present but not authenticated.
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: "gh", Stderr: "not logged in", Err: errors.New("exit status 1")}
	}}
	d = Deps{LookPath: fakeLookPath("gh"), Runner: fr}
	if check := checkGh(context.Background(), d); check.State != StateFail || check.Hint == "" {
		t.Errorf("gh unauthenticated: state = %q, hint = %q; want fail with a hint", check.State, check.Hint)
	}

	// gh present and authenticated.
	fr = &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "Logged in", nil }}
	d = Deps{LookPath: fakeLookPath("gh"), Runner: fr}
	if check := checkGh(context.Background(), d); check.State != StateOK {
		t.Errorf("gh authenticated: state = %q, want ok", check.State)
	}
}

func TestFromBenchComponent(t *testing.T) {
	cases := []struct {
		in   bench.State
		want State
	}{
		{bench.StateOK, StateOK},
		{bench.StateNotConfigured, StateSkip},
		{bench.StateDegraded, StateWarn},
		{bench.StateUnavailable, StateFail},
	}
	for _, c := range cases {
		got := fromBenchComponent(bench.Component{Name: "x", State: c.in})
		if got.State != c.want {
			t.Errorf("fromBenchComponent(%s) state = %q, want %q", c.in, got.State, c.want)
		}
	}
}

func TestCheckTrustStore(t *testing.T) {
	d := Deps{TrustedStore: func() (bless.Store, error) {
		return bless.Store{}, bless.ErrTrustStoreMissing
	}}
	if check := checkTrustStore(d); check.State != StateSkip {
		t.Errorf("missing trust store: state = %q, want skip (opt-in infra, not a failure)", check.State)
	}

	d = Deps{TrustedStore: func() (bless.Store, error) {
		return bless.Store{Keys: []bless.TrustedKey{{KeyID: "abc"}}}, nil
	}}
	if check := checkTrustStore(d); check.State != StateOK {
		t.Errorf("present trust store: state = %q, want ok", check.State)
	}
}

func TestCheckForgectlVersion(t *testing.T) {
	orig := meta.Version
	t.Cleanup(func() { meta.Version = orig })

	meta.Version = "dev"
	if check := checkForgectlVersion(context.Background(), Deps{}); check.State != StateSkip {
		t.Errorf("source build: state = %q, want skip", check.State)
	}

	meta.Version = "1.0.0"
	d := Deps{LookPath: fakeLookPath()}
	if check := checkForgectlVersion(context.Background(), d); check.State != StateWarn {
		t.Errorf("brew absent: state = %q, want warn", check.State)
	}

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	d = Deps{LookPath: fakeLookPath("brew"), Runner: fr}
	if check := checkForgectlVersion(context.Background(), d); check.State != StateOK {
		t.Errorf("up to date: state = %q, want ok", check.State)
	}

	fr = &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "cameronsjo/tap/forgectl (0.9.0) < 0.10.0", nil
	}}
	d = Deps{LookPath: fakeLookPath("brew"), Runner: fr}
	if check := checkForgectlVersion(context.Background(), d); check.State != StateWarn || check.Hint == "" {
		t.Errorf("outdated: state = %q, hint = %q; want warn with a hint", check.State, check.Hint)
	}
}

// TestRun_AggregatesAllChecksAndFailsOnAnyFailure exercises Run end to end
// with every seam faked, pinning that a single failing check (gh, here)
// makes Report.Healthy() false even though every other check passes.
func TestRun_AggregatesAllChecksAndFailsOnAnyFailure(t *testing.T) {
	redirectConfigDir(t)
	meta.Version = "dev" // skip the brew/version check — not under test here

	d := Deps{
		Cfg:      config.Config{},
		Runner:   &exec.FakeRunner{},
		LookPath: fakeLookPath("tmux", "ghostty", "cmux"), // gh deliberately absent
		TrustedStore: func() (bless.Store, error) {
			return bless.Store{}, bless.ErrTrustStoreMissing
		},
		Prober: fakeProber(200),
	}

	report := Run(context.Background(), d)
	if len(report.Checks) == 0 {
		t.Fatal("Run() returned no checks")
	}
	if report.Healthy() {
		t.Error("Healthy() = true with gh missing, want false")
	}

	var sawGh bool
	for _, c := range report.Checks {
		if c.Name == "gh" {
			sawGh = true
			if c.State != StateFail {
				t.Errorf("gh check state = %q, want fail", c.State)
			}
		}
	}
	if !sawGh {
		t.Error("Run() report has no gh check")
	}
}
