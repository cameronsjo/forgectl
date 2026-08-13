//go:build unix

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteConfigAtomic_RestrictiveUmaskMayNarrowFreshMode(t *testing.T) {
	if os.Getenv("FORGECTL_TEST_RESTRICTIVE_UMASK") == "1" {
		path := os.Getenv("FORGECTL_TEST_UMASK_PATH")
		syscall.Umask(0o777)
		state, err := writeConfigAtomicWithOps(path, []byte("new"), nativeAtomicWriteOps())
		if err != nil || state != commitDurable {
			t.Fatalf("child state=%v error=%v", state, err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriteConfigAtomic_RestrictiveUmaskMayNarrowFreshMode$")
	cmd.Env = append(os.Environ(), "FORGECTL_TEST_RESTRICTIVE_UMASK=1", "FORGECTL_TEST_UMASK_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("umask subprocess: %v\n%s", err, output)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0 {
		t.Fatalf("mode=%04o, want restrictive umask to narrow fresh mode to 0000", info.Mode().Perm())
	}
}
