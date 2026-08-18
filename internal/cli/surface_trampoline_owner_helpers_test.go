//go:build unix

package cli

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// timeT keeps fakeInfo's ModTime signature honest without importing time into
// the test that uses it for nothing else.
type timeT = time.Time

// statWithUID builds the platform stat struct the check reads, with a chosen
// owner. Stat_t.Uid is uint32 on both release targets.
func statWithUID(uid int) *syscall.Stat_t {
	return &syscall.Stat_t{Uid: uint32(uid)} //nolint:gosec // G115: a uid from os.Geteuid is in range
}

// newUnixSocketFile binds a real Unix socket and returns its path.
//
// Real rather than faked: the check reads a mode bit and an owner that only the
// kernel sets, and the accepting half of every test here is the control that
// keeps the refusals meaningful.
func newUnixSocketFile(t *testing.T) string {
	t.Helper()

	// macOS caps sun_path near 104 bytes and t.TempDir() embeds the test name.
	dir, err := os.MkdirTemp("", "so")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "s")
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return path
}
