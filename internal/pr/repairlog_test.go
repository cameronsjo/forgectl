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
	"errors"
	"io"
	"os"
	"path/filepath"
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
