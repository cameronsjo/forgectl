package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunbooksDirWithLegacyFallsBackOnlyWhenCurrentIsAbsent(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "state", "runbooks")
	legacy := filepath.Join(root, ".claude", "cadence", "runbooks")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := runbooksDirWithLegacy(SyncOptions{
		RunbooksDir:       current,
		LegacyRunbooksDir: legacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != legacy {
		t.Fatalf("absent current corpus should use legacy: got %q, want %q", got, legacy)
	}

	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = runbooksDirWithLegacy(SyncOptions{
		RunbooksDir:       current,
		LegacyRunbooksDir: legacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Fatalf("existing current corpus should win without merging: got %q, want %q", got, current)
	}
}

func TestRunbooksDirWithLegacyReturnsCurrentWhenBothAreAbsent(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "state", "runbooks")

	got, err := runbooksDirWithLegacy(SyncOptions{
		RunbooksDir:       current,
		LegacyRunbooksDir: filepath.Join(root, "legacy"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Fatalf("missing corpora should retain current path: got %q, want %q", got, current)
	}
}
