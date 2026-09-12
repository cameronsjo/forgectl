package pr

// Test plan for prune.go (forgectl#503)
//
// parseAsideAge (Classification: pure, name-derived age)
//   [x] The two names freeAsideName can produce, and every near-miss
//   [x] An unparseable name yields no time, so the caller lists and keeps it
// parseRetention (Classification: pure, operator-supplied window)
//   [x] <N>d and every time.ParseDuration form; zero and negative refuse
// classifyRepairRows (Classification: pure, what compaction may drop)
//   [x] A settled pair past the cutoff drops
//   [x] An UNPAIRED intent is kept at any age — it is the dangling signal
//   [x] An unparseable line is kept — nothing may drop what it cannot read
//   [x] A zero-TS row is kept — no age could be established
//   [x] An in-window pair is kept
// Prune (Classification: destructive sweep; refuses on uncertainty)
//   [x] A young set-aside file is listed, never removed
//   [x] A live window refuses THAT item; the file stays on disk, exit 0
//   [x] An unreadable window list refuses ref-bearing items only
//   [x] The intent row precedes the unlink and carries the record's bytes
//   [x] A failed intent row removes nothing
//   [x] A byte mismatch against the pinned re-read refuses
//   [x] A compaction rename failure leaves the old log intact and readable
//   [x] Compaction's own intent row survives into the new file
//   [x] Nothing removable exits 0 off a TTY without --yes
//   [x] --dry-run off a TTY without --yes touches nothing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestParseAsideAge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantOK  bool
		wantSec int64
	}{
		{"plain timestamp", "o-r-1-1.json" + unreadableSuffix + "1757000000", true, 1757000000},
		{"timestamp with the collision suffix", "o-r-1-1.json" + unreadableSuffix + "1757000000-a1b2c3d4e5f60718", true, 1757000000},
		{"an ordinary record", "o-r-1-1.json", false, 0},
		{"the prefix does not end in .json", "o-r-1-1" + unreadableSuffix + "123", false, 0},
		{"no timestamp at all", "o-r-1-1.json" + unreadableSuffix, false, 0},
		{"a non-numeric timestamp", "o-r-1-1.json" + unreadableSuffix + "abc", false, 0},
		{"an overflowing timestamp", "o-r-1-1.json" + unreadableSuffix + "99999999999999999999", false, 0},
		{"a short collision suffix", "o-r-1-1.json" + unreadableSuffix + "1757000000-a1b2", false, 0},
		{"a non-hex collision suffix", "o-r-1-1.json" + unreadableSuffix + "1757000000-zzzzzzzzzzzzzzzz", false, 0},
		{"a negative timestamp", "o-r-1-1.json" + unreadableSuffix + "-1757000000", false, 0},
		{"the log itself", repairLogName, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAsideAge(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("parseAsideAge(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.Unix() != tc.wantSec {
				t.Errorf("parseAsideAge(%q) = %d, want %d", tc.input, got.Unix(), tc.wantSec)
			}
		})
	}
}

