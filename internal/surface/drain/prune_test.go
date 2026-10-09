package drain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func cost(v float64) *float64 { return &v }

// pqrow is a launched-then-ended queue row in session s1.
func pqrow(name string, state worker.QueueState, at time.Time) worker.QueueRow {
	r := qrow(name, "/r", state, at)
	r.Session = "s1"
	return r
}

var pid = worker.LedgerID{Repo: "/r", Session: "s1"}

func lrow(name, launchID string, stage worker.Stage, started time.Time, ref bool) worker.Row {
	r := worker.Row{Name: name, LaunchID: launchID, Stage: stage, StartedAt: started}
	if ref {
		r.Ref = json.RawMessage(`{"ref":1}`)
	}
	return r
}

func planNames(items []PruneItem) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Kind+":"+it.Name)
	}
	return strings.Join(out, ",")
}

func reasonOf(t *testing.T, items []PruneItem, kind, name string) string {
	t.Helper()
	for _, it := range items {
		if it.Kind == kind && it.Name == name {
			return it.Reason
		}
	}
	t.Fatalf("no %s item %s in %s", kind, name, planNames(items))
	return ""
}

func TestPlanPrune(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	old := now.Add(-PruneAfter - time.Hour)
	young := now.Add(-PruneAfter + time.Hour)
	live := map[string]Workspace{"failed-live": WorkspaceLive, "failed-unknown": WorkspaceUnknown,
		"reported-live": WorkspaceLive, "reported-unknown": WorkspaceUnknown}
	in := PruneInput{
		Now: now, OlderThan: PruneAfter,
		Queue: []worker.QueueRow{
			pqrow("closed-old", worker.QueueClosed, old),
			pqrow("failed-live", worker.QueueFailed, old),
			pqrow("failed-unknown", worker.QueueFailed, old),
			pqrow("failed-gone", worker.QueueFailed, old),
			pqrow("expired-old", worker.QueueExpired, old),
			pqrow("closed-young", worker.QueueClosed, young),
			pqrow("reported-old", worker.QueueReported, old),
			pqrow("reported-gone", worker.QueueReported, old),
			pqrow("reported-live", worker.QueueReported, old),
			pqrow("reported-unknown", worker.QueueReported, old),
			pqrow("reported-young", worker.QueueReported, young),
			pqrow("queued-old", worker.QueueQueued, old),
		},
		Ledgers: []PruneLedger{{ID: pid, Rows: []worker.Row{
			lrow("failed-live", "L-failed-live", worker.StageFailed, old, true),
			lrow("failed-unknown", "L-failed-unknown", worker.StageFailed, old, true),
			lrow("failed-gone", "L-failed-gone", worker.StageFailed, old, true),
			lrow("reported-gone", "L-reported-gone", worker.StageLaunched, old, true),
			lrow("reported-live", "L-reported-live", worker.StageLaunched, old, true),
			lrow("reported-unknown", "L-reported-unknown", worker.StageLaunched, old, true),
			closedAt(lrow("kept-wt-old", "", worker.StageClosed, young, true), old),
			closedAt(lrow("kept-wt-young", "", worker.StageClosed, old, true), young),
			lrow("running", "", worker.StageLaunched, old, true),
		}}},
		Workspace: func(_ worker.LedgerID, r worker.Row) Workspace {
			if w, ok := live[r.Name]; ok {
				return w
			}
			return WorkspaceGone
		},
	}
	p := PlanPrune(in)
	if got, want := planNames(p.Remove), "queue:closed-old,queue:failed-gone,queue:expired-old,queue:reported-old,queue:reported-gone,ledger:kept-wt-old"; got != want {
		t.Fatalf("remove %s, want %s", got, want)
	}
	for name, want := range map[string]string{
		"failed-live":      "still live",
		"failed-unknown":   "cannot say",
		"closed-young":     "not older than the cutoff",
		"reported-live":    "still live",
		"reported-unknown": "cannot say",
		"reported-young":   "not older than the cutoff",
		"queued-old":       "state queued is not pruned",
	} {
		if got := reasonOf(t, p.Keep, PruneKindQueue, name); !strings.Contains(got, want) {
			t.Errorf("%s kept for %q, want %q", name, got, want)
		}
	}
	if got := reasonOf(t, p.Keep, PruneKindLedger, "running"); !strings.Contains(got, "stage launched") {
		t.Errorf("running kept for %q", got)
	}
	if got := reasonOf(t, p.Keep, PruneKindLedger, "kept-wt-young"); !strings.Contains(got, "not older than the cutoff") {
		t.Errorf("kept-wt-young kept for %q", got)
	}
}

