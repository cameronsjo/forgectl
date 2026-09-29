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
	"bytes"
	"context"
	"errors"
	"io/fs"
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
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	fake := reviewServer(mustWindowName(t, ref))
	c := testClient(t, fake)
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
	call, ok := findCallVerb(fake.Calls, "tmux", "kill-window")
	if !ok {
		t.Fatalf("expected a tmux kill-window call; got %+v", fake.Calls)
	}
	// The native window id, resolved under the review session and revalidated
	// immediately before the kill.
	if want := []string{"kill-window", "-t", "@5"}; !equalArgs(call.Args, want) {
		t.Errorf("tmux args = %v, want %v", call.Args, want)
	}
}

// TestTeardown_LeavesForeignWindowAlone is the destructive gate on the teardown
// path: a window carrying this review's NAME but sitting in another session is
// not this review's window, and teardown must leave it running.
func TestTeardown_LeavesForeignWindowAlone(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	sessionRow := strings.Join([]string{"123", "456", "$1", "forgectl", "1", "0", "1700000000", "/w"}, "\x1f")
	strayRow := strings.Join([]string{"123", "456", "@9", "$2", "other", "0", mustWindowName(t, ref), "0", "1"}, "\x1f")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return sessionRow, nil
		case "list-windows":
			return strayRow, nil
		}
		return "", nil
	}}
	c := testClient(t, fake)
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if call, ok := findCallVerb(fake.Calls, "tmux", "kill-window"); ok {
		t.Errorf("kill-window ran with %v against a window in another session", call.Args)
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
	ref := Ref{Owner: "o", Repo: "r", Number: 11}
	server := reviewServer(mustWindowName(t, ref))
	inner := server.RunFunc
	fake := &exec.FakeRunner{}
	fake.RunFunc = func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "kill-window" {
			_, err := os.Stat(bcPath)
			breadcrumbAtTmux = err == nil
		}
		return inner(name, args)
	}
	c := testClient(t, fake)
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
// fallback arm. That half is no longer pinned by construction alone — the
// sandboxTeardown seam this change added stages a real mid-teardown failure,
// and TestTeardown_LiveFailureNeverEntersTheStaleUnlink asserts the error
// surfaces with both breadcrumb and workspace intact. This test keeps the
// cheaper, injection-free half: the two branches' Runner signatures.
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

// auditRows reads the audit trail the way an operator would, failing the test
// rather than the caller when the log itself cannot be read.
func auditRows(t *testing.T, c *Client) []RepairRow {
	t.Helper()
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	return rows
}

// TestTeardown_WritesIntentAndCompletion is the headline: `pr teardown` removes
// a clean room, so it owes the same write-ahead pair `pr repair --rollback`
// writes. Without it, the one row that names a clean room mid-delete does not
// exist for the verb operators reach for most.
func TestTeardown_WritesIntentAndCompletion(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 21}
	c := testClient(t, &exec.FakeRunner{})
	path, ws := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + completion: %+v", len(rows), rows)
	}
	if rows[0].ID != rows[1].ID || rows[0].ID == "" {
		t.Errorf("rows are not paired by id: %q and %q", rows[0].ID, rows[1].ID)
	}
	if rows[0].Outcome != repairOutcomeIntent || rows[1].Outcome != repairOutcomeApplied {
		t.Errorf("outcomes = %q then %q, want %q then %q",
			rows[0].Outcome, rows[1].Outcome, repairOutcomeIntent, repairOutcomeApplied)
	}
	for i, r := range rows {
		if r.Verb != auditVerbTeardown {
			t.Errorf("row %d verb = %q, want %q", i, r.Verb, auditVerbTeardown)
		}
		// Mode is repair's flag spelling. A teardown has no flag, and filling it
		// in would make the trail claim a repair ran.
		if r.Mode != "" {
			t.Errorf("row %d mode = %q, want empty on a teardown row", i, r.Mode)
		}
		if r.Workspace != ws {
			t.Errorf("row %d workspace = %q, want %q — the only pointer left to the clean room", i, r.Workspace, ws)
		}
		if r.RecordPath != path {
			t.Errorf("row %d record path = %q, want %q", i, r.RecordPath, path)
		}
	}
}

