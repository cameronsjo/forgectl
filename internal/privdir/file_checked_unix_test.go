//go:build unix

package privdir_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func TestOpenFileCheckedRefusesBroadModeWithoutRepair(test *testing.T) {
	directory, path := pinnedDir(test)
	filePath := filepath.Join(path, "state")
	if err := os.WriteFile(filePath, nil, 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.Chmod(filePath, 0o644); err != nil { //nolint:gosec // G302: intentionally public fixture proves checked open does not repair
		test.Fatal(err)
	}
	if _, err := privdir.OpenFileChecked(directory, "state", unix.O_RDONLY, 0o600); !errors.Is(err, privdir.ErrUnsafeFile) {
		test.Fatalf("OpenFileChecked = %v, want ErrUnsafeFile", err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		test.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		test.Fatalf("OpenFileChecked repaired mode to %o", info.Mode().Perm())
	}
}
