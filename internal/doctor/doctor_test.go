package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/resume"
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

	// A fresh redirected config dir has no forgectl subdirectory yet — the
	// auto log path's parent doesn't exist. checkLogPath must never create
	// it (a health check has no side effect of its own; config.OpenAppendFile
	// creates it on demand at the first real log write) — StateWarn, not
	// StateFail, since it's created automatically the next time forgectl logs.
	check = checkLogPath(Deps{Cfg: config.Config{}})
	if check.State != StateWarn {
		t.Errorf("auto log path, dir absent: state = %q, detail = %q; want warn", check.State, check.Detail)
	}

	// Once the directory exists (forgectl has logged before, or the operator
	// created it), the same auto path resolves to StateOK.
	autoPath := config.ResolvedLogPath("")
	if err := os.MkdirAll(filepath.Dir(autoPath), 0o700); err != nil {
		t.Fatal(err)
	}
	check = checkLogPath(Deps{Cfg: config.Config{}})
	if check.State != StateOK {
		t.Errorf("auto log path, dir present: state = %q, detail = %q; want ok", check.State, check.Detail)
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

// TestCheckTrustStore_InvalidOrTamperedStoreFails pins the security-critical
// distinction ErrTrustStoreMissing's own doc comment calls out: "I never
// enrolled a key" (StateSkip — a valid, unconfigured install) must never
// read the same as "my trust store failed to verify" or "my anchor is
// missing/not root-owned" (StateFail — the exact conditions this check
// exists to catch). Before this test existed, checkTrustStore collapsed
// every TrustedStore error into StateSkip, so a tampered store or a
// world-writable anchor silently reported "-" and left Report.Healthy()
// true — the check answered the question wrongly rather than not answering
// it. ErrTrustStoreMissing wraps ErrTrustStoreInvalid (bless/verify.go), so
// the ordering in checkTrustStore matters: missing must be tested first, or
// errors.Is(err, ErrTrustStoreInvalid) would match the missing case too and
// silently restore the collapsed behavior while looking fixed.
func TestCheckTrustStore_InvalidOrTamperedStoreFails(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"invalid/tampered store", bless.ErrTrustStoreInvalid},
		{"wrapped invalid store", fmt.Errorf("trust store signed by X, not the anchor: %w", bless.ErrTrustStoreInvalid)},
		{"missing or unowned anchor", bless.ErrNoAnchor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Deps{TrustedStore: func() (bless.Store, error) { return bless.Store{}, c.err }}
			check := checkTrustStore(d)
			if check.State != StateFail {
				t.Errorf("state = %q, want fail — a tampered/invalid trust store must never read as skip", check.State)
			}
			if check.Hint == "" {
				t.Error("Fail check has no remediation hint")
			}
		})
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
	// FORGECTL_CLAUDE_BIN outranks [launch.defaults].binary_path (see
	// launch.ClaudePath's precedence order) — clear it, same as
	// TestCheckClaude, so an operator's ambient override can't change
	// whether the claude check passes here. The test only asserts on gh's
	// own state, but leaving claude's outcome to whatever happens to be on
	// PATH is exactly the class of defect this comment now guards against
	// (a test that would read as broken on a machine without claude
	// installed, purely by coincidence rather than by construction).
	t.Setenv("FORGECTL_CLAUDE_BIN", "")
	meta.Version = "dev" // skip the brew/version check — not under test here

	fakeClaude := t.TempDir() + "/claude"
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	d := Deps{
		Cfg:      config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{BinaryPath: fakeClaude}}},
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