// TestTeardown_StaleAndRecordOnlyAlsoWriteRows covers the two arms that delete
// a record without touching a workspace. They remove less, but they remove the
// only thing naming a review, so their removal is just as much a trail event.
func TestTeardown_StaleAndRecordOnlyAlsoWriteRows(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, c *Client, ref Ref) (path, workspace string)
	}{
		{"stale workspace", func(t *testing.T, c *Client, ref Ref) (string, string) {
			t.Helper()
			return seedStaleSession(t, c, ref, time.Now().UTC())
		}},
		{"record only", func(t *testing.T, c *Client, ref Ref) (string, string) {
			t.Helper()
			return seedPhaseRecord(t, c, ref, PhaseQueued, ""), ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := Ref{Owner: "o", Repo: "r", Number: 22}
			c := testClient(t, &exec.FakeRunner{})
			path, ws := tc.seed(t, c, ref)

			if err := c.Teardown(context.Background(), path); err != nil {
				t.Fatalf("Teardown: %v", err)
			}
			rows := auditRows(t, c)
			if len(rows) != 2 {
				t.Fatalf("audit rows = %d, want intent + completion: %+v", len(rows), rows)
			}
			if rows[0].Outcome != repairOutcomeIntent || rows[1].Outcome != repairOutcomeApplied {
				t.Errorf("outcomes = %q then %q, want intent then applied", rows[0].Outcome, rows[1].Outcome)
			}
			for i, r := range rows {
				if r.Verb != auditVerbTeardown {
					t.Errorf("row %d verb = %q, want %q", i, r.Verb, auditVerbTeardown)
				}
				if r.Workspace != ws {
					t.Errorf("row %d workspace = %q, want %q", i, r.Workspace, ws)
				}
			}
		})
	}
}

// TestTeardown_QueuedRecordRemovesFileCallsNoSandboxTeardown is Task 3's own
// positive control on the #472 spine: `pr teardown` on a `queued` record
// (no workspace, by construction — reserve.go/session.go's Queue) must land
// through discardRecordOnly, never through the live/stale sandbox path. The
// seam is overridden to FAIL, so a stray call surfaces as a test failure
// rather than a silent no-op success.
func TestTeardown_QueuedRecordRemovesFileCallsNoSandboxTeardown(t *testing.T) {
	orig := sandboxTeardown
	sandboxTeardown = func(context.Context, exec.Runner, string) error {
		t.Fatal("sandbox teardown must never run for a queued (workspace-less) record")
		return nil
	}
	t.Cleanup(func() { sandboxTeardown = orig })

	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 99}
	path := seedPhaseRecord(t, c, ref, PhaseQueued, "")

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("breadcrumb %s should be removed; stat err = %v", path, err)
	}
	for _, call := range c.run.(*exec.FakeRunner).Calls {
		if call.Name == "git" || call.Name == "tmux" {
			t.Errorf("a queued teardown must touch no workspace or window; saw %s %v", call.Name, call.Args)
		}
	}
}

// TestTeardown_RefusalWritesNoRow is the ordering rule the repair arms already
// hold to: a dangling intent means a delete died partway, so a refusal that
// wrote one would forge that signal and send someone hunting a directory
// nothing ever touched.
func TestTeardown_RefusalWritesNoRow(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 23}, time.Now().UTC())

	outside := filepath.Join(t.TempDir(), "attacker.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, operand := range []string{outside, filepath.Join(c.SessionsDir(), "*.json")} {
		if err := c.Teardown(context.Background(), operand); err == nil {
			t.Errorf("expected teardown to refuse %q", operand)
		}
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %d, want none: a refusal changed nothing to record: %+v", len(rows), rows)
	}
}

