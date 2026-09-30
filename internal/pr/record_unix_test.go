//go:build unix

package pr

// Test plan for osRecordFS.ReadFile's open (forgectl#621)
//
// The record reader opens with O_NOFOLLOW|O_NONBLOCK and Fstat's the
// descriptor, because its caller's Lstat checks the path, not what the open
// reaches.
//
//   [x] A FIFO record fails fast rather than blocking the open
//   [x] A symlink to a regular record is refused, not followed
//   [x] A regular record still reads, byte for byte
//   [x] Verb level: a FIFO named like a record in the sessions dir neither
//       hangs List nor Teardown/Cleanup, and List still lists the real record
//   [x] The record loader itself refuses a FIFO fast (every verb reads
//       through loadBreadcrumbRecord)

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Mutation that turns it red: in osRecordFS.ReadFile, open with
// os.Open(path) instead of openNoFollowNonblock — the open then blocks
// waiting for a writer, and mustFailFast fails the test.
func TestOSRecordFSReadFile_FIFOFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o-r-1-1.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	err := mustFailFast(t, "ReadFile on a FIFO", func() error {
		_, err := osRecordFS{}.ReadFile(path)
		return err
	})
	if !errors.Is(err, errRecordNotRegular) {
		t.Fatalf("ReadFile on a FIFO = %v, want errRecordNotRegular", err)
	}
}

// Mutation that turns it red: drop unix.O_NOFOLLOW from openNoFollowNonblock's
// flags (or reopen with os.Open) — the symlink is followed and the target's
// bytes come back.
func TestOSRecordFSReadFile_RefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, v2Record(t, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "o-r-1-1.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	data, err := osRecordFS{}.ReadFile(link)
	if err == nil {
		t.Fatalf("ReadFile followed a symlink and read %d bytes", len(data))
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("ReadFile on a symlink = %v, want an ELOOP refusal at the open", err)
	}
}

// Control: the hardened open changes nothing for a regular record.
func TestOSRecordFSReadFile_RegularRecordReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o-r-1-1.json")
	want := v2Record(t, 3)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := osRecordFS{}.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile on a regular record: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}
}

// fifoRecord plants a FIFO nobody writes to, named like a record, in dir.
func fifoRecord(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "o-r-1-1.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return path
}

// Mutation that turns it red: in loadBreadcrumbRecord, read with os.Open
// instead of readRecordFile — the open blocks waiting for a writer.
func TestLoadBreadcrumbRecord_FIFOFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := fifoRecord(t, dir)
	err := mustFailFast(t, "loadBreadcrumbRecord on a FIFO", func() error {
		_, _, err := loadBreadcrumbRecord(path, dir)
		return err
	})
	if !errors.Is(err, errRecordNotRegular) {
		t.Fatalf("loadBreadcrumbRecord on a FIFO = %v, want errRecordNotRegular", err)
	}
}

// TestList_AFIFONamedLikeARecordIsSkippedFast is the reviewer's reproduction
// on #755: a FIFO named o-r-1-1.json blocked List for as long as it waited,
// holding the lifecycle lock. It is skipped, and the real record beside it is
// still listed.
//
// Mutation that turns it red: remove the e.Type().IsRegular() skip in
// listLocked AND revert loadBreadcrumbRecord to os.Open — List then blocks.
// Removing only the skip makes the FIFO an unreadable row (the count check
// below).
func TestList_AFIFONamedLikeARecordIsSkippedFast(t *testing.T) {
	c := testClient(t, nil)
	ws := fakeWorkspace(t)
	seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, PhasePrepared, ws)
	fifoRecord(t, c.SessionsDir())

	var sums []SessionSummary
	var unreadable int
	err := mustFailFast(t, "List", func() error {
		var lerr error
		sums, unreadable, lerr = c.List(context.Background())
		return lerr
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sums) != 1 || unreadable != 0 {
		t.Errorf("List = %d summaries, %d unreadable; want the one real record and no unreadable row", len(sums), unreadable)
	}
}

// Teardown aimed at the FIFO, and a Cleanup sweep over a dir holding one,
// both return rather than block, and the FIFO is left in place.
//
// Mutation that turns it red: revert the member resolver's read to os.Open
// together with dropping its Lstat regular-file check, or remove the
// listLocked skip together with reverting loadBreadcrumbRecord.
func TestTeardownAndCleanup_AFIFONamedLikeARecordFailFast(t *testing.T) {
	t.Run("teardown", func(t *testing.T) {
		c := testClient(t, nil)
		path := fifoRecord(t, c.SessionsDir())
		err := mustFailFast(t, "Teardown", func() error { return c.Teardown(context.Background(), path) })
		if err == nil {
			t.Fatal("Teardown of a FIFO succeeded")
		}
		if _, lerr := os.Lstat(path); lerr != nil {
			t.Errorf("the FIFO was removed: %v", lerr)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		c := testClient(t, nil)
		path := fifoRecord(t, c.SessionsDir())
		_ = mustFailFast(t, "Cleanup", func() error {
			_, err := c.Cleanup(context.Background(), time.Now().UTC().Format("2006-01-02"))
			return err
		})
		if _, lerr := os.Lstat(path); lerr != nil {
			t.Errorf("the FIFO was removed: %v", lerr)
		}
	})
}
