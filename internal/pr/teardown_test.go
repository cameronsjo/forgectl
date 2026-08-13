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
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/quarantine"
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
	if want := []string{"kill-window", "-t", "=forgectl:pr-o-r-7"}; !equalArgs(tmux.Args, want) {
		t.Errorf("tmux args = %v, want %v", tmux.Args, want)
	}
}

func TestTeardown_CoveredRootQuarantineHasNoPhantomNestedMove(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 8}
	path, ws := seedSession(t, c, ref, time.Now().UTC())
	externalCanary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(externalCanary, []byte("survives"), 0o600); err != nil {
		t.Fatalf("seed external canary: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".claude.quarantined"), 0o700); err != nil {
		t.Fatalf("seed covered root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude.quarantined", "CLAUDE.md"), []byte("covered"), 0o600); err != nil {
		t.Fatalf("seed nested carrier: %v", err)
	}
	targets, err := quarantine.ExpandTargets(ws, quarantine.SuffixQuarantined, quarantine.DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets covered root: %v", err)
	}
	moves, err := quarantine.ComputeMoves(ws, quarantine.SuffixQuarantined, targets)
	if err != nil {
		t.Fatalf("ComputeMoves covered root: %v", err)
	}
	coveredOriginal := filepath.Join(ws, ".claude")
	for _, move := range moves {
		rel, relErr := filepath.Rel(coveredOriginal, move.From)
		if relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("covered root produced phantom nested move: %+v", moves)
		}
	}

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown covered root: %v", err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace should be removed after covered-root restore: %v", err)
	}
	content, err := os.ReadFile(externalCanary)
	if err != nil || string(content) != "survives" {
		t.Fatalf("external sibling canary changed: content=%q err=%v", content, err)
	}
}

// TestTeardown_LiveOrderRemovesBreadcrumbLast pins the live branch's ordering
// against the stale branch's: the breadcrumb is the LAST thing to go, still on
// disk when the tmux kill runs. If it were unlinked earlier, a failure partway
// through would leave a torn-down workspace with no record of it — the inverse
// of the leak #212 fixes, and worse, because nothing would point at it.
func TestTeardown_LiveOrderRemovesBreadcrumbLast(t *testing.T) {
	var breadcrumbAtTmux bool
	var bcPath string
	fake := &exec.FakeRunner{}
	fake.RunFunc = func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "kill-window" {
			_, err := os.Stat(bcPath)
			breadcrumbAtTmux = err == nil
		}
		return "", nil
	}
	c := testClient(t, fake)
	ref := Ref{Owner: "o", Repo: "r", Number: 11}
	path, ws := seedSession(t, c, ref, time.Now().UTC())
	bcPath = path

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !breadcrumbAtTmux {
		t.Error("the breadcrumb must still exist when the tmux kill runs — it is removed last")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("breadcrumb should be gone once the live teardown completes")
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Error("workspace should be removed by the live branch")
	}
}

// TestTeardown_BranchesAreDisjoint pins the branch boundary by its observable
// signature. The live branch necessarily reaches the Runner (the tmux kill);
// the stale branch necessarily does not, and must not, because everything the
// Runner does there would act on a workspace that is gone. So a nonempty
// Runner ledger for a stale record — or an empty one for a live record — means
// the branches have merged.
//
// The stale branch is also structurally unreachable after a live failure:
// Teardown's classification switch returns discard's error directly and has no
// fallback arm. There is no portable way to force a mid-teardown failure
// without a chmod assumption (quarantine.Restore treats an occupied
// destination as non-fatal by design, and sandbox.Teardown's only failure is
// an os.RemoveAll error), so that half is pinned by construction rather than
// by a contrived fault injection.
func TestTeardown_BranchesAreDisjoint(t *testing.T) {
	liveFake := &exec.FakeRunner{}
	liveClient := testClient(t, liveFake)
	livePath, _ := seedSession(t, liveClient, Ref{Owner: "o", Repo: "r", Number: 12}, time.Now().UTC())
	if err := liveClient.Teardown(context.Background(), livePath); err != nil {
		t.Fatalf("live Teardown: %v", err)
	}
	if _, ok := findCall(liveFake.Calls, "tmux"); !ok {
		t.Errorf("the live branch must reach tmux; got %+v", liveFake.Calls)
	}

	staleFake := &exec.FakeRunner{}
	staleClient := testClient(t, staleFake)
	stalePath, _ := seedStaleSession(t, staleClient, Ref{Owner: "o", Repo: "r", Number: 13}, time.Now().UTC())
	if err := staleClient.Teardown(context.Background(), stalePath); err != nil {
		t.Fatalf("stale Teardown: %v", err)
	}
	if len(staleFake.Calls) != 0 {
		t.Errorf("the stale branch must never reach the Runner; got %+v", staleFake.Calls)
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