// TestTeardown_InvalidWorkspaceRefusalWritesNoRow covers the refusal that lives
// past classification — the default arm — rather than in membership, which is
// where an intent row written too early would land.
func TestTeardown_InvalidWorkspaceRefusalWritesNoRow(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	path := seedInvalidSession(t, c, Ref{Owner: "o", Repo: "r", Number: 24}, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err == nil {
		t.Fatal("a workspace that is neither live nor cleanly absent must be refused")
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %d, want none: %+v", len(rows), rows)
	}
}

// TestTeardown_DriftRefusalCompletesTheRowAsFailed pins the other side of the
// ordering rule. Drift is caught INSIDE the discard arms, after the intent row
// is on disk, so it completes the pair as failed rather than leaving a dangling
// intent that would read as a delete that died mid-way.
func TestTeardown_DriftRefusalCompletesTheRowAsFailed(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	path, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 25}, time.Now().UTC())

	orig := staleMemberIsRegular
	staleMemberIsRegular = func(fs.FileInfo) bool { return false }
	t.Cleanup(func() { staleMemberIsRegular = orig })

	if err := c.Teardown(context.Background(), path); err == nil {
		t.Fatal("observed drift must refuse the removal")
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + a failed completion: %+v", len(rows), rows)
	}
	if rows[1].Outcome != repairOutcomeFailed {
		t.Errorf("completion outcome = %q, want %q", rows[1].Outcome, repairOutcomeFailed)
	}
	if rows[1].Error == "" {
		t.Error("a failed completion with no error says nothing about why")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a refused teardown removed the record: %v", err)
	}
}

// TestCleanup_WritesOneRowPairPerRecord: a sweep is N removals, not one, and a
// trail that collapsed them would name only the last clean room.
func TestCleanup_WritesOneRowPairPerRecord(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	day := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	other := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 31}, day)
	seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 32}, day)
	pOther, _ := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 33}, other)

	if err := c.Cleanup(context.Background(), "2026-07-08"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(pOther); err != nil {
		t.Errorf("cleanup swept a record outside the date: %v", err)
	}
	rows := auditRows(t, c)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %d, want one pair per swept record: %+v", len(rows), rows)
	}
	ids := map[string]int{}
	for i, r := range rows {
		if r.Verb != auditVerbCleanup {
			t.Errorf("row %d verb = %q, want %q", i, r.Verb, auditVerbCleanup)
		}
		ids[r.ID]++
	}
	if len(ids) != 2 {
		t.Errorf("distinct row ids = %d, want one per swept record: %+v", len(ids), rows)
	}
	for id, n := range ids {
		if n != 2 {
			t.Errorf("row id %s appears %d times, want an intent and a completion", id, n)
		}
	}
}

// TestRepairRollback_WritesExactlyOnePair is the nested-pair regression pin.
// repairRollbackLocked writes its own intent row and then calls the UNAUDITED
// teardown core; auditing that core instead of the two outer verbs would give
// this one mutation two intents and two completions, which is exactly the shape
// that means "a delete died mid-way" to anyone reading the trail.
func TestRepairRollback_WritesExactlyOnePair(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 41}
	c := repairClient(t, repairRunner(nil))
	ws := fakeWorkspace(t)
	if err := os.RemoveAll(ws); err != nil {
		t.Fatalf("stale the workspace: %v", err)
	}
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	if _, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want exactly one intent and one completion: %+v", len(rows), rows)
	}
	for i, r := range rows {
		if r.Verb != auditVerbRepair {
			t.Errorf("row %d verb = %q, want %q — the repair owns this mutation", i, r.Verb, auditVerbRepair)
		}
		if r.Mode != RepairModeRollback {
			t.Errorf("row %d mode = %q, want %q", i, r.Mode, RepairModeRollback)
		}
	}
}

