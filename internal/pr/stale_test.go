package pr

// End-to-end reproduction for #212 — the permanent regression test.
//
// The defect: a breadcrumb whose recorded workspace has been deleted was
// unreachable by every verb. loadSession ran validateWorkspace, whose Stat
// failed, so List() hid the row, Teardown() refused it, and Cleanup() never
// selected it. The breadcrumb then survived forever with no supported way to
// remove it.
//
// The unit-level halves live in workspace_state_test.go (classification),
// summary_test.go (visibility), and teardown_test.go (the unlink protocol).
// These two tests exist so the USER-VISIBLE symptom stays fixed even if that
// internal structure is reorganized later.
//
//   [x] A stale breadcrumb is tearable down, removing ONLY the breadcrumb
//   [x] A stale teardown issues ZERO Runner calls
//   [x] Cleanup selects a stale breadcrumb by its recorded date

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestTeardown_StaleBreadcrumbIsRemovable(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	path, ws := seedStaleSession(t, c, ref, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown of a stale breadcrumb: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("breadcrumb %q should be removed after a stale teardown; Stat err = %v", path, err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("workspace %q must stay absent; a stale teardown creates nothing", ws)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a stale teardown must issue ZERO Runner calls (no git, no tmux); got %+v", fake.Calls)
	}
}

// TestCleanup_MixedStatesContinuesAndReportsFirstError pins the sweep's full
// contract on a realistic day: a live record and a stale record from the same
// date both go, an invalid record on that date is left behind rather than
// guessed at, another date is untouched, and one failure does not abort the
// rest.
func TestCleanup_MixedStatesContinuesAndReportsFirstError(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)

	day := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	other := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	livePath, liveWS := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, day)
	stalePath, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, day)
	invalidPath := seedInvalidSession(t, c, Ref{Owner: "o", Repo: "r", Number: 3}, day)
	otherDayPath, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 4}, other)

	// An invalid record is skipped by List, so the sweep never selects it and
	// returns no error for it — it is left visible rather than deleted blind.
	if err := c.Cleanup(context.Background(), "2026-07-08"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	for name, path := range map[string]string{"live": livePath, "stale": stalePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the %s breadcrumb dated 2026-07-08 should be cleaned up; Stat err = %v", name, err)
		}
	}
	if _, err := os.Stat(liveWS); !os.IsNotExist(err) {
		t.Error("the live session's workspace should be removed")
	}
	if _, err := os.Stat(invalidPath); err != nil {
		t.Errorf("an unclassifiable record must be left in place, not deleted on a guess: %v", err)
	}
	if _, err := os.Stat(otherDayPath); err != nil {
		t.Errorf("another day's record must be untouched: %v", err)
	}
}

// TestCleanup_SelectsStaleBreadcrumbs pins the date sweep: a stale record is
// selected by its recorded CreatedAt exactly like a live one, and a record
// from another day is still left alone.
func TestCleanup_SelectsStaleBreadcrumbs(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)

	day := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	other := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	stale, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, day)
	keep, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, other)

	if err := c.Cleanup(context.Background(), "2026-07-08"); err != nil {
		t.Fatalf("Cleanup with a stale breadcrumb: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale breadcrumb dated 2026-07-08 should be cleaned up; Stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a stale breadcrumb from another day must be untouched: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a stale cleanup must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}
