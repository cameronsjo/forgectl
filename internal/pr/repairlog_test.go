package pr

// Test plan for repairlog.go (forgectl#299 Task 2)
//
// appendRepairRow (Classification: write-ahead audit trail)
//   [x] Happy: the row lands whole and the file is fsynced
//   [x] A short write leaves NO partial bytes behind, so the next row is still
//       readable — the failure that would otherwise cost TWO rows
//   [x] A sync failure rolls the row back too
//   [x] A failed truncate is reported alongside the original cause
//   [x] An unterminated tail gets a separator, so the row stands on its own
//       line rather than merging into an undecodable one (forgectl#549)
//   [x] An empty file and a '\n'-terminated one get no separator
//   [x] A short write that lands only the separator is rolled back whole
//   [x] An unreadable last byte gets the separator: a blank line is harmless

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// stubLogFile is a repairLogFile over an in-memory buffer, with one injectable
// defect. It is not backed by a real file because a short write is not
// reachable on one: the kernel either writes the whole buffer or errors.
type stubLogFile struct {
	data []byte
	// shortBy truncates the next write to len(p)-shortBy bytes.
	shortBy  int
	syncErr  error
	truncErr error
	readErr  error
	synced   bool
}

func (f *stubLogFile) Write(p []byte) (int, error) {
	n := len(p)
	if f.shortBy > 0 {
		n = len(p) - f.shortBy
		f.shortBy = 0
	}
	f.data = append(f.data, p[:n]...)
	return n, nil
}

func (f *stubLogFile) ReadAt(p []byte, off int64) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if off < 0 || off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *stubLogFile) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekEnd || offset != 0 {
		return 0, errors.New("stubLogFile only supports seeking to the end")
	}
	return int64(len(f.data)), nil
}

func (f *stubLogFile) Truncate(size int64) error {
	if f.truncErr != nil {
		return f.truncErr
	}
	if size > int64(len(f.data)) {
		return errors.New("truncate past the end")
	}
	f.data = f.data[:size]
	return nil
}

func (f *stubLogFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	f.synced = true
	return nil
}

func TestAppendRepairRow_WritesTheWholeRowAndSyncs(t *testing.T) {
	f := &stubLogFile{}
	if err := appendRepairRow(f, []byte("{\"id\":\"a\"}\n")); err != nil {
		t.Fatalf("appendRepairRow: %v", err)
	}
	if string(f.data) != "{\"id\":\"a\"}\n" {
		t.Errorf("file = %q, want the whole row", f.data)
	}
	if !f.synced {
		t.Error("the row was not fsynced; an audit trail that is not durable is not one")
	}
}