// TestTeardown_AuditLogIsNotEnumerated: the trail lives beside the records it
// describes, so the enumerations must keep ignoring it. A .jsonl admitted as a
// record would block every launch as unreadable.
func TestTeardown_AuditLogIsNotEnumerated(t *testing.T) {
	c := repairClient(t, repairRunner(nil))
	path, _ := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 51}, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.SessionsDir(), repairLogName)); err != nil {
		t.Fatalf("setup: the teardown wrote no audit log to enumerate past: %v", err)
	}
	summaries, unreadable, err := c.listLocked()
	if err != nil {
		t.Fatalf("listLocked: %v", err)
	}
	if len(unreadable) != 0 {
		t.Errorf("the audit log was admitted as an unreadable record: %+v", unreadable)
	}
	for _, sum := range summaries {
		if filepath.Base(sum.Path()) == repairLogName {
			t.Errorf("the audit log was listed as a session record: %s", sum.Path())
		}
	}
	if _, err := c.reserve(context.Background(), Ref{Owner: "o", Repo: "r", Number: 52}, 4, PrepareOpts{Agent: "claude"}); err != nil {
		t.Errorf("reserve refuses with the audit log present: %v", err)
	}
}

// hangingTmux is a Runner whose tmux calls block until their context is done,
// the way a wedged tmux server does. blockVerb picks which subcommand hangs;
// "" hangs every one. Everything else delegates to the wrapped fake.
type hangingTmux struct {
	*exec.FakeRunner
	blockVerb string
}

func (h *hangingTmux) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "tmux" && len(args) > 0 && (h.blockVerb == "" || args[0] == h.blockVerb) {
		_, _ = h.FakeRunner.Run(ctx, name, args...) // only the call ledger matters
		<-ctx.Done()
		return "", ctx.Err()
	}
	return h.FakeRunner.Run(ctx, name, args...)
}

