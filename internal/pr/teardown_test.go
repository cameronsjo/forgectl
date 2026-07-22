package pr

// Test plan for teardown.go
//
// Teardown (Classification: hostile-input exact-match membership)
//   [x] Accepts a genuine breadcrumb (exact member): restores, removes
//       workspace, kills window, deletes breadcrumb
//   [x] REJECTS a non-member path — with ZERO Runner calls (no git/tmux runs
//       against an attacker-supplied path)
//   [x] REJECTS a glob-ish / prefix path (membership is exact, not a glob)
//   [x] Restore round-trips a quarantined file without error
// Cleanup (Classification: date-wide discard)
//   [x] Discards only sessions matching the given date

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// seedSession writes a real workspace + breadcrumb and returns the breadcrumb
// path, so teardown has a genuine member to act on.
func seedSession(t *testing.T, c *Client, ref Ref, createdAt time.Time) (bcPath, workspace string) {
	t.Helper()
	workspace = fakeWorkspace(t)
	bc := Breadcrumb{Workspace: workspace, Ref: ref.String(), Agent: "claude", CreatedAt: createdAt}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}
	return path, workspace
}

func TestTeardown_AcceptsMember(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	path, ws := seedSession(t, c, ref, time.Now().UTC())

	// A real quarantined file so Restore has something to rename back.
	if err := os.WriteFile(filepath.Join(ws, "CLAUDE.md.quarantined"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed quarantined file: %v", err)
	}

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown member: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("breadcrumb should be removed after teardown")
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Error("workspace should be removed after teardown")
	}
	tmux, ok := findCall(fake.Calls, "tmux")
	if !ok {
		t.Fatalf("expected a tmux kill-window call; got %+v", fake.Calls)
	}
	if want := []string{"kill-window", "-t", "forgectl:pr-o-r-7"}; !equalArgs(tmux.Args, want) {
		t.Errorf("tmux args = %v, want %v", tmux.Args, want)
	}
}

func TestTeardown_RejectsNonMember(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	// Seed one real session so the dir is non-empty, then target a different path.
	seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 7}, time.Now().UTC())

	outside := filepath.Join(t.TempDir(), "attacker.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.Teardown(context.Background(), outside); err == nil {
		t.Error("expected teardown to reject a non-member path")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a rejected teardown must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

func TestTeardown_RejectsGlob(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 7}, time.Now().UTC())

	glob := filepath.Join(c.SessionsDir(), "*.json")
	if err := c.Teardown(context.Background(), glob); err == nil {
		t.Error("membership is exact-match, not a glob; expected rejection")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a rejected teardown must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

func TestCleanup_DateScoped(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)

	today := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	other := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	pToday, _ := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, today)
	pOther, _ := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, other)

	if err := c.Cleanup(context.Background(), "2026-07-08"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(pToday); !os.IsNotExist(err) {
		t.Error("today's session should be cleaned up")
	}
	if _, err := os.Stat(pOther); err != nil {
		t.Error("other day's session should be untouched")
	}
}
