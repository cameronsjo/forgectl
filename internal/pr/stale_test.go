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
//   [x] Cleanup continues past a failing candidate and reports the FIRST error

import (
	"context"
	"errors"
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
// contract on a realistic day: an invalid record on that date is left behind
// rather than guessed at, another date is untouched, and — the half this test
// used to only claim — a failing candidate neither aborts the sweep nor loses
// its error.
//
// Two of the day's candidates are staged to FAIL, through the sandboxTeardown
// seam. List sorts newest-first, so the seeded times fix the order: the first
// failure, then a stale record that must still be removed (the `continue`
// arm), then a second, different failure that must NOT displace the first
// (the firstErr retention). Without a staged failure both of those arms went
// uncovered and the assertion below checked a nil error.
func TestCleanup_MixedStatesContinuesAndReportsFirstError(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)

	day := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	middle := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)
	last := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	other := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	firstPath, firstWS := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, day)
	stalePath, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, middle)
	lastPath, lastWS := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 5}, last)
	invalidPath := seedInvalidSession(t, c, Ref{Owner: "o", Repo: "r", Number: 3}, day)
	otherDayPath, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 4}, other)

	errFirst := errors.New("permission denied removing the first workspace")
	errLast := errors.New("permission denied removing the last workspace")
	restore := sandboxTeardown
	sandboxTeardown = func(ctx context.Context, r exec.Runner, ws string) error {
		switch ws {
		case firstWS:
			return errFirst
		case lastWS:
			return errLast
		}
		return restore(ctx, r, ws)
	}
	t.Cleanup(func() { sandboxTeardown = restore })

	// An invalid record is skipped by List, so the sweep never selects it and
	// returns no error for it — it is left visible rather than deleted blind.
	err := c.Cleanup(context.Background(), "2026-07-08")
	if !errors.Is(err, errFirst) {
		t.Fatalf("Cleanup must report the FIRST failure, got %v", err)
	}
	if errors.Is(err, errLast) {
		t.Errorf("a later failure must not displace the first: %v", err)
	}

	// The sweep continued past the first failure: the stale record between the
	// two failures is gone, and the second failing candidate was still tried.
	if _, statErr := os.Stat(stalePath); !os.IsNotExist(statErr) {
		t.Errorf("the sweep must continue past a failure and remove the stale record; Stat err = %v", statErr)
	}
	for name, path := range map[string]string{"first": firstPath, "last": lastPath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("the %s breadcrumb must survive its failed teardown: %v", name, statErr)
		}
	}
	for name, ws := range map[string]string{"first": firstWS, "last": lastWS} {
		if _, statErr := os.Stat(ws); statErr != nil {
			t.Errorf("the %s workspace must survive its failed teardown: %v", name, statErr)
		}
	}
	if _, statErr := os.Stat(invalidPath); statErr != nil {
		t.Errorf("an unclassifiable record must be left in place, not deleted on a guess: %v", statErr)
	}
	if _, statErr := os.Stat(otherDayPath); statErr != nil {
		t.Errorf("another day's record must be untouched: %v", statErr)
	}
}

// TestCleanup_AllSucceedingCandidatesAreRemoved keeps the plain happy path the
// mixed-states test above no longer covers now that it stages failures: a live
// record and a stale record from the same date both go, with no error.
func TestCleanup_AllSucceedingCandidatesAreRemoved(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)

	day := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	livePath, liveWS := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, day)
	stalePath, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, day)

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