func closedAt(r worker.Row, at time.Time) worker.Row {
	r.ClosedAt = &at
	return r
}

// TestPlanPruneClosedLedgerRows pins how closed ledger rows age: by
// closed_at when they have one (a row started long ago but closed
// yesterday stays), and with none only once their recorded worktree is gone.
func TestPlanPruneClosedLedgerRows(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	old := now.Add(-PruneAfter - time.Hour)
	young := now.Add(-PruneAfter + time.Hour)
	withWT := func(r worker.Row, path string) worker.Row { r.Worktree = path; return r }
	asked := map[string]bool{}
	in := PruneInput{Now: now, OlderThan: PruneAfter,
		Ledgers: []PruneLedger{{ID: pid, Rows: []worker.Row{
			withWT(closedAt(lrow("closed-yesterday", "", worker.StageClosed, old, false), young), "/wt/a"),
			withWT(closedAt(lrow("closed-long-ago", "", worker.StageClosed, old, false), old), "/wt/b"),
			withWT(lrow("untimed-gone", "", worker.StageClosed, old, false), "/wt/gone"),
			withWT(lrow("untimed-present", "", worker.StageClosed, old, false), "/wt/present"),
			withWT(lrow("untimed-unknown", "", worker.StageClosed, old, false), "/wt/unknown"),
			lrow("untimed-no-path", "", worker.StageClosed, old, false),
			withWT(lrow("untimed-young", "", worker.StageClosed, young, false), "/wt/gone-young"),
		}}},
		WorktreeGone: func(path string) (bool, error) {
			asked[path] = true
			switch path {
			case "/wt/present":
				return false, nil
			case "/wt/unknown":
				return false, errors.New("permission denied")
			}
			return true, nil
		},
	}
	p := PlanPrune(in)
	if got, want := planNames(p.Remove), "ledger:closed-long-ago,ledger:untimed-gone"; got != want {
		t.Fatalf("remove %s, want %s", got, want)
	}
	for name, want := range map[string]string{
		"closed-yesterday": "not older than the cutoff",
		"untimed-present":  "still exists",
		"untimed-unknown":  "cannot be checked: permission denied",
		"untimed-no-path":  "no recorded worktree path",
		"untimed-young":    "has no closed_at, not older than the cutoff",
	} {
		if got := reasonOf(t, p.Keep, PruneKindLedger, name); !strings.Contains(got, want) {
			t.Errorf("%s kept for %q, want %q", name, got, want)
		}
	}
	if asked["/wt/a"] || asked["/wt/b"] || asked["/wt/gone-young"] {
		t.Fatalf("the worktree was checked for a row with closed_at or inside the cutoff: %v", asked)
	}
}

// TestPlanPruneBadLedgerFileKeepsRows pins that while a ledger file cannot
// be read at all, a launched queue row whose ledger is not among the readable
// ones is kept: its ledger row may be in that file.
func TestPlanPruneBadLedgerFileKeepsRows(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	old := now.Add(-PruneAfter - time.Hour)
	unlaunched := pqrow("never-launched", worker.QueueExpired, old)
	unlaunched.LaunchID, unlaunched.Session = "", ""
	in := PruneInput{Now: now, OlderThan: PruneAfter,
		Queue:      []worker.QueueRow{pqrow("w", worker.QueueFailed, old), unlaunched},
		BadLedgers: []string{"0123456789abcdef0123456789abcdef.json"},
	}
	p := PlanPrune(in)
	if got := planNames(p.Remove); got != "queue:never-launched" {
		t.Fatalf("remove %s; a launched row whose ledger may be the unreadable file stays", got)
	}
	if got := reasonOf(t, p.Keep, PruneKindQueue, "w"); !strings.Contains(got, "0123456789abcdef0123456789abcdef.json") {
		t.Fatalf("w kept for %q; the reason names the unreadable file", got)
	}
	in.BadLedgers = nil
	if got := planNames(PlanPrune(in).Remove); got != "queue:w,queue:never-launched" {
		t.Fatalf("with every ledger readable: remove %s", got)
	}
}

