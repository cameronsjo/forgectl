//go:build !unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestMigrationOther_RefusesMutationBeforeEveryWriterOperationButAllowsReadOnlyFallback(t *testing.T) {
	base := t.TempDir()
	legacyPath := filepath.Join(base, "claunch", "claunch.conf")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("[defaults]\nmodel = \"sonnet\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{Home: base, XDGConfigHome: base, UserConfigHome: base}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close() //nolint:errcheck
	if boundary.Status != config.BoundaryRefused || !errors.Is(boundary.Refusal, config.ErrLegacyMigrationUnsupported) {
		t.Fatalf("boundary status=%v refusal=%v", boundary.Status, boundary.Refusal)
	}
	fallback, err := boundary.LoadReadOnlyLegacy()
	if err != nil || fallback.Defaults.Model != "sonnet" {
		t.Fatalf("fallback=%+v error=%v", fallback, err)
	}

	calls := 0
	ops := nativeMigrationTxnOps()
	ops.ensureParent = func(string, configParentOps) ([]string, error) { calls++; return nil, nil }
	ops.withLock = func(string, func() error) error { calls++; return nil }
	ops.writeConfig = func(string, []byte, atomicWriteOps) (commitState, error) { calls++; return commitNone, nil }
	ops.allocateBackup = func(*config.LegacySnapshot) (*config.BackupAllocation, error) { calls++; return nil, nil }
	for name, result := range map[string]MigrationResult{
		"automatic": migrateLegacyAutomatically(boundary, config.Config{}, ops),
		"explicit":  migrateLegacyExplicit(boundary, ops),
	} {
		if !errors.Is(result.Err, config.ErrLegacyMigrationUnsupported) {
			t.Errorf("%s error=%v, want unsupported", name, result.Err)
		}
	}
	if calls != 0 {
		t.Fatalf("writer operation calls=%d, want zero", calls)
	}
}