// resumeFixture builds a throwaway ~/.claude plus a forgectl store and returns
// Deps wired to it, so the resume check never reads the developer's real tree.
func resumeFixture(t *testing.T) (resume.Paths, Deps) {
	t.Helper()
	root := t.TempDir()
	p := resume.Paths{
		ClaudeHome: filepath.Join(root, ".claude"),
		StoreDir:   filepath.Join(root, "store"),
	}
	for _, d := range []string{
		filepath.Join(p.ClaudeHome, "tasks"),
		filepath.Join(p.ClaudeHome, "sessions"),
		p.StoreDir,
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return p, Deps{ResumePaths: func() (resume.Paths, error) { return p, nil }}
}

// mkTaskDir creates one ~/.claude/tasks/<name>/ holding a single task body.
func mkTaskDir(t *testing.T, p resume.Paths, name string) {
	t.Helper()
	dir := filepath.Join(p.ClaudeHome, "tasks", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(`{"id":"1","subject":"x"}`), 0o600); err != nil {
		t.Fatalf("write task: %v", err)
	}
}

// TestCheckResumeTasks covers the check `forgectl resume`'s own plan calls the
// tripwire for its one version coupling with Claude Code. DriftCheck is tested
// in isolation over in internal/resume; this pins the doctor WIRING — that each
// condition maps to the state an operator acts on.
func TestCheckResumeTasks(t *testing.T) {
	t.Run("no paths configured skips", func(t *testing.T) {
		if got := checkResumeTasks(Deps{}); got.State != StateSkip {
			t.Errorf("state = %q, want skip when ResumePaths is nil", got.State)
		}
	})

	t.Run("a paths error skips rather than failing", func(t *testing.T) {
		d := Deps{ResumePaths: func() (resume.Paths, error) {
			return resume.Paths{}, errors.New("no home directory")
		}}
		if got := checkResumeTasks(d); got.State != StateSkip {
			t.Errorf("state = %q, want skip — an unresolvable home is not a broken install", got.State)
		}
	})

	t.Run("empty tree skips", func(t *testing.T) {
		_, d := resumeFixture(t)
		got := checkResumeTasks(d)
		if got.State != StateSkip {
			t.Errorf("state = %q (%s), want skip on a tree with no task dirs", got.State, got.Detail)
		}
	})

	t.Run("per-session dialect present is ok", func(t *testing.T) {
		p, d := resumeFixture(t)
		mkTaskDir(t, p, "11111111-2222-3333-4444-555555555555")
		got := checkResumeTasks(d)
		if got.State != StateOK {
			t.Errorf("state = %q (%s), want ok", got.State, got.Detail)
		}
	})

	t.Run("only team dialect warns", func(t *testing.T) {
		p, d := resumeFixture(t)
		mkTaskDir(t, p, "session-abcd1234")
		got := checkResumeTasks(d)
		if got.State != StateWarn {
			t.Fatalf("state = %q (%s), want warn — restore would write where nothing reads", got.State, got.Detail)
		}
		if got.Hint == "" {
			t.Error("drift warning carries no hint; the operator needs the next action")
		}
	})

	// The capture-wiring branch is the one that actually costs data: an
	// unwired Stop hook is otherwise undetectable, because snapshot always
	// exits 0 and a missing capture looks exactly like a successful one until
	// a session exits and its tasks are gone.
	t.Run("live sessions with an empty store warns about capture", func(t *testing.T) {
		p, d := resumeFixture(t)
		mkTaskDir(t, p, "11111111-2222-3333-4444-555555555555")
		writeLiveRegistry(t, p, os.Getpid())

		got := checkResumeTasks(d)
		if got.State != StateWarn {
			t.Fatalf("state = %q (%s), want warn — a live session with no snapshot means capture is not wired", got.State, got.Detail)
		}
		if !strings.Contains(got.Detail, "no snapshots stored") {
			t.Errorf("detail = %q, want it to name the empty store", got.Detail)
		}
	})

	t.Run("live sessions with a populated store is ok", func(t *testing.T) {
		p, d := resumeFixture(t)
		mkTaskDir(t, p, "11111111-2222-3333-4444-555555555555")
		writeLiveRegistry(t, p, os.Getpid())
		if err := resume.Save(p.StoreDir, &resume.Record{ID: "11111111-2222-3333-4444-555555555555"}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		if got := checkResumeTasks(d); got.State != StateOK {
			t.Errorf("state = %q (%s), want ok once capture has produced a record", got.State, got.Detail)
		}
	})
}

// writeLiveRegistry plants a registry file for a pid that is genuinely running
// (the test process itself), so the liveness probe reports it live without any
// stubbing.
func writeLiveRegistry(t *testing.T, p resume.Paths, pid int) {
	t.Helper()
	body := fmt.Sprintf(`{"pid":%d,"sessionId":"11111111-2222-3333-4444-555555555555","cwd":"/tmp","version":"2.1.220"}`, pid)
	path := filepath.Join(p.ClaudeHome, "sessions", fmt.Sprintf("%d.json", pid))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
}
