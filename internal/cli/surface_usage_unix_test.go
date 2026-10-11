//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func runUsageCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newSurfaceUsageCmd(module.Deps{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSurfaceUsageSumsFileAndQueue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cost := 2.5
	seedPruneRow(t, q, "live-a", worker.QueueClosed, now, true, &cost)
	seedPruneRow(t, q, "unpriced", worker.QueueClosed, now, true, nil)
	files, err := worker.OpenDrainFiles()
	if err != nil {
		t.Fatal(err)
	}
	day := now.UTC().Format(worker.UTCDayLayout)
	if err := files.AppendUsage([]byte(`{"day":"` + day + `","name":"pruned","launch_id":"l9","costUsd":1.5}` + "\n" + "garbage\n")); err != nil {
		t.Fatal(err)
	}

	out, err := runUsageCmd(t, "--json")
	if err != nil {
		t.Fatalf("usage --json: %v\n%s", err, out)
	}
	var got drain.Spend
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.Days != 30 || got.TotalUSD != 4 || got.Skipped != 1 || len(got.ByDay) != 1 || got.ByDay[0].Rows != 2 {
		t.Fatalf("usage --json = %+v", got)
	}

	text, err := runUsageCmd(t, "--days", "7")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{day + "  $4.00  2 workers", "total over 7 days: $4.00", "skipped 1 entry"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output missing %q:\n%s", want, text)
		}
	}
}

func TestSurfaceUsageEmptyState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, err := runUsageCmd(t, "--json")
	if err != nil {
		t.Fatalf("usage on an empty state: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	// by_day is [] rather than null, so a reader can range over it.
	if byDay, ok := got["by_day"].([]any); !ok || len(byDay) != 0 || got["total_usd"] != 0.0 || got["skipped"] != 0.0 {
		t.Fatalf("empty usage = %s", out)
	}
}

func TestSurfaceUsageUnreadableStateExitsFailed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if _, err := runUsageCmd(t, "--json"); err == nil || ExitCode(err) != exitFailed {
		t.Fatalf("unreadable state: %v, want exit %d", err, exitFailed)
	}
}

func TestSurfaceUsageRejectsBadDays(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, d := range []string{"0", "366", "-1"} {
		if _, err := runUsageCmd(t, "--days", d); err == nil || ExitCode(err) != exitUsage {
			t.Errorf("--days %s: %v, want exit %d", d, err, exitUsage)
		}
	}
}
