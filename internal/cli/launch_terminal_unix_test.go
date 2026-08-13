//go:build unix

package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestLaunchDoctor_ControlRefusalNeverParsesPotentiallyBlockingConfigPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "control\nconfig")
	legacyPath := filepath.Join(base, "claunch", "claunch.conf")
	configPath := filepath.Join(base, "forgectl", "config.toml")
	for _, dir := range []string{filepath.Dir(legacyPath), filepath.Dir(configPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := "[defaults]\nbinary_path = \"/bin/true\"\n"
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{Home: t.TempDir(), XDGConfigHome: base, UserConfigHome: base}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close() //nolint:errcheck
	if boundary.Status != config.BoundaryRefused || !errors.Is(boundary.Refusal, config.ErrLegacyPathControl) {
		t.Fatalf("status/refusal=%v/%v", boundary.Status, boundary.Refusal)
	}

	cmd := newLaunchDoctorCmd(boundary, config.Config{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("doctor blocked opening the refused config FIFO")
	}
}
