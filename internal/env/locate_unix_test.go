//go:build unix

package env

// locate_test.go's FIFO case, split out because syscall.Mkfifo exists only
// on unix: in locate_test.go it kept `GOOS=windows go vet ./internal/env/`
// from compiling the package's tests (#803).

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocate_ExistingFIFO_Refused(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	fifoPath := filepath.Join(root, ".env")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo unsupported in this environment: %v", err)
	}

	// A FIFO with no writer would block os.Open/parseFile forever — resolution
	// must refuse it before any caller ever opens it.
	_, err := locate(".env", root)
	if err == nil {
		t.Fatal("locate against a FIFO target returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want it to name the regular-file rule", err.Error())
	}
}