func TestPlanPruneCutoffBoundary(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	at := now.Add(-PruneAfter)
	in := PruneInput{Now: now, OlderThan: PruneAfter, Queue: []worker.QueueRow{
		pqrow("at", worker.QueueClosed, at),
		pqrow("past", worker.QueueClosed, at.Add(-time.Second)),
	}, Ledgers: []PruneLedger{{ID: pid, Rows: []worker.Row{
		closedAt(lrow("l-at", "", worker.StageClosed, at, false), at),
		closedAt(lrow("l-past", "", worker.StageClosed, at, false), at.Add(-time.Second)),
	}}}}
	if got := planNames(PlanPrune(in).Remove); got != "queue:past,ledger:l-past" {
		t.Fatalf("remove %s; a row exactly at the cutoff stays", got)
	}
}

func TestPlanPruneHerdrUnreadable(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	old := now.Add(-PruneAfter - time.Hour)
	called := false
	in := PruneInput{
		Now: now, OlderThan: PruneAfter, HerdrErr: "herdr session not running",
		Queue: []worker.QueueRow{
			pqrow("no-ledger", worker.QueueClosed, old),
			pqrow("with-ws", worker.QueueFailed, old),
		},
		Ledgers: []PruneLedger{{ID: pid, Rows: []worker.Row{
			lrow("with-ws", "L-with-ws", worker.StageFailed, old, true),
			lrow("closed-old", "", worker.StageClosed, old, false),
		}}},
		Workspace: func(worker.LedgerID, worker.Row) Workspace { called = true; return WorkspaceGone },
	}
	p := PlanPrune(in)
	if got := planNames(p.Remove); got != "queue:no-ledger" {
		t.Fatalf("remove %s; with herdr unreadable no ledger row goes, and a row that may be live stays", got)
	}
	if called {
		t.Fatal("herdr was asked while unreadable")
	}
	if got := reasonOf(t, p.Keep, PruneKindLedger, "closed-old"); !strings.Contains(got, "herdr could not be read") {
		t.Fatalf("closed-old kept for %q", got)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "herdr session not running") {
		t.Fatalf("notes %q", p.Notes)
	}
}

func TestPlanPruneUnreadableLedger(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	old := now.Add(-PruneAfter - time.Hour)
	p := PlanPrune(PruneInput{Now: now, OlderThan: PruneAfter, Queue: []worker.QueueRow{pqrow("w", worker.QueueFailed, old)},
		Ledgers: []PruneLedger{{ID: pid, Err: "hardlinked"}}})
	if len(p.Remove) != 0 || !strings.Contains(reasonOf(t, p.Keep, PruneKindQueue, "w"), "ledger could not be read") {
		t.Fatalf("%+v", p)
	}
}

func TestUsageRollup(t *testing.T) {
	day1 := time.Date(2026, 9, 1, 23, 30, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 2, 0, 30, 0, 0, time.UTC)
	rows := []worker.QueueRow{
		{Name: "a", StateAt: day2, CostUSD: cost(2)},
		{Name: "b", StateAt: day1, CostUSD: cost(1.25)},
		{Name: "c", StateAt: day1, CostUSD: cost(0.5)},
		{Name: "d", StateAt: day1}, // unpriced: not counted
	}
	got := UsageRollup(rows)
	want := []UsageDay{{Day: "2026-09-01", CostUSD: 1.75, Rows: 2}, {Day: "2026-09-02", CostUSD: 2, Rows: 1}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("day %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(UsageRollup(nil)) != 0 {
		t.Fatal("no rows, no lines")
	}
}

func TestPruneDue(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 59, 0, 0, time.UTC)
	if !PruneDue("", now) || !PruneDue("2026-10-08", now) {
		t.Fatal("a new day must be due")
	}
	if PruneDue("2026-10-09", now) || PruneDue("2026-10-09", now.Add(-23*time.Hour)) {
		t.Fatal("the same UTC day must not be due")
	}
	if !PruneDue("2026-10-09", now.Add(time.Minute)) {
		t.Fatal("the next UTC day must be due")
	}
}
