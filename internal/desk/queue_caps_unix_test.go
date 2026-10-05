//go:build unix

package desk

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// One scan reads at most ScanPendingMax pending items, in name order; the
// rest are counted and left for a later scan.
func TestAScanReadsAtMostTheCapOfPendingItems(t *testing.T) {
	d := openDesk(t)
	for i := range ScanPendingMax + 3 {
		writeFile(t, filepath.Join(d.Path(), DirPending, fmt.Sprintf("%03d-x.sh", i+1)), "true\n", 0o600)
	}
	s := scan(t, d)
	if len(s.Pending) != ScanPendingMax || s.PendingUnread != 3 {
		t.Fatalf("pending %d, unread %d; want %d and 3", len(s.Pending), s.PendingUnread, ScanPendingMax)
	}
	if last := s.Pending[len(s.Pending)-1].Name; last != fmt.Sprintf("%03d-x", ScanPendingMax) {
		t.Errorf("last read %s, want the cap's last in name order", last)
	}
}

// One scan reads at most ScanPendingBytes of pending content.
func TestAScanReadsAtMostTheByteCap(t *testing.T) {
	d := openDesk(t)
	n := ScanPendingBytes/maxItemBytes + 2
	for i := range n {
		writeFile(t, filepath.Join(d.Path(), DirPending, fmt.Sprintf("%02d-big.sh", i+1)), strings.Repeat("#", maxItemBytes), 0o600)
	}
	s := scan(t, d)
	if len(s.Pending) != ScanPendingBytes/maxItemBytes || s.PendingUnread != 2 {
		t.Fatalf("pending %d, unread %d; want %d and 2", len(s.Pending), s.PendingUnread, ScanPendingBytes/maxItemBytes)
	}
}

// PendingItem reads one item again, checks its bytes against the hash the
// caller approves, and changes nothing.
func TestPendingItemChecksTheHashAndMovesNothing(t *testing.T) {
	d := openDesk(t)
	p := filepath.Join(d.Path(), DirPending, "01-a.sh")
	writeFile(t, p, "# WHAT: w\necho a\n", 0o600)
	s := scan(t, d)
	sha := s.Pending[0].Meta.SHA256
	it, err := d.PendingItem("01-a", sha)
	if err != nil || string(it.Content) != "# WHAT: w\necho a\n" || it.What != "w" {
		t.Fatalf("PendingItem = %+v, %v", it, err)
	}
	if _, err := d.PendingItem("01-a", strings.Repeat("0", 64)); !errors.Is(err, ErrChanged) {
		t.Errorf("another hash: err = %v, want ErrChanged", err)
	}
	writeFile(t, p, "echo b\n", 0o600)
	if _, err := d.PendingItem("01-a", sha); !errors.Is(err, ErrChanged) {
		t.Errorf("rewritten bytes: err = %v, want ErrChanged", err)
	}
	if s := scanQuiet(d); s == nil {
		t.Fatal("scan failed")
	}
	if _, err := d.PendingItem("01-a", sha); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the scan moved it: err = %v, want ErrNotFound", err)
	}
}

func scanQuiet(d *Desk) *Snapshot {
	s, err := d.Scan()
	if err != nil {
		return nil
	}
	return s
}
