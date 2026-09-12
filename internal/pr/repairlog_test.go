package pr

// Test plan for repairlog.go (forgectl#299 Task 2)
//
// appendRepairRow (Classification: write-ahead audit trail)
//   [x] Happy: the row lands whole and the file is fsynced
//   [x] A short write leaves NO partial bytes behind, so the next row is still
//       readable — the failure that would otherwise cost TWO rows
//   [x] A sync failure rolls the row back too
//   [x] A failed truncate is reported alongside the original cause

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
// concatenate onto a truncated line, and readRepairLog drops the merged result
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
