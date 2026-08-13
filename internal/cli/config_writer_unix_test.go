//go:build unix

package cli

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestNormalWriterBoundaryRefusal_UnixDoesNotIgnoreUnsupportedCapability(t *testing.T) {
	boundary := &config.LegacyMigrationBoundary{Status: config.BoundaryRefused, Refusal: config.ErrLegacyMigrationUnsupported}
	if err := refuseConfigMutationForLegacyBoundary(boundary); !errors.Is(err, config.ErrLegacyMigrationUnsupported) {
		t.Fatalf("unix unsupported refusal=%v, want blocked", err)
	}
}

func TestConfigWriterLock_FIFOConfigRefusesWithoutBlockingOrRendering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	var renders atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := updateConfigLocked(path, nativeConfigWriterOps(), func(raw []byte) ([]byte, error) {
			renders.Add(1)
			return raw, nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("updateConfigLocked accepted a FIFO config")
		}
	case <-time.After(time.Second):
		t.Fatal("updateConfigLocked blocked on a FIFO config")
	}
	if got := renders.Load(); got != 0 {
		t.Fatalf("render callback count = %d, want 0", got)
	}
}
