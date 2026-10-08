//go:build unix

package worker

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDrainFiles(t *testing.T) (DrainFiles, string) {
	t.Helper()
	state := t.TempDir()
	return DrainFiles{stateBase: state}, filepath.Join(state, "forgectl", "surface")
}

func TestDrainLockIsExclusive(t *testing.T) {
	d, dir := testDrainFiles(t)
	first, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Lock(); !errors.Is(err, ErrDrainLocked) {
		t.Fatalf("second Lock: err %v, want ErrDrainLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	defer again.Close() //nolint:errcheck // test
	st, err := os.Stat(filepath.Join(dir, drainLockName))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("drain.lock mode %v, err %v; want 0600", st.Mode().Perm(), err)
	}
}

func TestDrainFilesRefuseASymlink(t *testing.T) {
	d, dir := testDrainFiles(t)
	lock, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, drainEventsName)); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendEvent(lock, []byte("{}\n")); !errors.Is(err, ErrLedgerUnreadable) {
		t.Fatalf("append through a symlink: err %v, want ErrLedgerUnreadable", err)
	}
	//nolint:gosec // G304: the test's own temp file
	if got, _ := os.ReadFile(target); string(got) != "x" {
		t.Fatalf("the symlink target was written: %q", got)
	}
}

func TestDrainStatusRoundTrip(t *testing.T) {
	d, dir := testDrainFiles(t)
	if got, err := d.ReadStatus(); err != nil || got != nil {
		t.Fatalf("absent status: %q, %v", got, err)
	}
	lock, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test
	if err := d.WriteStatus(lock, []byte(`{"status":"running"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadStatus()
	if err != nil || !bytes.Contains(got, []byte("running")) {
		t.Fatalf("ReadStatus = %q, %v", got, err)
	}
	st, err := os.Stat(filepath.Join(dir, drainStatusName))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("drain.json mode %v, err %v; want 0600", st.Mode().Perm(), err)
	}
}

// TestDrainEventsRotateAtTheCap pins the one-file rotation: a line that
// would pass the cap renames the file to .1, replacing the previous .1.
func TestDrainEventsRotateAtTheCap(t *testing.T) {
	d, _ := testDrainFiles(t)
	lock, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test
	// One 1 KiB line.
	line := append(bytes.Repeat([]byte("a"), 1023), '\n')
	for range MaxDrainEventsBytes / len(line) {
		if err := d.AppendEvent(lock, line); err != nil {
			t.Fatal(err)
		}
	}
	older, current, err := d.ReadEvents()
	if err != nil || older != nil || len(current) != MaxDrainEventsBytes {
		t.Fatalf("at the cap: older %d bytes, current %d bytes, err %v; want 0 and %d", len(older), len(current), err, MaxDrainEventsBytes)
	}
	marker := []byte("second\n")
	if err := d.AppendEvent(lock, marker); err != nil {
		t.Fatal(err)
	}
	older, current, err = d.ReadEvents()
	if err != nil || len(older) != MaxDrainEventsBytes || !bytes.Equal(current, marker) {
		t.Fatalf("after rotation: older %d bytes, current %q, err %v", len(older), current, err)
	}
	// A second rotation replaces .1 rather than keeping two old files.
	for range MaxDrainEventsBytes / len(line) {
		if err := d.AppendEvent(lock, line); err != nil {
			t.Fatal(err)
		}
	}
	older, _, err = d.ReadEvents()
	if err != nil || !bytes.HasPrefix(older, marker) {
		t.Fatalf("second rotation: .1 starts %q, err %v; want the file that began with the marker", older[:min(len(older), 10)], err)
	}
	if err := d.AppendEvent(lock, []byte("no newline")); err == nil {
		t.Error("an event without a trailing newline was appended")
	}
}

func TestQueueClaimForRecordsTheSession(t *testing.T) {
	q, _ := testQueue(t)
	mustEnqueue(t, q, "w1", "fix the thing")
	row, err := q.ClaimFor("w1", "launch-1", "fleet", queueNow)
	if err != nil || row.Session != "fleet" || row.LaunchID != "launch-1" || row.State != QueueClaimed {
		t.Fatalf("ClaimFor = %+v, %v", row, err)
	}
}

// TestQueueRemoveIfIsConditional pins the prune's guard: a row dequeued and
// enqueued again after the read is not removed.
func TestQueueRemoveIfIsConditional(t *testing.T) {
	q, _ := testQueue(t)
	read := mustEnqueue(t, q, "w1", "fix the thing")
	if _, err := q.Dequeue("w1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Enqueue("w1", "/repo/one", "fix the thing", "", queueNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RemoveIf("w1", SameRead(read)); !errors.Is(err, ErrQueueRowChanged) {
		t.Fatalf("RemoveIf of a re-enqueued row: err %v, want ErrQueueRowChanged", err)
	}
	rows, err := q.Rows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %+v, err %v; the re-enqueued row must stay", rows, err)
	}
	if _, err := q.RemoveIf("w1", SameRead(rows[0])); err != nil {
		t.Fatalf("RemoveIf of the row as read: %v", err)
	}
	if rows, _ := q.Rows(); len(rows) != 0 {
		t.Fatalf("rows after remove: %+v", rows)
	}
	if _, err := q.RemoveIf("w1", SameRead(read)); !errors.Is(err, ErrQueueNoRow) {
		t.Fatalf("RemoveIf of a missing row: err %v, want ErrQueueNoRow", err)
	}
}
