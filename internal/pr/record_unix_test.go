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

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
