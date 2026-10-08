package pr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// storeSnapshot records every entry in dir: name, size, mode, and mtime.
func storeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	var rows []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, e.Name()+"|"+info.Mode().String()+"|"+info.ModTime().Format(time.RFC3339Nano)+"|"+strconv.FormatInt(info.Size(), 10))
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// TestPeekCounts_WritesNothing pins the hub's read: counting a store that
// holds records creates, modifies, and locks nothing — the lifecycle lock
// file included (forgectl#730).
func TestPeekCounts_WritesNothing(t *testing.T) {
	c := testClient(t, ghViewRunner())
	for _, n := range []int{41, 42} {
		if _, err := c.Queue(context.Background(), Ref{Owner: "cameronsjo", Repo: "forgectl", Number: n}, PrepareOpts{Agent: "claude"}); err != nil {
			t.Fatalf("Queue: %v", err)
		}
	}
	// Setup took the lock; remove its file so the peek is shown not to make it.
	if err := os.Remove(filepath.Join(c.SessionsDir(), lifecycleLockName)); err != nil {
		t.Fatalf("remove setup lock: %v", err)
	}
	before := storeSnapshot(t, c.SessionsDir())

	running, queued, err := c.PeekCounts()
	if err != nil || running != 0 || queued != 2 {
		t.Fatalf("PeekCounts = (%d, %d, %v), want (0, 2, nil)", running, queued, err)
	}
	if after := storeSnapshot(t, c.SessionsDir()); after != before {
		t.Errorf("PeekCounts changed the store:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Lstat(filepath.Join(c.SessionsDir(), lifecycleLockName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PeekCounts created the lifecycle lock file (lstat err = %v)", err)
	}
}

// TestPeekCounts_UnavailableRatherThanShort pins the refusals: an absent
// store is not created, and one unreadable record makes the count
// unavailable instead of short.
func TestPeekCounts_UnavailableRatherThanShort(t *testing.T) {
	absent := New(ghViewRunner(), WithSessionsDir(filepath.Join(t.TempDir(), "pr-sessions")))
	if _, _, err := absent.PeekCounts(); !errors.Is(err, ErrPeekUnavailable) {
		t.Errorf("absent store: err = %v, want ErrPeekUnavailable", err)
	}
	if _, err := os.Lstat(absent.SessionsDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("PeekCounts created an absent sessions dir")
	}

	c := testClient(t, ghViewRunner())
	if _, err := c.Queue(context.Background(), Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 7}, PrepareOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.SessionsDir(), "torn.json"), []byte(`{"version":2,`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PeekCounts(); !errors.Is(err, ErrPeekUnavailable) {
		t.Errorf("torn record: err = %v, want ErrPeekUnavailable", err)
	}
}