func TestParseRetention(t *testing.T) {
	for _, tc := range []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"0", 0, true},
		{"0d", 0, true},
		{"-1d", 0, true},
		{"-1h", 0, true},
		{"d", 0, true},
		{"", 0, true},
		{"30days", 0, true},
		{"1.5d", 0, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseRetention(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRetention(%q) = %v, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRetention(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseRetention(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// rowLine renders one audit line the way the log holds it.
func rowLine(t *testing.T, id, outcome string, ts time.Time) []byte {
	t.Helper()
	data, err := marshalRepairRow(RepairRow{
		TS: ts, ID: id, Mode: RepairModePrune, Outcome: outcome, RecordPath: "/tmp/x.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(data, []byte("\n"))
}

func TestClassifyRepairRows(t *testing.T) {
	now := fixedTime()
	cutoff := now.Add(-24 * time.Hour)
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)

	t.Run("a settled pair past the cutoff drops", func(t *testing.T) {
		lines := [][]byte{
			rowLine(t, "aaa", repairOutcomeIntent, old),
			rowLine(t, "aaa", repairOutcomeApplied, old),
		}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 2 || len(keep) != 0 {
			t.Fatalf("dropped = %d, kept = %d, want the whole settled pair dropped", dropped, len(keep))
		}
	})

	t.Run("a failed completion settles the pair too", func(t *testing.T) {
		lines := [][]byte{
			rowLine(t, "bbb", repairOutcomeIntent, old),
			rowLine(t, "bbb", repairOutcomeFailed, old),
		}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 2 || len(keep) != 0 {
			t.Fatalf("dropped = %d, kept = %d, want a failed pair dropped too", dropped, len(keep))
		}
	})

	t.Run("an unpaired intent is kept at any age", func(t *testing.T) {
		lines := [][]byte{rowLine(t, "ccc", repairOutcomeIntent, old)}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 0 || len(keep) != 1 {
			t.Fatalf("dropped = %d, kept = %d — a dangling intent is the only pointer left to a clean room",
				dropped, len(keep))
		}
	})

	t.Run("an unparseable line is kept", func(t *testing.T) {
		lines := [][]byte{[]byte("{not json"), rowLine(t, "ddd", repairOutcomeIntent, old), rowLine(t, "ddd", repairOutcomeApplied, old)}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 2 {
			t.Fatalf("dropped = %d, want only the settled pair", dropped)
		}
		if len(keep) != 1 || !bytes.Equal(keep[0], []byte("{not json")) {
			t.Fatalf("keep = %q, want the unreadable line preserved verbatim", keep)
		}
	})

	t.Run("a zero-TS row is kept", func(t *testing.T) {
		lines := [][]byte{
			rowLine(t, "eee", repairOutcomeIntent, time.Time{}),
			rowLine(t, "eee", repairOutcomeApplied, time.Time{}),
		}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 0 || len(keep) != 2 {
			t.Fatalf("dropped = %d, kept = %d — no age could be established, so nothing may drop", dropped, len(keep))
		}
	})

	t.Run("an in-window pair is kept", func(t *testing.T) {
		lines := [][]byte{
			rowLine(t, "fff", repairOutcomeIntent, recent),
			rowLine(t, "fff", repairOutcomeApplied, recent),
		}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 0 || len(keep) != 2 {
			t.Fatalf("dropped = %d, kept = %d, want an in-window pair kept", dropped, len(keep))
		}
	})

	t.Run("a pair straddling the cutoff is kept whole", func(t *testing.T) {
		lines := [][]byte{
			rowLine(t, "ggg", repairOutcomeIntent, old),
			rowLine(t, "ggg", repairOutcomeApplied, recent),
		}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 0 || len(keep) != 2 {
			t.Fatalf("dropped = %d, kept = %d, want the whole pair kept", dropped, len(keep))
		}
	})

	t.Run("a row with no id is kept", func(t *testing.T) {
		lines := [][]byte{rowLine(t, "", repairOutcomeApplied, old)}
		keep, dropped := classifyRepairRows(lines, cutoff)
		if dropped != 0 || len(keep) != 1 {
			t.Fatalf("dropped = %d, kept = %d — an id-less row can be paired with nothing", dropped, len(keep))
		}
	})
}

// --- Prune, end to end -------------------------------------------------------

// pruneClient is repairClient with a removal confirmer wired, so the gate is
// drivable without a TTY where a test wants it.
func pruneClient(t *testing.T, fake *exec.FakeRunner, opts ...Option) *Client {
	t.Helper()
	base := []Option{
		WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(2 * time.Second),
		WithTTYCheck(func() bool { return false }),
	}
	return New(fake, append(base, opts...)...)
}

// seedAside writes a set-aside file whose NAME carries the given age, which is
// the only timestamp prune reads: a rename preserves mtime, so mtime dates the
// record's last write rather than the moment it was set aside.
func seedAside(t *testing.T, c *Client, base string, age time.Duration, body []byte) string {
	t.Helper()
	stamp := time.Now().UTC().Add(-age).Unix()
	name := fmt.Sprintf("%s%s%d", base, unreadableSuffix, stamp)
	path := filepath.Join(c.SessionsDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func defaultPruneOpts() PruneOpts {
	return PruneOpts{OlderThan: 30 * 24 * time.Hour, LogRetention: 90 * 24 * time.Hour, Yes: true}
}

func TestPrune_AYoungSetAsideIsListedNeverRemoved(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	path := seedAside(t, c, "o-r-1-1.json", time.Hour, []byte("{not json"))

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %+v, want the young file listed", report.Items)
	}
	if report.Items[0].Outcome != pruneOutcomeKept {
		t.Errorf("outcome = %q, want %q", report.Items[0].Outcome, pruneOutcomeKept)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a young set-aside file was removed: %v", serr)
	}
}

func TestPrune_AnUnparseableNameIsListedNeverRemoved(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	path := filepath.Join(c.SessionsDir(), "o-r-1-1.json"+unreadableSuffix+"whenever")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeKept {
		t.Fatalf("items = %+v, want the file listed and kept", report.Items)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a file whose age could not be read was removed: %v", serr)
	}
}

func TestPrune_RemovesAnOldSetAsideAndWritesTheIntentFirst(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	raw := []byte("{not json")
	path := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, raw)

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeRemoved {
		t.Fatalf("items = %+v, want one removal", report.Items)
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the file survived a removal: %v", serr)
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatalf("audit rows = %d, want at least the intent and its completion", len(rows))
	}
	if rows[0].Outcome != repairOutcomeIntent {
		t.Errorf("first row outcome = %q, want the intent to precede the unlink", rows[0].Outcome)
	}
	if rows[0].Mode != RepairModePrune {
		t.Errorf("mode = %q, want %q", rows[0].Mode, RepairModePrune)
	}
	if rows[0].Record != string(raw) {
		t.Errorf("intent row record = %q, want the file's own bytes %q — this is an unlink, and the row is the only trace",
			rows[0].Record, raw)
	}
	if rows[0].RecordBytes != len(raw) {
		t.Errorf("record bytes = %d, want %d", rows[0].RecordBytes, len(raw))
	}
	if rows[1].Outcome != repairOutcomeApplied {
		t.Errorf("second row outcome = %q, want the completion", rows[1].Outcome)
	}
}

// TestPrune_AnAppendFaultBetweenIntentAndUnlinkLeavesADanglingIntent seeds the
// fault BETWEEN the intent and the completion, which is the crash shape the
// intent row exists for: the file is gone and one dangling intent carrying its
// bytes is the whole trace.
func TestPrune_AnAppendFaultBetweenIntentAndUnlinkLeavesADanglingIntent(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	raw := []byte("{not json")
	seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, raw)

	// The first append is the intent; fail the SECOND, which is the completion.
	appends := 0
	original := appendRepairRow
	appendRepairRow = func(f repairLogFile, data []byte) error {
		appends++
		if appends == 2 {
			return errors.New("injected append fault")
		}
		return original(f, data)
	}
	t.Cleanup(func() { appendRepairRow = original })

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeRemoved {
		t.Fatalf("items = %+v, want the removal to have happened", report.Items)
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly the dangling intent: %+v", len(rows), rows)
	}
	if rows[0].Outcome != repairOutcomeIntent {
		t.Errorf("row outcome = %q, want the intent", rows[0].Outcome)
	}
	if rows[0].Record != string(raw) {
		t.Errorf("dangling intent record = %q, want the removed file's bytes", rows[0].Record)
	}
}

// TestPrune_AFailedIntentRowRemovesNothing: the row is the recovery pointer, so
// a removal with no row is the one shape that can lose the bytes outright.
func TestPrune_AFailedIntentRowRemovesNothing(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	path := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, []byte("{not json"))

	original := appendRepairRow
	appendRepairRow = func(repairLogFile, []byte) error { return errors.New("injected append fault") }
	t.Cleanup(func() { appendRepairRow = original })

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune returned %v; a per-item refusal must not fail the sweep", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeRefused {
		t.Fatalf("items = %+v, want the item refused", report.Items)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("the file was removed without a trail: %v", serr)
	}
}

func TestPrune_ALiveWindowRefusesThatItemOnly(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	c := pruneClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))
	future := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#1","agent":"claude",` +
		`"createdAt":"2026-09-12T00:00:00Z","version":3,"phase":"active","revision":4}` + "\n")
	live := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, future)
	refless := seedAside(t, c, "o-r-2-1.json", 60*24*time.Hour, []byte("{not json"))

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v — a refused item is not a failed sweep", err)
	}
	byPath := map[string]PruneItem{}
	for _, it := range report.Items {
		byPath[it.Path] = it
	}
	if got := byPath[live].Outcome; got != pruneOutcomeRefused {
		t.Errorf("outcome for the live-window file = %q, want %q", got, pruneOutcomeRefused)
	}
	if _, serr := os.Stat(live); serr != nil {
		t.Errorf("a refused item was removed: %v", serr)
	}
	if got := byPath[refless].Outcome; got != pruneOutcomeRemoved {
		t.Errorf("outcome for the ref-less file = %q, want %q — one live window refuses one item",
			got, pruneOutcomeRemoved)
	}
}

