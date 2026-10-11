//go:build unix

package exec

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestOSRunnerRunsInTheChosenDirectory: the child's working directory is the
// one asked for, and a relative one is refused before anything runs.
func TestOSRunnerRunsInTheChosenDirectory(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := OSRunner{}.RunWithEnvFilteredInDir(t.Context(), dir, nil, nil, "pwd")
	if err != nil {
		t.Fatalf("pwd: %v", err)
	}
	if got, _ := filepath.EvalSymlinks(out); got != dir {
		t.Fatalf("pwd %q, want %q", out, dir)
	}
	if _, err := (OSRunner{}).RunWithEnvFilteredInDir(t.Context(), "relative/dir", nil, nil, "pwd"); !errors.Is(err, ErrRelativeDir) {
		t.Fatalf("a relative directory: %v, want ErrRelativeDir", err)
	}
}
