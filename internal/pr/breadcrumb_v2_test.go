package pr

// Test plan for the v2 record rules in breadcrumb.go (forgectl#299 Task 1)
//
// validateBreadcrumbRecord, v2 (Classification: hostile-input validator)
//   [x] A version:2 record needs phase and revision; each absence refuses
//   [x] An unknown phase refuses; an unknown version refuses
//   [x] active requires windowId in the newDispatch spelling (tmux.FieldSep)
//   [x] needs-repair requires repairReason; any other phase forbids it
//   [x] queued and preparing allow an empty workspace; prepared does not
//   [x] A legacy record (no version) still loads and lists unchanged
//   [x] A queued record with no workspace loads and lists with phase queued
// List (Classification: survey verb, surfaces what it cannot read)
//   [x] One undecodable file yields unreadable=1 and the readable rows

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

func v2Base() Breadcrumb {
	return Breadcrumb{
		Workspace: "/tmp/forgectl-workflow-x", Ref: "o/r#1", Agent: "claude",
		CreatedAt: fixedTime(), Version: 2, Phase: PhasePrepared, Revision: 1,
	}
}

func TestValidateBreadcrumbRecord_V2Rules(t *testing.T) {
	goodWindow := strings.Join([]string{"123", "456", "@7"}, tmux.FieldSep)
	cases := []struct {
		name    string
		mut     func(*Breadcrumb)
		wantErr string // "" means valid
	}{
		{"v2 baseline is valid", func(*Breadcrumb) {}, ""},
		{"missing phase", func(b *Breadcrumb) { b.Phase = "" }, "phase"},
		{"missing revision", func(b *Breadcrumb) { b.Revision = 0 }, "revision"},
		{"unknown phase", func(b *Breadcrumb) { b.Phase = "flying" }, "phase"},
		{"unknown version", func(b *Breadcrumb) { b.Version = 3 }, "version"},
		{"active without windowId", func(b *Breadcrumb) { b.Phase = PhaseActive }, "windowId"},
		{"active with bare @N", func(b *Breadcrumb) { b.Phase = PhaseActive; b.WindowID = "@7" }, "windowId"},
		{"active with colon spelling", func(b *Breadcrumb) { b.Phase = PhaseActive; b.WindowID = "123:456:@7" }, "windowId"},
		{"active with FieldSep spelling", func(b *Breadcrumb) { b.Phase = PhaseActive; b.WindowID = goodWindow }, ""},
		{"windowId on a non-active phase", func(b *Breadcrumb) { b.WindowID = goodWindow }, "windowId"},
		{"needs-repair without reason", func(b *Breadcrumb) { b.Phase = PhaseNeedsRepair }, "repairReason"},
		{"needs-repair with reason", func(b *Breadcrumb) { b.Phase = PhaseNeedsRepair; b.RepairReason = "launch failed: x" }, ""},
		{"reason on prepared", func(b *Breadcrumb) { b.RepairReason = "x" }, "repairReason"},
		{"queued with empty workspace", func(b *Breadcrumb) { b.Phase = PhaseQueued; b.Workspace = "" }, ""},
		{"preparing with empty workspace", func(b *Breadcrumb) { b.Phase = PhasePreparing; b.Workspace = "" }, ""},
		{"prepared with empty workspace", func(b *Breadcrumb) { b.Workspace = "" }, "workspace"},
		{"legacy shape still valid", func(b *Breadcrumb) { b.Version = 0; b.Phase = ""; b.Revision = 0 }, ""},
		{"legacy with a phase smuggled in", func(b *Breadcrumb) { b.Version = 0; b.Revision = 0 }, "version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := v2Base()
			tc.mut(&bc)
			err := validateBreadcrumbRecord(bc)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestList_LegacyAndQueuedRecordsBothList(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	legacyRef := Ref{Owner: "o", Repo: "r", Number: 1}
	seedSession(t, c, legacyRef, time.Now().UTC().Add(-time.Minute))

	queuedRef := Ref{Owner: "o", Repo: "r", Number: 2}
	queued := Breadcrumb{Ref: queuedRef.String(), Agent: "claude", CreatedAt: time.Now().UTC(),
		Version: 2, Phase: PhaseQueued, Revision: 1}
	if _, err := writeBreadcrumb(c.SessionsDir(), queuedRef, queued); err != nil {
		t.Fatalf("seed queued: %v", err)
	}

	rows, unreadable, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d, want 0", unreadable)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Newest first: the queued record.
	if got := rows[0].Phase(); got != PhaseQueued {
		t.Errorf("rows[0].Phase() = %q, want queued", got)
	}
	if !rows[0].IsWorkspaceNone() {
		t.Error("queued row should classify as workspace none")
	}
	if got := rows[1].Phase(); got != "" {
		t.Errorf("legacy Phase() = %q, want empty", got)
	}
	if !rows[1].IsWorkspaceLive() {
		t.Error("legacy row with a real workspace should classify live")
	}
}

func TestList_SurfacesUnreadableRecords(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())
	// A record a future build wrote: one key this build does not know.
	bad := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#9","createdAt":"2026-09-11T00:00:00Z","futureKey":true}` + "\n")
	if err := os.WriteFile(filepath.Join(c.SessionsDir(), "o-r-9-1.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, unreadable, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", unreadable)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want the one readable row", len(rows))
	}
}