// assertParkedNotDiscarded checks the fail-closed outcome of a teardown whose
// tmux never answered: the error says so, the record is still there parked in
// needs-repair with the exact reason, the workspace is untouched, and the lock
// is free again.
func assertParkedNotDiscarded(t *testing.T, c *Client, path, ws string, err error) {
	t.Helper()
	if !errors.Is(err, ErrWindowKillTimedOut) {
		t.Fatalf("Teardown err = %v, want ErrWindowKillTimedOut", err)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("the workspace must be kept while the window state is unknown: %v", serr)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseNeedsRepair || bc.RepairReason != "window kill timed out (tmux unresponsive)" {
		t.Errorf("record = phase %q reason %q, want needs-repair with the timeout reason", bc.Phase, bc.RepairReason)
	}
	start := time.Now()
	if lerr := c.withLifecycleLock(context.Background(), "probe", func() error { return nil }); lerr != nil {
		t.Fatalf("lifecycle lock still held after teardown: %v", lerr)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("re-acquiring the lock took %v", d)
	}
}

// TestTeardown_HungTmuxParksTheRecordAndReleasesTheLock is forgectl#556. The
// tmux kill stays under the lifecycle lock, so a wedged tmux must be cut off by
// teardownTmuxBudget, and because the window's state is then unknown the
// teardown fails closed: it parks the record in needs-repair and removes
// nothing, rather than discarding a record whose window may still be live.
func TestTeardown_HungTmuxParksTheRecordAndReleasesTheLock(t *testing.T) {
	old := teardownTmuxBudget
	teardownTmuxBudget = 100 * time.Millisecond
	t.Cleanup(func() { teardownTmuxBudget = old })

	for _, tc := range []struct{ name, hang string }{
		{"resolution hangs", ""},
		{"only the kill hangs", "kill-window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := Ref{Owner: "o", Repo: "r", Number: 21}
			h := &hangingTmux{FakeRunner: reviewServer(mustWindowName(t, ref)), blockVerb: tc.hang}
			c := New(h, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
				WithApprover(func(string) (bool, error) { return false, nil }),
				WithTTYCheck(func() bool { return false }))
			ws := fakeWorkspace(t)
			path := seedPhaseRecord(t, c, ref, PhasePrepared, ws)

			done := make(chan error, 1)
			go func() { done <- c.Teardown(context.Background(), path) }()
			select {
			case err := <-done:
				assertParkedNotDiscarded(t, c, path, ws, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Teardown is still blocked on tmux; the lock would be held indefinitely")
			}
			if _, ok := findCallVerb(h.Calls, "tmux", firstNonEmpty(tc.hang, "list-sessions")); !ok {
				t.Errorf("the hanging tmux call was never issued; the test did not exercise the bound: %+v", h.Calls)
			}
		})
	}
}

// TestTeardown_HungTmuxOnALegacyRecordSaysItWasNotParked: a record with no
// version cannot be moved to needs-repair, so the timeout leaves it exactly as
// it was, and the error must say so (ErrRecordNotParked) rather than let a
// caller report a parked record that was never written.
func TestTeardown_HungTmuxOnALegacyRecordSaysItWasNotParked(t *testing.T) {
	old := teardownTmuxBudget
	teardownTmuxBudget = 100 * time.Millisecond
	t.Cleanup(func() { teardownTmuxBudget = old })
	ref := Ref{Owner: "o", Repo: "r", Number: 24}
	h := &hangingTmux{FakeRunner: reviewServer(mustWindowName(t, ref))}
	c := New(h, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	path, ws := seedSession(t, c, ref, time.Now().UTC()) // legacy: no version
	before, err := os.ReadFile(path)                     //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatal(err)
	}

	err = c.Teardown(context.Background(), path)
	if !errors.Is(err, ErrWindowKillTimedOut) || !errors.Is(err, ErrRecordNotParked) {
		t.Fatalf("err = %v, want ErrWindowKillTimedOut wrapping ErrRecordNotParked", err)
	}
	after, rerr := os.ReadFile(path) //nolint:gosec // test-owned temp dir
	if rerr != nil || !bytes.Equal(before, after) {
		t.Errorf("the legacy record must be left byte-identical: %v", rerr)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("workspace must be kept: %v", serr)
	}
}

// TestTeardown_RealTmuxGrandchildHoldingThePipesIsBounded reproduces the
// reviewer's case with a REAL exec.OSRunner: a `tmux` whose script forks a
// sleeping grandchild that inherits the output pipes. CommandContext's SIGKILL
// only reaches the script, so without exec's WaitDelay Wait blocks until the
// grandchild exits — long past the budget, with the lock held.
func TestTeardown_RealTmuxGrandchildHoldingThePipesIsBounded(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 6 &\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o700); err != nil { //nolint:gosec // test-owned script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := teardownTmuxBudget
	teardownTmuxBudget = 300 * time.Millisecond
	t.Cleanup(func() { teardownTmuxBudget = old })

	ref := Ref{Owner: "o", Repo: "r", Number: 23}
	c := New(exec.OSRunner{}, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhasePrepared, ws)

	start := time.Now()
	err := c.Teardown(context.Background(), path)
	// budget + WaitDelay (500 ms) + slack; the grandchild sleeps 6 s.
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Teardown took %v: a grandchild holding the pipes defeated the tmux budget", d)
	}
	assertParkedNotDiscarded(t, c, path, ws, err)
}

// TestTeardown_KillWindowRunsUnderTheLifecycleLock pins the #556 decision to
// keep the kill under the lock rather than release it first: with the lock
// released, a new admission of the same ref could create a same-named window
// that the kill would then take out. From inside the kill, a second client
// must find the lock busy.
func TestTeardown_KillWindowRunsUnderTheLifecycleLock(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 22}
	server := reviewServer(mustWindowName(t, ref))
	inner := server.RunFunc
	dir := t.TempDir()
	var other *Client
	var probeErr error
	probed := false
	fake := &exec.FakeRunner{}
	fake.RunFunc = func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "kill-window" {
			probed = true
			probeErr = other.withLifecycleLock(context.Background(), "probe", func() error { return nil })
		}
		return inner(name, args)
	}
	opts := []Option{WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false })}
	c := New(fake, opts...)
	other = New(&exec.FakeRunner{}, append(opts, WithLockWait(50*time.Millisecond))...)
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !probed {
		t.Fatal("kill-window never ran; the test did not exercise the lock")
	}
	var busy *lockBusyError
	if !errors.As(probeErr, &busy) {
		t.Errorf("a second client acquired the lock during the kill (err = %v); the kill has left the lock", probeErr)
	}
}