func TestPrune_AnUnreadableWindowListRefusesOnlyRefBearingItems(t *testing.T) {
	c := pruneClient(t, repairRunner(errors.New("tmux is gone")))
	future := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#1","agent":"claude",` +
		`"createdAt":"2026-09-12T00:00:00Z","version":3,"phase":"active","revision":4}` + "\n")
	withRef := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, future)
	refless := seedAside(t, c, "o-r-2-1.json", 60*24*time.Hour, []byte("{not json"))

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	byPath := map[string]PruneItem{}
	for _, it := range report.Items {
		byPath[it.Path] = it
	}
	if got := byPath[withRef].Outcome; got != pruneOutcomeRefused {
		t.Errorf("outcome for the ref-bearing file = %q, want %q — an unreadable list is not an absent window",
			got, pruneOutcomeRefused)
	}
	if _, serr := os.Stat(withRef); serr != nil {
		t.Errorf("a refused item was removed: %v", serr)
	}
	if got := byPath[refless].Outcome; got != pruneOutcomeRemoved {
		t.Errorf("outcome for the ref-less file = %q, want %q — an unreadable list says nothing about a file "+
			"that names no window", got, pruneOutcomeRemoved)
	}
}

// TestPrune_AByteMismatchAgainstThePinnedRereadRefuses: the removal re-reads
// the file through the pinned directory handle and compares it against the
// bytes the intent row carries. A file that changed under the sweep is no
// longer the thing that was authorized.
func TestPrune_AByteMismatchAgainstThePinnedRereadRefuses(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	path := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, []byte("{not json"))

	original := readAsideBytes
	readAsideBytes = func(*os.Root, string) ([]byte, error) { return []byte("{different"), nil }
	t.Cleanup(func() { readAsideBytes = original })

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeFailed {
		t.Fatalf("items = %+v, want the removal to have failed", report.Items)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a file that changed under the sweep was removed anyway: %v", serr)
	}
}

