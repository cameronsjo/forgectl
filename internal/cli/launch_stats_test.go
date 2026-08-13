package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
)

// scratchUsageStore points config.LaunchUsageBase at a throwaway state root and
// returns the leaf directory, so a test can seed rows or plant damage.
func scratchUsageStore(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)
	return filepath.Join(base, "forgectl")
}

func seedUsageRow(t *testing.T, ts, harness, model, mode, posture string) {
	t.Helper()
	err := launch.RecordUsage(true, launch.UsageEventV1{
		SchemaVersion: launch.UsageSchemaVersion,
		TS:            ts,
		Event:         launch.UsageEventExecAttempt,
		Harness:       harness,
		Model:         model,
		SessionMode:   mode,
		Posture:       posture,
	})
	if err != nil {
		t.Fatalf("seed RecordUsage: %v", err)
	}
}

func pinStatsClock(t *testing.T) {
	t.Helper()
	prev := usageNow
	usageNow = func() time.Time { return time.Date(2026, 8, 13, 19, 42, 17, 0, time.UTC) }
	t.Cleanup(func() { usageNow = prev })
}

func runStats(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newLaunchStatsCmd()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestLaunchStats_JSONExactAllTimeWindowedAndEmpty(t *testing.T) {
	scratchUsageStore(t)
	pinStatsClock(t)

	empty, err := runStats(t, "--json")
	if err != nil {
		t.Fatalf("stats on an empty store: %v", err)
	}
	wantEmpty := `{"schema_version":1,"window":{"days":null,"cutoff":null},"total_attempts":0,` +
		`"first_ts":null,"last_ts":null,"counts":{"harness":{},"model":{},"session_mode":{},"posture":{}},` +
		`"skipped_rows":0}` + "\n"
	if empty != wantEmpty {
		t.Fatalf("empty JSON =\n%s\nwant\n%s", empty, wantEmpty)
	}

	seedUsageRow(t, "2026-08-10T13:00:00Z", "claude", "opus", "new", "default")
	seedUsageRow(t, "2026-08-13T19:40:00Z", "claude", "opus", "fork", "default")
	seedUsageRow(t, "2026-08-12T09:00:00Z", "codex", "", "new", "default")

	windowed, err := runStats(t, "7", "--json")
	if err != nil {
		t.Fatalf("windowed stats: %v", err)
	}
	want := `{"schema_version":1,"window":{"days":7,"cutoff":"2026-08-06T19:42:17Z"},` +
		`"total_attempts":3,"first_ts":"2026-08-10T13:00:00Z","last_ts":"2026-08-13T19:40:00Z",` +
		`"counts":{"harness":{"claude":2,"codex":1},"model":{"":1,"opus":2},` +
		`"session_mode":{"fork":1,"new":2},"posture":{"default":3}},"skipped_rows":0}` + "\n"
	if windowed != want {
		t.Fatalf("windowed JSON =\n%s\nwant\n%s", windowed, want)
	}
}

func TestLaunchStats_SkippedRowsPrintTheReportThenExitOne(t *testing.T) {
	leaf := scratchUsageStore(t)
	pinStatsClock(t)
	seedUsageRow(t, "2026-08-13T10:00:00Z", "claude", "opus", "new", "default")

	data := filepath.Join(leaf, "launch-usage.jsonl")
	existing, err := os.ReadFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, append(existing, []byte("{not json}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, err := runStats(t, "--json")
	if err == nil {
		t.Fatal("stats exited 0 with an unreadable row")
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "skipped 1 unreadable") {
		t.Fatalf("diagnostic = %q, want it to name the skipped row count", err.Error())
	}
	// The complete report still lands on stdout — exactly one JSON object,
	// with the single diagnostic left for the root error handler to render.
	//
	// Assert on the first line rather than all of stdout: a subcommand
	// executed standalone has no parent, so cobra resolves SilenceUsage
	// against the subcommand itself and appends a usage block the real root
	// suppresses. That trailer is a harness artifact, not command behavior.
	firstLine, _, _ := strings.Cut(stdout, "\n")
	if !strings.HasPrefix(firstLine, "{") || !strings.HasSuffix(firstLine, "}") {
		t.Fatalf("first stdout line = %q, want one complete JSON object", firstLine)
	}
	if !strings.Contains(firstLine, `"skipped_rows":1`) {
		t.Fatalf("aggregate = %q, want it to report the skipped row", firstLine)
	}
	if strings.Count(stdout, `"schema_version"`) != 1 {
		t.Fatalf("stdout = %q, want exactly one aggregate object", stdout)
	}
	if strings.Contains(stdout, "skipped 1 unreadable") {
		t.Fatal("the command printed its own warning as well as returning the error")
	}
}

func TestLaunchStats_HumanOutputIsTerminalSafeAndJSONStaysRaw(t *testing.T) {
	scratchUsageStore(t)
	pinStatsClock(t)
	seedUsageRow(t, "2026-08-13T10:00:00Z", "claude", "opus"+string(rune(0x1b))+"[2Kfake", "new", "default")
	seedUsageRow(t, "2026-08-13T11:00:00Z", "codex", "", "new", "default")

	human, err := runStats(t)
	if err != nil {
		t.Fatalf("human stats: %v", err)
	}
	for _, b := range []byte(human) {
		if b < 0x20 && b != '\n' {
			t.Fatalf("human report carries a raw control byte %#x: %q", b, human)
		}
	}
	if !strings.Contains(human, "(harness default)") {
		t.Fatalf("human report does not label the empty model:\n%s", human)
	}

	raw, err := runStats(t, "--json")
	if err != nil {
		t.Fatalf("json stats: %v", err)
	}
	// The machine contract keeps the raw empty key; only the human report
	// substitutes a sentence for it.
	if !strings.Contains(raw, `"model":{"":1,`) || strings.Contains(raw, "(harness default)") {
		t.Fatalf("JSON must keep the raw empty model key, not the human label:\n%s", raw)
	}
}

func TestLaunchStats_DaysArgumentIsPositiveWholeAndOverflowSafe(t *testing.T) {
	scratchUsageStore(t)
	pinStatsClock(t)
	for _, bad := range []string{"0", "-1", "1.5", "seven", "99999999999999999999", "9223372036854775807"} {
		if _, err := runStats(t, bad, "--json"); err == nil {
			t.Fatalf("stats accepted %q as a window", bad)
		}
	}
	if _, err := runStats(t, "1", "--json"); err != nil {
		t.Fatalf("stats rejected a valid window: %v", err)
	}
}
