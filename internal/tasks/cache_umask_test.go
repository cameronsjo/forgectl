//go:build unix

package tasks

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The cache must stay readable by its owner whatever the umask. CreateTemp
// asks for 0600 and a umask can take owner-read away from that, which would
// leave a cache SaveCache reports as written and LoadCache cannot read.
//
// Not parallel: the umask is process-wide.
func TestSaveCache_IsOwnerReadableUnderARestrictiveUmask(t *testing.T) {
	// The directory is made before the umask changes, or TempDir itself
	// could not create it.
	path := filepath.Join(t.TempDir(), "tasks-cache.json")
	old := syscall.Umask(0o277)
	t.Cleanup(func() { syscall.Umask(old) })

	if err := SaveCache(path, Snapshot{Tasks: []Task{{ID: 1, Title: "a"}}}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("cache mode = %04o, want 0600", mode)
	}
	if _, err := LoadCache(path); err != nil {
		t.Errorf("LoadCache after a save under umask 0277: %v", err)
	}
}