// seedSettledPair writes one settled pair straight into the log, aged past any
// retention window a test will use.
func seedSettledPair(t *testing.T, c *Client, id string, age time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-age)
	for _, outcome := range []string{repairOutcomeIntent, repairOutcomeApplied} {
		if err := c.appendRepairRowLocked(RepairRow{
			TS: ts, ID: id, Mode: RepairModeRollback, Outcome: outcome, RecordPath: "/tmp/x.json",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrune_CompactionDropsSettledPairsAndKeepsItsOwnIntent(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	seedSettledPair(t, c, "old00000000000a", 200*24*time.Hour)
	seedSettledPair(t, c, "new00000000000b", time.Hour)

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if report.Log.Dropped != 2 {
		t.Errorf("dropped = %d, want the one settled pair past the window", report.Log.Dropped)
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	var sawOld, sawNew, sawCompaction bool
	for _, r := range rows {
		switch {
		case r.ID == "old00000000000a":
			sawOld = true
		case r.ID == "new00000000000b":
			sawNew = true
		case r.Mode == RepairModePrune:
			sawCompaction = true
		}
	}
	if sawOld {
		t.Error("the settled pair past the window survived compaction")
	}
	if !sawNew {
		t.Error("an in-window pair was dropped")
	}
	if !sawCompaction {
		t.Error("compaction's own intent row did not survive into the new file — nothing records that it happened")
	}
}

// TestPrune_ACompactionRenameFailureLeavesTheOldLogIntact is the sharp edge:
// the log is the only pointer to a possibly-orphaned clean room, so a failed
// rewrite must lose nothing.
func TestPrune_ACompactionRenameFailureLeavesTheOldLogIntact(t *testing.T) {
	fs := newFaultFS(t, "Rename")
	c := pruneClient(t, repairRunner(nil), WithRecordFS(fs))
	seedSettledPair(t, c, "old00000000000a", 200*24*time.Hour)

	before, err := os.ReadFile(c.repairLogPath()) //nolint:gosec // the test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v — a failed compaction is reported, not fatal", err)
	}
	if report.Log.Outcome != pruneOutcomeFailed {
		t.Errorf("log outcome = %q, want %q", report.Log.Outcome, pruneOutcomeFailed)
	}
	after, err := os.ReadFile(c.repairLogPath()) //nolint:gosec // the test's own t.TempDir
	if err != nil {
		t.Fatalf("the log is unreadable after a failed compaction: %v", err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Fatalf("the old log content did not survive:\nbefore %q\nafter  %q", before, after)
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatalf("the log no longer reads back: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == "old00000000000a" {
			found = true
		}
	}
	if !found {
		t.Error("the rows the failed compaction would have dropped are gone anyway")
	}
	entries, err := os.ReadDir(c.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp log was left behind: %s", e.Name())
		}
	}
}

func TestPrune_NothingRemovableExitsZeroOffATTYWithoutYes(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	opts := defaultPruneOpts()
	opts.Yes = false

	report, err := c.Prune(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prune: %v — a no-op prune must not refuse off a TTY", err)
	}
	if len(report.Items) != 0 {
		t.Errorf("items = %+v, want none", report.Items)
	}
	if report.Log.Outcome != pruneOutcomeUnchanged {
		t.Errorf("log outcome = %q, want %q", report.Log.Outcome, pruneOutcomeUnchanged)
	}
}

func TestPrune_DryRunOffATTYWithoutYesTouchesNothing(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	path := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, []byte("{not json"))
	seedSettledPair(t, c, "old00000000000a", 200*24*time.Hour)
	before, err := os.ReadFile(c.repairLogPath()) //nolint:gosec // the test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}

	opts := defaultPruneOpts()
	opts.Yes = false
	opts.DryRun = true
	report, err := c.Prune(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prune --dry-run: %v — there is nothing to confirm", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeWouldRemove {
		t.Fatalf("items = %+v, want one would-remove", report.Items)
	}
	if report.Log.Outcome != pruneOutcomeWouldCompact {
		t.Errorf("log outcome = %q, want %q", report.Log.Outcome, pruneOutcomeWouldCompact)
	}
	if report.Log.Dropped != 2 {
		t.Errorf("would drop %d rows, want 2", report.Log.Dropped)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("--dry-run removed the file: %v", serr)
	}
	after, err := os.ReadFile(c.repairLogPath()) //nolint:gosec // the test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("--dry-run wrote to the log:\nbefore %q\nafter  %q", before, after)
	}
}

func TestPrune_OffATTYWithoutYesRefusesRealWork(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	path := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, []byte("{not json"))

	opts := defaultPruneOpts()
	opts.Yes = false
	if _, err := c.Prune(context.Background(), opts); err == nil {
		t.Fatal("expected a refusal: this unlinks files and there is no terminal to confirm on")
	} else if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal %q does not name --yes", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refusal removed the file: %v", serr)
	}
}

