//go:build unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/resume"
)

// I-4: a second run while another holds the lock exits without acting. The
// holder is a plain flock on the same file, standing in for a live run.
func TestResumeRestart_SecondConcurrentRunIsRefused(t *testing.T) {
	env := &cliRestartEnv{}
	store := restartFixture(t, env)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(store, "restart.lock"), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- a fixed name under this test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := runRestartCmd(t, "--outdated"); !errors.Is(err, resume.ErrRestartBusy) || env.terminated != 0 {
		t.Fatalf("err=%v terminated=%d; want ErrRestartBusy and no action", err, env.terminated)
	}
}
