//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// testPruneDeps is prune over the real queue, ledgers and usage file under
// the test's XDG_STATE_HOME, with herdr replaced: live names the ledger rows
// whose workspace herdr calls live (any other is gone), and herdrErr makes
// herdr unreadable.
func testPruneDeps(t *testing.T, q *worker.Queue, live map[string]bool, herdrErr error) pruneDeps {
	t.Helper()
	files, err := worker.OpenDrainFiles()
	if err != nil {
		t.Fatal(err)
	}
	return pruneDeps{
		queue:   q,
		ledgers: worker.ListLedgers,
		ledger:  func(id worker.LedgerID) (pruneLedger, error) { return worker.Open(id.Repo, id.Session) },
		workspace: func(context.Context) (func(worker.LedgerID, worker.Row) drain.Workspace, error) {
			if herdrErr != nil {
				return nil, herdrErr
			}
			return func(_ worker.LedgerID, r worker.Row) drain.Workspace {
				if live[r.Name] {
					return drain.WorkspaceLive
				}
				return drain.WorkspaceGone
			}, nil
		},
		appendUsage: files.AppendUsage,
		cache:       mustStatusCache(t),
	}
}

func mustStatusCache(t *testing.T) worker.StatusCache {
	t.Helper()
	c, err := worker.OpenStatusCache()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const pruneRepo = "/repo/p"

var pruneNow = drainT0.Add(90 * 24 * time.Hour)

// seedPruneRow puts a row named name in state at the time at; launched rows
// carry a launch id and the test session, as the drain leaves them.
func seedPruneRow(t *testing.T, q *worker.Queue, name string, state worker.QueueState, at time.Time, launched bool, cost *float64) {
	t.Helper()
	enqueueAt(t, q, name, pruneRepo, at)
	r := rowNamed(t, q, name)
	if launched {
		var err error
		if r, err = q.ClaimFor(name, "launch-"+name, drainTestSession, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.UpdateIf(name, worker.SameRead(r), at, func(r *worker.QueueRow) { r.State, r.CostUSD = state, cost }); err != nil {
		t.Fatal(err)
	}
}

// seedPruneLedger writes a ledger row in pruneRepo started at started; ref
// gives it a workspace reference.
func seedPruneLedger(t *testing.T, name, launchID string, stage worker.Stage, started time.Time, ref bool) {
	t.Helper()
	seedLedger(t, pruneRepo, name, launchID, stage, func(r *worker.Row) {
		r.StartedAt = started
		if ref {
			r.Ref = json.RawMessage(`{"ref":1}`)
		}
	})
}

func queueNames(t *testing.T, q *worker.Queue) []string {
	t.Helper()
	rows, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

func ledgerNames(t *testing.T) []string {
	t.Helper()
	led, err := worker.Open(pruneRepo, drainTestSession)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := led.Rows()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

func usageFile(t *testing.T) string {
	t.Helper()
	files, err := worker.OpenDrainFiles()
	if err != nil {
		t.Fatal(err)
	}
	data, err := files.ReadUsage()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func itemNames(items []drain.PruneItem) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Kind+":"+it.Name)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// seedPruneWorld is one queue and one ledger with a row for each case.
func seedPruneWorld(t *testing.T) *worker.Queue {
	t.Helper()
	_, _, q := newFakeDrain(t)
	old := pruneNow.Add(-drain.PruneAfter - time.Hour)
	young := pruneNow.Add(-drain.PruneAfter + time.Hour)
	day1 := time.Date(old.Year(), old.Month(), old.Day(), 1, 0, 0, 0, time.UTC)
	seedPruneRow(t, q, "closed-a", worker.QueueClosed, day1, true, ptr(1.5))
	seedPruneRow(t, q, "closed-b", worker.QueueClosed, day1.Add(time.Hour), true, ptr(0.25))
	seedPruneRow(t, q, "expired", worker.QueueExpired, day1.Add(-48*time.Hour), false, nil)
	seedPruneRow(t, q, "failed-live", worker.QueueFailed, old, true, ptr(9))
	seedPruneLedger(t, "failed-live", "launch-failed-live", worker.StageFailed, old, true)
	seedPruneRow(t, q, "failed-gone", worker.QueueFailed, old, true, nil)
	seedPruneLedger(t, "failed-gone", "launch-failed-gone", worker.StageFailed, old, true)
	seedPruneRow(t, q, "closed-young", worker.QueueClosed, young, true, ptr(3))
	seedPruneRow(t, q, "reported-old", worker.QueueReported, old, true, nil)
	seedPruneRow(t, q, "reported-live", worker.QueueReported, old, true, nil)
	seedPruneLedger(t, "reported-live", "launch-reported-live", worker.StageLaunched, old, true)
	seedClosedLedger(t, "kept-wt", old.Add(-24*time.Hour), old)
	seedClosedLedger(t, "kept-wt-young", old.Add(-24*time.Hour), young)
	return q
}

// seedClosedLedger writes a closed ledger row in pruneRepo, started at
// started and closed at closed.
func seedClosedLedger(t *testing.T, name string, started, closed time.Time) {
	t.Helper()
	seedLedger(t, pruneRepo, name, "", worker.StageClosed, func(r *worker.Row) {
		r.StartedAt, r.ClosedAt, r.Worktree = started, &closed, "/wt/"+name
	})
}

func TestRunPruneRemovesOldRowsKeepsLive(t *testing.T) {
	q := seedPruneWorld(t)
	res, err := runPrune(t.Context(), testPruneDeps(t, q, map[string]bool{"failed-live": true, "reported-live": true}, nil), pruneNow, drain.PruneAfter, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := itemNames(res.Removed), "ledger:kept-wt,queue:closed-a,queue:closed-b,queue:expired,queue:failed-gone,queue:reported-old"; got != want {
		t.Fatalf("removed %s, want %s", got, want)
	}
	if got := strings.Join(queueNames(t, q), ","); got != "closed-young,failed-live,reported-live" {
		t.Fatalf("queue left %s", got)
	}
	if got := strings.Join(ledgerNames(t), ","); got != "failed-gone,failed-live,kept-wt-young,reported-live" {
		t.Fatalf("ledger left %s", got)
	}
	for _, it := range res.Kept {
		if (it.Name == "failed-live" || it.Name == "reported-live") && it.Kind == drain.PruneKindQueue && !strings.Contains(it.Reason, "still live") {
			t.Fatalf("%s kept for %q", it.Name, it.Reason)
		}
	}
	day := pruneNow.Add(-drain.PruneAfter - time.Hour).UTC().Format(worker.UTCDayLayout)
	want := `{"day":"` + day + `","name":"closed-a","launch_id":"launch-closed-a","costUsd":1.5}` + "\n" +
		`{"day":"` + day + `","name":"closed-b","launch_id":"launch-closed-b","costUsd":0.25}` + "\n"
	if got := usageFile(t); got != want {
		t.Fatalf("usage-daily.jsonl %q, want %q", got, want)
	}
}

func TestRunPruneHerdrUnreadable(t *testing.T) {
	q := seedPruneWorld(t)
	res, err := runPrune(t.Context(), testPruneDeps(t, q, nil, errors.New("herdr session not running")), pruneNow, drain.PruneAfter, false)
	if err != nil {
		t.Fatal(err)
	}
	// Rows with no workspace go; a row whose ledger row has one stays, and
	// no ledger row is removed.
	if got, want := itemNames(res.Removed), "queue:closed-a,queue:closed-b,queue:expired,queue:reported-old"; got != want {
		t.Fatalf("removed %s, want %s", got, want)
	}
	if got := strings.Join(ledgerNames(t), ","); got != "failed-gone,failed-live,kept-wt,kept-wt-young,reported-live" {
		t.Fatalf("ledger left %s; herdr unreadable must remove no ledger row", got)
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "herdr could not be read") }) {
		t.Fatalf("notes %q do not say herdr was unreadable", res.Notes)
	}
}

func TestRunPruneDryRunWritesNothing(t *testing.T) {
	q := seedPruneWorld(t)
	before, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	res, err := runPrune(t.Context(), testPruneDeps(t, q, map[string]bool{"failed-live": true}, nil), pruneNow, drain.PruneAfter, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || itemNames(res.Removed) == "" || len(res.Usage) != 2 || res.Usage[0].CostUSD+res.Usage[1].CostUSD != 1.75 {
		t.Fatalf("dry run: %+v", res)
	}
	after, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("dry run removed queue rows: %d -> %d", len(before), len(after))
	}
	if got := len(ledgerNames(t)); got != 5 {
		t.Fatalf("dry run removed ledger rows: %d left", got)
	}
	if got := usageFile(t); got != "" {
		t.Fatalf("dry run wrote usage %q", got)
	}
}

func TestRunPruneUsageAppendOnly(t *testing.T) {
	_, _, q := newFakeDrain(t)
	d1 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedPruneRow(t, q, "a", worker.QueueClosed, d1, false, ptr(1))
	deps := testPruneDeps(t, q, nil, nil)
	if _, err := runPrune(t.Context(), deps, pruneNow, drain.PruneAfter, false); err != nil {
		t.Fatal(err)
	}
	seedPruneRow(t, q, "b", worker.QueueClosed, d1, false, ptr(2))
	seedPruneRow(t, q, "c", worker.QueueFailed, d1.Add(24*time.Hour), false, ptr(0.5))
	seedPruneRow(t, q, "unpriced", worker.QueueClosed, d1, false, nil)
	if _, err := runPrune(t.Context(), deps, pruneNow, drain.PruneAfter, false); err != nil {
		t.Fatal(err)
	}
	want := `{"day":"2026-08-01","name":"a","launch_id":"","costUsd":1}` + "\n" +
		`{"day":"2026-08-01","name":"b","launch_id":"","costUsd":2}` + "\n" +
		`{"day":"2026-08-02","name":"c","launch_id":"","costUsd":0.5}` + "\n"
	if got := usageFile(t); got != want {
		t.Fatalf("usage-daily.jsonl\n%s\nwant\n%s", got, want)
	}
	if got := queueNames(t, q); len(got) != 0 {
		t.Fatalf("queue left %v", got)
	}
}

func TestRunPruneUsageFailureRemovesNothing(t *testing.T) {
	_, _, q := newFakeDrain(t)
	seedPruneRow(t, q, "a", worker.QueueClosed, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), false, ptr(1))
	deps := testPruneDeps(t, q, nil, nil)
	deps.appendUsage = func([]byte) error { return errors.New("disk full") }
	if _, err := runPrune(t.Context(), deps, pruneNow, drain.PruneAfter, false); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v; want the usage failure", err)
	}
	if got := queueNames(t, q); len(got) != 1 {
		t.Fatalf("queue %v; a row whose cost could not be recorded must stay", got)
	}
}

func TestSurfacePruneUsageErrors(t *testing.T) {
	for _, arg := range []string{"30m", "0d", "soon"} {
		cmd := newSurfacePruneCmd(module.Deps{})
		cmd.SetArgs([]string{"--older-than", arg})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		err := cmd.Execute()
		var coded interface{ ExitCode() int }
		if !errors.As(err, &coded) || coded.ExitCode() != exitUsage {
			t.Fatalf("--older-than %s: %v; want exit %d", arg, err, exitUsage)
		}
	}
}

// TestSurfacePruneStateUnreadableExitsOne pins ADR-0015's split: state
// prune cannot open is exit 1 (failed), and 2 stays for usage errors.
func TestSurfacePruneStateUnreadableExitsOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	cmd := newSurfacePruneCmd(module.Deps{})
	cmd.SetArgs([]string{"--dry-run"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != exitFailed {
		t.Fatalf("%v: exit %d, want %d", err, ExitCode(err), exitFailed)
	}
}

func TestDrainFilesPruneDayAndUsage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	files, err := worker.OpenDrainFiles()
	if err != nil {
		t.Fatal(err)
	}
	if day, err := files.ReadPruneDay(); err != nil || day != "" {
		t.Fatalf("fresh: %q, %v", day, err)
	}
	if err := files.WritePruneDay("2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if day, err := files.ReadPruneDay(); err != nil || day != "2026-10-09" {
		t.Fatalf("after write: %q, %v", day, err)
	}
	if err := files.WritePruneDay("10/09/2026"); err == nil {
		t.Fatal("a malformed day was written")
	}
	if err := files.AppendUsage([]byte("no newline")); err == nil {
		t.Fatal("a partial line was appended")
	}
}

func TestRunPruneStatusCache(t *testing.T) {
	_, _, q := newFakeDrain(t)
	c := mustStatusCache(t)
	oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, h := range []string{oldHead, newHead} {
		if err := c.Write(h, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	state := os.Getenv("XDG_STATE_HOME")
	old := pruneNow.Add(-drain.PruneAfter - time.Hour)
	if err := os.Chtimes(filepath.Join(state, "forgectl", "surface", "status-cache-"+oldHead+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	young := pruneNow.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(state, "forgectl", "surface", "status-cache-"+newHead+".json"), young, young); err != nil {
		t.Fatal(err)
	}
	res, err := runPrune(t.Context(), testPruneDeps(t, q, nil, nil), pruneNow, drain.PruneAfter, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(res.Removed); got != "status-cache:"+oldHead {
		t.Fatalf("removed %s", got)
	}
	entries, err := c.Entries()
	if err != nil || len(entries) != 1 || entries[0].Head != newHead {
		t.Fatalf("left %+v, %v", entries, err)
	}
}

// TestRunPruneKeepsRowsOfAnUnreadableLedgerFile pins that a ledger file
// prune cannot read at all keeps every launched queue row whose ledger is
// not among the readable ones, naming the file.
func TestRunPruneKeepsRowsOfAnUnreadableLedgerFile(t *testing.T) {
	_, _, q := newFakeDrain(t)
	old := pruneNow.Add(-drain.PruneAfter - time.Hour)
	seedPruneRow(t, q, "failed-elsewhere", worker.QueueFailed, old, true, nil)
	seedPruneRow(t, q, "never-launched", worker.QueueExpired, old, false, nil)
	const badName = "0123456789abcdef0123456789abcdef.json"
	surface := filepath.Join(os.Getenv("XDG_STATE_HOME"), "forgectl", "surface")
	if err := os.WriteFile(filepath.Join(surface, badName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := runPrune(t.Context(), testPruneDeps(t, q, nil, nil), pruneNow, drain.PruneAfter, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(res.Removed); got != "queue:never-launched" {
		t.Fatalf("removed %s; a launched row whose ledger may be the unreadable file stays", got)
	}
	kept := slices.IndexFunc(res.Kept, func(it drain.PruneItem) bool { return it.Name == "failed-elsewhere" })
	if kept < 0 || !strings.Contains(res.Kept[kept].Reason, badName) {
		t.Fatalf("kept %+v; the reason names %s", res.Kept, badName)
	}
}

// TestRunPruneClosedRowWithoutClosedAt pins that a closed ledger row from
// before closed_at existed goes only once its recorded worktree is gone.
func TestRunPruneClosedRowWithoutClosedAt(t *testing.T) {
	_, _, q := newFakeDrain(t)
	old := pruneNow.Add(-drain.PruneAfter - time.Hour)
	present := t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed")
	for name, wt := range map[string]string{"untimed-present": present, "untimed-gone": gone} {
		seedLedger(t, pruneRepo, name, "", worker.StageClosed, func(r *worker.Row) { r.StartedAt, r.Worktree = old, wt })
	}
	deps := testPruneDeps(t, q, nil, nil)
	deps.worktreeGone = pathGone
	res, err := runPrune(t.Context(), deps, pruneNow, drain.PruneAfter, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(res.Removed); got != "ledger:untimed-gone" {
		t.Fatalf("removed %s", got)
	}
	if got := strings.Join(ledgerNames(t), ","); got != "untimed-present" {
		t.Fatalf("ledger left %s", got)
	}
}
