//go:build unix

package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// A held item (desk.StateHeld) is its own row kind: shown, skippable, never
// runnable, and not counted as waiting for the operator.
func TestDeskRowsShowAHeldItemAsHeld(t *testing.T) {
	snap := &desk.Snapshot{Pending: []desk.Item{
		{Name: "01-a", State: desk.StateWaiting},
		{Name: "02-b", State: desk.StateHeld, Hold: "waits on 01-a, which has not finished"},
		{Name: "03-c", State: desk.StateRefused, Refusal: "bad"},
	}}
	rows := deskRows(snap, time.Now())
	want := []rowKind{rowWaiting, rowHeld, rowRefused}
	for i, r := range rows {
		if r.kind != want[i] {
			t.Errorf("row %d (%s): kind %d, want %d", i, r.item.Name, r.kind, want[i])
		}
	}
	if rows[1].runnable() {
		t.Error("a held row is runnable")
	}
	if rowLabel(rowHeld) != "held" {
		t.Errorf("label %q, want held", rowLabel(rowHeld))
	}
	for _, e := range deskTimeline(snap) {
		if e.row.item.Name == "02-b" && (e.row.kind != rowHeld || e.needsYou()) {
			t.Errorf("timeline: held item kind %d, needsYou %v", e.row.kind, e.needsYou())
		}
	}
}

// A TTY item whose home directory cannot be used is released to skipped/
// (launch-failed) before any run begins, as before ApplyItemEnv: no run is
// recorded and script(1) is never built.
func TestTTYRunWithNoHomeReleasesTheClaim(t *testing.T) {
	t.Setenv("HOME", "relative")
	h := newDeskHarness(t)
	h.drop("01-console.sh", ttyScript("console"))
	h.scan()
	snap, err := h.d.Scan()
	if err != nil || len(snap.Pending) != 1 {
		t.Fatalf("scan: %v, %d pending", err, len(snap.Pending))
	}
	c, err := h.backend.Claim("01-console", snap.Pending[0].Meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	rcPath := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rcPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTTYRun(h.backend, c, rcPath, func(string) []string { t.Fatal("script(1) was built"); return nil }); err == nil {
		t.Fatal("newTTYRun with a relative HOME succeeded")
	}
	if got := h.where("01-console"); got != desk.DirSkipped {
		t.Errorf("item in %s, want skipped", got)
	}
	if _, err := os.Stat(h.d.LogPath("01-console")); err == nil {
		t.Error("a run log was created")
	}
}