// TestAppendRepairRow_ShortWriteLeavesNoPartialRow is the failure that costs
// TWO rows, not one: partial bytes left in the file make the NEXT append
// concatenate onto a truncated line, and readRepairLogTail drops the merged result
// as unparseable. On an out-of-space tail that is the intent row naming a clean
// room — the one line the log exists to preserve.
func TestAppendRepairRow_ShortWriteLeavesNoPartialRow(t *testing.T) {
	f := &stubLogFile{}
	first := []byte("{\"id\":\"first\"}\n")
	if err := appendRepairRow(f, first); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	f.shortBy = 5
	err := appendRepairRow(f, []byte("{\"id\":\"second\"}\n"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
	if string(f.data) != string(first) {
		t.Fatalf("file = %q, want only the first row — the partial append was not rolled back", f.data)
	}

	// And the next row still lands whole, which is the property that matters.
	if err := appendRepairRow(f, []byte("{\"id\":\"third\"}\n")); err != nil {
		t.Fatalf("third row: %v", err)
	}
	if want := string(first) + "{\"id\":\"third\"}\n"; string(f.data) != want {
		t.Errorf("file = %q, want %q", f.data, want)
	}
}

func TestAppendRepairRow_SyncFailureRollsTheRowBack(t *testing.T) {
	f := &stubLogFile{syncErr: errors.New("disk said no")}
	if err := appendRepairRow(f, []byte("{\"id\":\"a\"}\n")); err == nil {
		t.Fatal("expected the sync failure to surface")
	}
	if len(f.data) != 0 {
		t.Errorf("file = %q, want empty — an unsynced row must not be left behind", f.data)
	}
}

func TestAppendRepairRow_ReportsAFailedRollbackAlongsideTheCause(t *testing.T) {
	f := &stubLogFile{shortBy: 3, truncErr: errors.New("truncate refused")}
	err := appendRepairRow(f, []byte("{\"id\":\"a\"}\n"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want the original short-write cause preserved", err)
	}
	if !strings.Contains(err.Error(), "truncate refused") {
		t.Errorf("err = %q, want it to name the failed rollback too", err)
	}
}

// TestAppendRepairRowLocked_AnUnterminatedTailDoesNotSwallowTheRow is
// forgectl#549: a single stray byte with no newline after it, and the next
// genuine row merged into it as one undecodable line the reader skipped — a
// real intent row gone from the history with nothing said.
func TestAppendRepairRowLocked_AnUnterminatedTailDoesNotSwallowTheRow(t *testing.T) {
	c := testClient(t, nil)
	writeRepairLogRaw(t, c, "x")
	if err := c.appendRepairRowLocked(RepairRow{ID: "real", Outcome: repairOutcomeIntent}); err != nil {
		t.Fatalf("appendRepairRowLocked: %v", err)
	}
	rows, _, skipped, err := c.readRepairLogTail(10)
	if err != nil {
		t.Fatalf("readRepairLogTail: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "real" {
		t.Fatalf("rows = %+v, want the real row decoded on its own line", rows)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1: the stray prefix is its own unreadable line", skipped)
	}
}

// TestAppendRepairRowLocked_NoSeparatorOnAnEmptyOrTerminatedLog pins the other
// side: the separator is for an unterminated tail only, so a fresh log starts
// with its row and a clean one gains no blank line per append.
func TestAppendRepairRowLocked_NoSeparatorOnAnEmptyOrTerminatedLog(t *testing.T) {
	for _, seed := range []string{"", repairRowLine(t, RepairRow{ID: "prev"})} {
		c := testClient(t, nil)
		writeRepairLogRaw(t, c, seed)
		if err := c.appendRepairRowLocked(RepairRow{ID: "a", Outcome: repairOutcomeIntent}); err != nil {
			t.Fatalf("appendRepairRowLocked: %v", err)
		}
		row, err := marshalRepairRow(RepairRow{ID: "a", Outcome: repairOutcomeIntent})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(c.repairLogPath())
		if err != nil {
			t.Fatal(err)
		}
		if want := seed + string(row); string(got) != want {
			t.Errorf("seed %q: file = %q, want %q with no separator", seed, got, want)
		}
	}
}

// TestAppendRepairRow_ASeparatorOnlyShortWriteRollsBackWhole: the separator and
// the row are one write, so a write that lands the separator alone leaves the
// file exactly as it was — still unterminated, so the next append separates
// again rather than trusting a newline that was rolled back.
func TestAppendRepairRow_ASeparatorOnlyShortWriteRollsBackWhole(t *testing.T) {
	row := []byte("{\"id\":\"a\"}\n")
	f := &stubLogFile{data: []byte("x"), shortBy: len(row)}
	if err := appendRepairRow(f, row); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
	if string(f.data) != "x" {
		t.Fatalf("file = %q, want the untouched prefix", f.data)
	}
	if err := appendRepairRow(f, row); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if want := "x\n" + string(row); string(f.data) != want {
		t.Errorf("file = %q, want %q", f.data, want)
	}
}

// TestAppendRepairRow_AnUnreadableLastByteGetsTheSeparator: when the tail
// cannot be checked, a blank line (skipped by every reader) is the safe guess
// and a merged row is not.
func TestAppendRepairRow_AnUnreadableLastByteGetsTheSeparator(t *testing.T) {
	row := []byte("{\"id\":\"a\"}\n")
	f := &stubLogFile{data: []byte("{\"id\":\"p\"}"), readErr: errors.New("pread refused")}
	if err := appendRepairRow(f, row); err != nil {
		t.Fatalf("appendRepairRow: %v", err)
	}
	if want := "{\"id\":\"p\"}\n" + string(row); string(f.data) != want {
		t.Errorf("file = %q, want %q", f.data, want)
	}
}

// TestAppendRepairRowLocked_CreatesThe0600Log pins the on-disk posture: the
// audit trail lives beside the records it describes, private, and with an
// extension List's enumeration never sees.
func TestAppendRepairRowLocked_CreatesThe0600Log(t *testing.T) {
	c := testClient(t, nil)
	if err := c.appendRepairRowLocked(RepairRow{ID: "a", Outcome: repairOutcomeIntent}); err != nil {
		t.Fatalf("appendRepairRowLocked: %v", err)
	}
	path := filepath.Join(c.SessionsDir(), repairLogName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the audit log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
	if filepath.Ext(path) == ".json" {
		t.Error("the audit log must not carry the .json extension List enumerates")
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatalf("readRepairLog: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "a" {
		t.Errorf("rows = %+v, want the one row back", rows)
	}
}

// TestRepairRow_EveryStringFieldHasASizingDecision is the structural guard, and
// it is the point of this whole round. The same defect returned three times,
// one field over each time — Record, then Ref, then Workspace — because the
// shrink loop was a hand-picked list and the row was not. Reflecting over the
// struct makes the set exhaustive: a new string field added without a decision
// fails here rather than silently reopening the class.
func TestRepairRow_EveryStringFieldHasASizingDecision(t *testing.T) {
	var row RepairRow
	shrinkable := map[string]bool{}
	for _, f := range shrinkableRowFields(&row) {
		if shrinkable[f.Name] {
			t.Errorf("field %s is listed twice in the shrink order", f.Name)
		}
		shrinkable[f.Name] = true
	}

	rt := reflect.TypeOf(row)
	seen := 0
	for i := range rt.NumField() {
		field := rt.Field(i)
		if field.Type.Kind() != reflect.String {
			continue
		}
		seen++
		_, bounded := boundedRowFields[field.Name]
		switch {
		case shrinkable[field.Name] && bounded:
			t.Errorf("field %s is both shrunk and declared bounded; pick one", field.Name)
		case !shrinkable[field.Name] && !bounded:
			t.Errorf("field %s has no sizing decision: add it to shrinkableRowFields, "+
				"or to boundedRowFields with the bound that makes leaving it out safe", field.Name)
		}
	}
	if seen == 0 {
		t.Fatal("reflected over no string fields; the guard is asserting nothing")
	}
	// And the reverse: a name in either list that is not a field at all is a
	// rename nobody finished.
	for name := range boundedRowFields {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("boundedRowFields names %s, which is not a field on RepairRow", name)
		}
	}
	for name := range shrinkable {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("the shrink order names %s, which is not a field on RepairRow", name)
		}
	}
	for name, why := range boundedRowFields {
		if strings.TrimSpace(why) == "" {
			t.Errorf("boundedRowFields[%s] states no bound", name)
		}
	}
}

// TestMarshalRepairRow_EachFieldAloneCanOverflowAndIsShrunk fills each
// shrinkable field, one at a time, with far more than the line budget of the
// worst-expanding byte, and proves the row still lands — with the note naming
// that field, so the truncation is never silent.
//
// The allowlisted fields are deliberately absent: 12 KiB in Mode or Outcome is
// not an input production can construct (they are package constants), and the
// structural test above is what holds their bounds. Asserting against a value
// no producer can emit would be testing the test.
func TestMarshalRepairRow_EachFieldAloneCanOverflowAndIsShrunk(t *testing.T) {
	var probe RepairRow
	for _, f := range shrinkableRowFields(&probe) {
		t.Run(f.Name, func(t *testing.T) {
			row := RepairRow{
				TS: fixedTime(), ID: "abcdef0123456789",
				FromPhase: repairPhaseUnreadable, Mode: RepairModeForgetIfAbsent,
				Outcome: repairOutcomeIntent,
			}
			// Find the same field on THIS row and fill it. Every byte expands
			// six-fold on marshal, so 12 KiB clears the 8 KiB limit alone.
			var target *string
			for _, g := range shrinkableRowFields(&row) {
				if g.Name == f.Name {
					target = g.Value
				}
			}
			if target == nil {
				t.Fatalf("field %s vanished between two calls", f.Name)
			}
			*target = strings.Repeat("<", 12<<10)

			data, err := marshalRepairRow(row)
			if err != nil {
				t.Fatalf("an oversized %s refused the row: %v", f.Name, err)
			}
			if len(data) > maxRepairLogLineBytes {
				t.Fatalf("encoded row is %d bytes, over the %d limit", len(data), maxRepairLogLineBytes)
			}
			if n := strings.Count(string(data), "\n"); n != 1 || data[len(data)-1] != '\n' {
				t.Fatalf("row is not exactly one newline-terminated line (%d newlines)", n)
			}
			var back RepairRow
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("row does not parse: %v", err)
			}
			if !strings.Contains(back.RecordNote, f.Label) {
				t.Errorf("note = %q, want it to name the truncated %s", back.RecordNote, f.Label)
			}
		})
	}
}

// TestComposeRepairActor_CutsOnARuneBoundary is the actor field's own version
// of the cap defect: the bound was a plain byte slice, so a session id of
// multi-byte runes could be cut mid-sequence, and json.Marshal then rewrote the
// orphan to U+FFFD — quietly changing the one field that says who ran the
// command. The session id comes from the environment, so multi-byte content in
// it is input, not a hypothesis.
func TestComposeRepairActor_CutsOnARuneBoundary(t *testing.T) {
	// 6-byte username, then a session id of 3-byte runes: the actor is
	// "abcdef session=" (15 bytes) plus 3n, so the 256-byte bound lands 241
	// bytes into the id — not a multiple of three, and therefore inside a rune.
	actor := composeRepairActor("abcdef", strings.Repeat("好", 200))
	if len(actor) > maxActorBytes {
		t.Fatalf("actor is %d bytes, over the %d bound", len(actor), maxActorBytes)
	}
	if !utf8.ValidString(actor) {
		t.Fatalf("actor is not valid UTF-8: %q", actor[max(0, len(actor)-8):])
	}
	if !strings.HasPrefix(actor, "abcdef session=") {
		t.Fatalf("actor = %q, want it to keep the identifying prefix", actor)
	}

	// And it survives the marshal VERBATIM — the whole point, since a rewritten
	// actor reads as a real value nobody typed.
	data, err := marshalRepairRow(RepairRow{
		TS: fixedTime(), ID: "abcdef0123456789", Actor: actor,
		FromPhase: repairPhaseUnreadable, Mode: RepairModeForgetIfAbsent, Outcome: repairOutcomeIntent,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RepairRow
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("row does not parse: %v", err)
	}
	if back.Actor != actor {
		t.Errorf("actor round-tripped as %q, want the composed %q", back.Actor, actor)
	}
	if strings.ContainsRune(back.Actor, utf8.RuneError) {
		t.Errorf("actor carries U+FFFD, so json.Marshal rewrote bytes the field was supposed to preserve: %q", back.Actor)
	}
}

// readRepairLog is the unbounded read the older tests were written against: the
// tail reader with a limit nothing reaches.
func (c *Client) readRepairLog() ([]RepairRow, error) {
	rows, _, _, err := c.readRepairLogTail(math.MaxInt)
	return rows, err
}

func writeRepairLogRaw(t *testing.T, c *Client, content string) {
	t.Helper()
	if err := os.WriteFile(c.repairLogPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func repairRowLine(t *testing.T, row RepairRow) string {
	t.Helper()
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func TestReadRepairLogTail_KeepsTheNewestRowsInOrder(t *testing.T) {
	c := testClient(t, nil)
	var log strings.Builder
	for i := 0; i < 8; i++ {
		log.WriteString(repairRowLine(t, RepairRow{ID: fmt.Sprintf("r%d", i)}))
	}
	writeRepairLogRaw(t, c, log.String())

	rows, omitted, _, err := c.readRepairLogTail(3)
	if err != nil {
		t.Fatalf("readRepairLogTail: %v", err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if want := []string{"r5", "r6", "r7"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want the newest three oldest-first %v", ids, want)
	}
	if omitted != 5 {
		t.Errorf("omitted = %d, want 5", omitted)
	}
}

func TestReadRepairLogTail_SkipsAnOverLongLineAndKeepsTheRest(t *testing.T) {
	c := testClient(t, nil)
	writeRepairLogRaw(t, c, repairRowLine(t, RepairRow{ID: "before"})+
		strings.Repeat("x", 9000)+"\n"+
		repairRowLine(t, RepairRow{ID: "after"}))

	rows, omitted, _, err := c.readRepairLogTail(10)
	if err != nil {
		t.Fatalf("an over-long line must not fail the read: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "before" || rows[1].ID != "after" {
		t.Errorf("rows = %+v, want before and after", rows)
	}
	if omitted != 0 {
		t.Errorf("omitted = %d, want 0: a skipped line is not an omitted row", omitted)
	}
}

func TestReadRepairLogTail_AcceptsARowAtExactlyTheLineLimit(t *testing.T) {
	c := testClient(t, nil)
	row := RepairRow{ID: "big", Detail: "a"}
	base := repairRowLine(t, row)
	row.Detail = strings.Repeat("a", 1+maxRepairLogLineBytes-len(base))
	big := repairRowLine(t, row)
	if len(big) != maxRepairLogLineBytes {
		t.Fatalf("fixture line is %d bytes, want exactly %d", len(big), maxRepairLogLineBytes)
	}
	writeRepairLogRaw(t, c, big+repairRowLine(t, RepairRow{ID: "next"}))

	rows, _, _, err := c.readRepairLogTail(10)
	if err != nil {
		t.Fatalf("readRepairLogTail: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "big" || rows[1].ID != "next" {
		t.Errorf("rows = %d, want the at-limit row and the next one", len(rows))
	}
}

func TestReadRepairLogTail_ReadsAFinalLineWithNoNewline(t *testing.T) {
	c := testClient(t, nil)
	writeRepairLogRaw(t, c, repairRowLine(t, RepairRow{ID: "a"})+
		strings.TrimSuffix(repairRowLine(t, RepairRow{ID: "b"}), "\n"))

	rows, _, _, err := c.readRepairLogTail(10)
	if err != nil {
		t.Fatalf("readRepairLogTail: %v", err)
	}
	if len(rows) != 2 || rows[1].ID != "b" {
		t.Errorf("rows = %+v, want the unterminated final row kept", rows)
	}
}

func intentRow(id string) RepairRow { return RepairRow{ID: id, Outcome: repairOutcomeIntent} }
func doneRow(id string) RepairRow   { return RepairRow{ID: id, Outcome: repairOutcomeApplied} }

func seedRows(t *testing.T, c *Client, rows ...RepairRow) {
	t.Helper()
	var log strings.Builder
	for _, r := range rows {
		log.WriteString(repairRowLine(t, r))
	}
	writeRepairLogRaw(t, c, log.String())
}

// TestScanRepairLogTail_CountsOnlyDisplacedIntentsNoCompletionCloses covers
// every way a displaced intent can be paired, with a ring of 4 over ten rows.
// Displaced: U, P (intent), P (completion), S (intent), filler, L (intent).
// Only U is unpaired: P's completion was displaced too, S's completion is
// still in the ring at the end, and L's completion arrived after L left it.
func TestScanRepairLogTail_CountsOnlyDisplacedIntentsNoCompletionCloses(t *testing.T) {
	c := testClient(t, nil)
	seedRows(t, c,
		intentRow("U"), intentRow("P"), doneRow("P"), intentRow("S"), doneRow("f1"),
		intentRow("L"), doneRow("S"), doneRow("f2"), doneRow("f3"), doneRow("L"))

	tail, err := c.scanRepairLogTail(4, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tail.omitted != 6 {
		t.Fatalf("omitted = %d, want 6", tail.omitted)
	}
	if tail.omittedUnpaired != 1 || tail.unpairedCapped {
		t.Errorf("omittedUnpaired = %d (capped %v), want exactly 1 (U) and not capped", tail.omittedUnpaired, tail.unpairedCapped)
	}
}

// TestScanRepairLogTail_AnIntentStillInTheRingIsNotCounted: an unpaired intent
// the view still shows is visible already, so it must not be in the omitted
// count.
func TestScanRepairLogTail_AnIntentStillInTheRingIsNotCounted(t *testing.T) {
	c := testClient(t, nil)
	seedRows(t, c, doneRow("a"), doneRow("b"), intentRow("visible"))
	tail, err := c.scanRepairLogTail(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tail.omitted != 2 || tail.omittedUnpaired != 0 {
		t.Errorf("omitted=%d unpaired=%d, want 2 and 0", tail.omitted, tail.omittedUnpaired)
	}
}

// TestScanRepairLogTail_UnpairedTrackingIsCapped: past the cap the count stops
// growing and says it is a lower bound, so heap stays bounded.
func TestScanRepairLogTail_UnpairedTrackingIsCapped(t *testing.T) {
	c := testClient(t, nil)
	var rows []RepairRow
	for i := 0; i < 5; i++ {
		rows = append(rows, intentRow(fmt.Sprintf("u%d", i)))
	}
	rows = append(rows, doneRow("x"))
	seedRows(t, c, rows...)
	tail, err := c.scanRepairLogTail(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if tail.omittedUnpaired != 3 || !tail.unpairedCapped {
		t.Errorf("omittedUnpaired=%d capped=%v, want 3 and capped", tail.omittedUnpaired, tail.unpairedCapped)
	}
}

// TestScanRepairLogTail_UndecodableLinesAreNeverAttributed: a garbled line that
// merely mentions an intent stays in skipped and adds nothing to the count.
func TestScanRepairLogTail_UndecodableLinesAreNeverAttributed(t *testing.T) {
	c := testClient(t, nil)
	writeRepairLogRaw(t, c, `{"id":"g","outcome":"intent"`+"\n"+repairRowLine(t, doneRow("a"))+repairRowLine(t, doneRow("b")))
	tail, err := c.scanRepairLogTail(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tail.skipped != 1 || tail.omittedUnpaired != 0 {
		t.Errorf("skipped=%d unpaired=%d, want 1 and 0", tail.skipped, tail.omittedUnpaired)
	}
}

// TestRepairHistory_ReportsOmittedUnpairedIntents runs the public path over
// more than MaxRepairHistoryRows rows.
func TestRepairHistory_ReportsOmittedUnpairedIntents(t *testing.T) {
	c := testClient(t, nil)
	rows := []RepairRow{intentRow("u10")}
	for i := 0; i < MaxRepairHistoryRows+50; i++ {
		rows = append(rows, doneRow(fmt.Sprintf("d%d", i)))
	}
	seedRows(t, c, rows...)
	trail, err := c.RepairHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if trail.OmittedUnpaired != 1 || trail.OmittedUnpairedCapped || trail.Omitted != 51 {
		t.Errorf("trail = omitted %d, unpaired %d, capped %v; want 51, 1, false", trail.Omitted, trail.OmittedUnpaired, trail.OmittedUnpairedCapped)
	}
}