func TestPrune_TheInteractiveGateHonorsADecline(t *testing.T) {
	var prompted string
	c := pruneClient(t, repairRunner(nil),
		WithTTYCheck(func() bool { return true }),
		WithRemovalConfirmer(func(prompt string) (bool, error) { prompted = prompt; return false, nil }),
	)
	path := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, []byte("{not json"))

	opts := defaultPruneOpts()
	opts.Yes = false
	report, err := c.Prune(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !strings.Contains(prompted, "1") {
		t.Errorf("prompt %q does not say how much is being removed", prompted)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeDeclined {
		t.Fatalf("items = %+v, want the decline honored", report.Items)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a declined prune removed the file: %v", serr)
	}
}

// TestPrune_LeavesOrdinaryRecordsAlone is the blast-radius assertion: the sweep
// names set-aside files only, so a live .json record and the lock are untouched.
func TestPrune_LeavesOrdinaryRecordsAlone(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	rec := seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, PhasePreparing, "")
	seedAside(t, c, "o-r-2-1.json", 60*24*time.Hour, []byte("{not json"))

	if _, err := c.Prune(context.Background(), defaultPruneOpts()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, serr := os.Stat(rec); serr != nil {
		t.Errorf("prune touched an ordinary session record: %v", serr)
	}
}

// TestPruneReport_JSONShape holds the contract the CLI encodes.
func TestPruneReport_JSONShape(t *testing.T) {
	c := pruneClient(t, repairRunner(nil))
	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Items []map[string]json.RawMessage `json:"items"`
		Log   map[string]json.RawMessage   `json:"log"`
	}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("report did not round-trip: %v\n%s", err, data)
	}
	if back.Items == nil {
		t.Errorf("items encoded as null, want []: %s", data)
	}
	if back.Log == nil {
		t.Errorf("log encoded as null, want an object: %s", data)
	}
}
