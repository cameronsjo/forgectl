package cli

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
)

// usageProbe captures what the launch path recorded and whether it execed,
// without any filesystem store. Every field is asserted somewhere: a probe
// that only counted calls could not catch a row carrying an argv fragment.
type usageProbe struct {
	events   []launch.UsageEventV1
	enabled  []bool
	execs    int
	failWith error
}

func installUsageProbe(t *testing.T) *usageProbe {
	t.Helper()
	probe := &usageProbe{}

	prevRecord, prevExec, prevNow := recordLaunchUsage, execHarness, usageNow
	recordLaunchUsage = func(enabled bool, ev launch.UsageEventV1) error {
		probe.enabled = append(probe.enabled, enabled)
		if enabled {
			probe.events = append(probe.events, ev)
		}
		return probe.failWith
	}
	execHarness = func(string, []string, []string) error {
		probe.execs++
		return nil
	}
	usageNow = func() time.Time { return time.Date(2026, 8, 13, 19, 42, 17, 0, time.UTC) }
	t.Cleanup(func() {
		recordLaunchUsage, execHarness, usageNow = prevRecord, prevExec, prevNow
	})
	return probe
}

// stubHarnessBinary writes an executable the profile can resolve, so launchExec
// reaches its exec seam instead of failing on binary resolution.
func stubHarnessBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func usageConfig(t *testing.T, harness string, enabled bool) config.Config {
	t.Helper()
	binary := stubHarnessBinary(t)
	defaults := config.LaunchDefaults{Harness: harness, Model: "opus"}
	switch harness {
	case "codex":
		defaults.Model = ""
		defaults.ApprovalPolicy = "on-request"
		defaults.Sandbox = "read-only"
		defaults.CodexBinaryPath = binary
	case "pi":
		defaults.Model = ""
		defaults.PiBinaryPath = binary
	default:
		defaults.BinaryPath = binary
	}
	return config.Config{Launch: config.LaunchConfig{Defaults: defaults, UsageStats: enabled}}
}

func TestIsOwnLaunchVerb_StatsNeverReachesHarness(t *testing.T) {
	if !isOwnLaunchVerb("stats") {
		t.Fatal("`launch stats` must be intercepted before the harness passthrough")
	}
	probe := installUsageProbe(t)
	deps := module.Deps{Cfg: usageConfig(t, "claude", true)}
	handled, err := runLaunch(deps, []string{"stats", "--json"})
	if err != nil {
		t.Fatalf("runLaunch: %v", err)
	}
	if handled {
		t.Fatal("`launch stats` was handled as a passthrough instead of routed to its subcommand")
	}
	if probe.execs != 0 || len(probe.events) != 0 {
		t.Fatalf("`launch stats` execed %d time(s) and recorded %d event(s), want none", probe.execs, len(probe.events))
	}
}

func TestLaunchExec_ClassifiesEveryBranchWithoutReadingArgv(t *testing.T) {
	// The builder and agents arguments deliberately look like native resume
	// and fork invocations. forgectl did not build them, so it must not read
	// them: the row stays "unknown" and carries no fragment of the argv.
	for _, tc := range []struct {
		name        string
		harness     string
		args        []string
		wantMode    string
		wantPosture string
		wantModel   string
	}{
		{"bare claude", "claude", nil, launch.UsageSessionNew, launch.UsagePostureDefault, "opus"},
		{"bare codex", "codex", nil, launch.UsageSessionNew, launch.UsagePostureDefault, ""},
		{"bare pi", "pi", nil, launch.UsageSessionNew, launch.UsagePostureDefault, ""},
		{"opaque builder", "claude", []string{"--resume", "sessionid", "-p", "secretprompt"}, launch.UsageSessionUnknown, launch.UsagePostureBuilder, "opus"},
		{"native codex resume", "codex", []string{"resume", "--fork", "deadbeef"}, launch.UsageSessionUnknown, launch.UsagePostureBuilder, ""},
		{"native pi resume", "pi", []string{"--resume"}, launch.UsageSessionUnknown, launch.UsagePostureBuilder, ""},
		{"agents passthrough", "claude", []string{"agents", "list", "--json"}, launch.UsageSessionUnknown, launch.UsagePostureAgents, "opus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := installUsageProbe(t)
			if err := launchExec(nil, usageConfig(t, tc.harness, true), tc.args); err != nil {
				t.Fatalf("launchExec: %v", err)
			}
			if len(probe.events) != 1 || probe.execs != 1 {
				t.Fatalf("recorded %d event(s) and execed %d time(s), want exactly one of each", len(probe.events), probe.execs)
			}
			ev := probe.events[0]
			if ev.SessionMode != tc.wantMode || ev.Posture != tc.wantPosture {
				t.Fatalf("classification = %s/%s, want %s/%s", ev.SessionMode, ev.Posture, tc.wantMode, tc.wantPosture)
			}
			if ev.Harness != tc.harness || ev.Model != tc.wantModel {
				t.Fatalf("harness/model = %s/%q, want %s/%q", ev.Harness, ev.Model, tc.harness, tc.wantModel)
			}
			if ev.TS != "2026-08-13T19:42:17Z" || ev.Event != launch.UsageEventExecAttempt {
				t.Fatalf("ts/event = %s/%s", ev.TS, ev.Event)
			}
			assertNoArgvLeak(t, ev, tc.args)
		})
	}
}

