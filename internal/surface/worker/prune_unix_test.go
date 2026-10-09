//go:build unix

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueuePruneIf(t *testing.T) {
	q, _ := testQueue(t)
	for _, n := range []string{"a", "b", "c"} {
		mustEnqueue(t, q, n, "brief "+n)
	}
	var seen []string
	removed, err := q.PruneIf(func(r QueueRow) bool { return r.Name != "b" }, func(rows []QueueRow) error {
		for _, r := range rows {
			seen = append(seen, r.Name)
		}
		return nil
	})
	if err != nil || len(removed) != 2 || strings.Join(seen, ",") != "a,c" {
		t.Fatalf("removed %v, before saw %v, err %v", removed, seen, err)
	}
	rows, err := q.Rows()
	if err != nil || len(rows) != 1 || rows[0].Name != "b" {
		t.Fatalf("left %v, %v", rows, err)
	}

	// A failing before removes nothing.
	if _, err := q.PruneIf(func(QueueRow) bool { return true }, func([]QueueRow) error { return errors.New("no") }); err == nil {
		t.Fatal("a failing before reported success")
	}
	if rows, _ := q.Rows(); len(rows) != 1 {
		t.Fatalf("a failing before removed rows: %v", rows)
	}
	// Nothing to remove: before is not called.
	called := false
	if removed, err := q.PruneIf(func(QueueRow) bool { return false }, func([]QueueRow) error { called = true; return nil }); err != nil || len(removed) != 0 || called {
		t.Fatalf("removed %v err %v called %v", removed, err, called)
	}
}

func TestQueueClosersFieldsRoundTrip(t *testing.T) {
	q, dir := testQueue(t)
	r := mustEnqueue(t, q, "w", "brief")
	at := queueNow.Add(time.Hour)
	cost := 3.5
	if _, err := q.UpdateIf("w", SameRead(r), queueNow, func(r *QueueRow) { r.PRClosedAt, r.CostUSD = &at, &cost }); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
	data, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"pr_closed_at": "2026-10-07T13:00:00Z"`) || !strings.Contains(string(data), `"cost_usd": 3.5`) {
		t.Fatalf("queue.json:\n%s", data)
	}
	rows, err := q.Rows()
	if err != nil || rows[0].PRClosedAt == nil || !rows[0].PRClosedAt.Equal(at) || *rows[0].CostUSD != 3.5 {
		t.Fatalf("read back %+v, %v", rows, err)
	}
	// A negative cost is not one forgectl wrote.
	bad := strings.Replace(string(data), `"cost_usd": 3.5`, `"cost_usd": -1`, 1)
	//nolint:gosec // G703: writing a file this test owns under its own temp dir
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Rows(); !errors.Is(err, ErrQueueUnreadable) {
		t.Fatalf("negative cost: %v", err)
	}
}

func TestListLedgers(t *testing.T) {
	state := t.TempDir()
	if ids, bad, err := listLedgersAt(state); err != nil || len(ids) != 0 || len(bad) != 0 {
		t.Fatalf("no directory: %v %v %v", ids, bad, err)
	}
	for _, id := range []LedgerID{{"/repo/one", "s1"}, {"/repo/two", "s2"}} {
		l, err := openAt(state, id.Repo, id.Session)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Begin(Row{Name: "w", Branch: "worker/w"}); err != nil {
			t.Fatal(err)
		}
	}
	q := openQueueAt(state)
	mustEnqueue(t, q, "x", "brief") // queue.json is not a ledger
	dir := filepath.Join(state, "forgectl", "surface")
	// A file named like a ledger but holding another key's repo.
	src := filepath.Join(dir, ledgerKey("/repo/one", "s1")+".json")
	//nolint:gosec // G304: a file this test wrote under its own temp dir
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	misnamed := strings.Repeat("0", 32) + ".json"
	//nolint:gosec // G703: writing a file this test owns under its own temp dir
	if err := os.WriteFile(filepath.Join(dir, misnamed), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ids, bad, err := listLedgersAt(state)
	if err != nil {
		t.Fatal(err)
	}
	got := map[LedgerID]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[LedgerID{"/repo/one", "s1"}] || !got[LedgerID{"/repo/two", "s2"}] {
		t.Fatalf("ids %v", ids)
	}
	if len(bad) != 1 || bad[0] != misnamed {
		t.Fatalf("bad %v, want the misnamed file", bad)
	}
}