func assertNoArgvLeak(t *testing.T, ev launch.UsageEventV1, args []string) {
	t.Helper()
	row, err := launch.EncodeUsageEvent(ev)
	if err != nil {
		t.Fatalf("EncodeUsageEvent: %v", err)
	}
	// Skip tokens that are also legitimate row values, or too short to be
	// evidence: "agents" is a posture, "resume" is a session mode, and a bare
	// "-p" reduces to one letter that matches half the row. Only distinctive
	// tokens prove the point — a check that flags a correct row is a broken
	// check, not a finding.
	legitimate := map[string]bool{
		"new": true, "resume": true, "fork": true, "unknown": true,
		"default": true, "builder": true, "agents": true,
		"claude": true, "codex": true, "opus": true, "exec_attempt": true,
	}
	for _, arg := range args {
		trimmed := strings.TrimLeft(arg, "-")
		if len(trimmed) < 5 || legitimate[trimmed] {
			continue
		}
		if strings.Contains(string(row), trimmed) {
			t.Fatalf("event row leaked the harness argument %q: %s", arg, row)
		}
	}
}

// captureStdio returns everything the callee wrote to the real os.Stdout and
// os.Stderr. launchExec banners straight to os.Stderr, so a cobra buffer would
// not see the bytes this test exists to compare.
func captureStdio(t *testing.T, run func()) string {
	t.Helper()
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writeEnd, writeEnd

	done := make(chan string, 1)
	go func() {
		var buf strings.Builder
		io.Copy(&buf, readEnd) //nolint:errcheck // the pipe close ends the copy
		done <- buf.String()
	}()

	run()

	os.Stdout, os.Stderr = prevOut, prevErr
	writeEnd.Close() //nolint:errcheck // closing is what ends the copy above
	captured := <-done
	readEnd.Close() //nolint:errcheck
	return captured
}

// debugLoggerToStderr mirrors log_level="debug" with log_file="-". The wall
// clock is dropped from every record: two runs a millisecond apart differ only
// in their timestamps, and a comparison that flagged that would be measuring
// the clock rather than the behavior under test.
func debugLoggerToStderr() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	}))
}

// TestLaunchExec_RecordFailureIsByteIdenticalToDisabled is the invariant the
// whole feature rests on: turning collection on, and having it fail in every
// way it can, must not move a single byte of what launch prints or change how
// many times it execs. Debug logging is pointed at stderr for the comparison,
// because that is where a stray slog line would surface — and it is the
// configuration a debugging session actually runs under.
func TestLaunchExec_RecordFailureIsByteIdenticalToDisabled(t *testing.T) {
	cfg := usageConfig(t, "claude", false)
	args := []string{"agents", "list", "--json"}

	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	baselineProbe := installUsageProbe(t)
	baseline := captureStdio(t, func() {
		slog.SetDefault(debugLoggerToStderr())
		if err := launchExec(nil, cfg, args); err != nil {
			t.Errorf("disabled launchExec: %v", err)
		}
	})
	if baselineProbe.execs != 1 {
		t.Fatalf("disabled run execed %d times, want 1", baselineProbe.execs)
	}

	enabled := cfg
	enabled.Launch.UsageStats = true
	for _, failure := range []error{
		launch.ErrUsageBusy,
		launch.ErrUsageUnsafeStore,
		launch.ErrUsageRowTooLarge,
		launch.ErrUsageInvalidEvent,
		launch.ErrUsageUnsupported,
		io.ErrShortWrite,
		os.ErrPermission,
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			probe := installUsageProbe(t)
			probe.failWith = failure
			got := captureStdio(t, func() {
				slog.SetDefault(debugLoggerToStderr())
				if err := launchExec(nil, enabled, args); err != nil {
					t.Errorf("enabled launchExec: %v", err)
				}
			})
			if got != baseline {
				t.Fatalf("a %v recorder failure changed the output:\n%q\nwant\n%q", failure, got, baseline)
			}
			if probe.execs != 1 {
				t.Fatalf("execed %d times, want exactly 1", probe.execs)
			}
			if len(probe.events) != 1 {
				t.Fatalf("recorded %d event(s), want exactly 1 attempt even though it failed", len(probe.events))
			}
		})
	}
}

func TestLaunchExec_PreExecBlockersRecordZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		mutate  func(*config.Config)
		args    []string
	}{
		{"invalid profile", "claude", func(c *config.Config) { c.Launch.Defaults.Harness = "aider" }, nil},
		{"unresolvable binary", "claude", func(c *config.Config) { c.Launch.Defaults.BinaryPath = "/nonexistent/harness" }, nil},
		{"codex agents refusal", "codex", func(*config.Config) {}, []string{"agents", "list"}},
		{"pi agents refusal", "pi", func(*config.Config) {}, []string{"agents", "list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := installUsageProbe(t)
			cfg := usageConfig(t, tc.harness, true)
			tc.mutate(&cfg)
			if err := launchExec(nil, cfg, tc.args); err == nil {
				t.Fatal("launchExec succeeded; this case must refuse before exec")
			}
			if len(probe.events) != 0 || probe.execs != 0 {
				t.Fatalf("a refused launch recorded %d event(s) and execed %d time(s), want none",
					len(probe.events), probe.execs)
			}
		})
	}
}

func TestLaunchExec_DisabledRecordsNothingAndStillExecs(t *testing.T) {
	probe := installUsageProbe(t)
	if err := launchExec(nil, usageConfig(t, "claude", false), nil); err != nil {
		t.Fatalf("launchExec: %v", err)
	}
	if len(probe.enabled) != 1 || probe.enabled[0] {
		t.Fatalf("recorder called with enabled=%v, want exactly one disabled call", probe.enabled)
	}
	if len(probe.events) != 0 {
		t.Fatalf("recorded %d events while disabled, want 0", len(probe.events))
	}
	if probe.execs != 1 {
		t.Fatalf("execed %d times, want exactly 1", probe.execs)
	}
}
