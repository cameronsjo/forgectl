//go:build unix

package launch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// usageScratch points the whole store at a throwaway state base. No test in
// this file uses t.Parallel: they all swap package-level seams.
func usageScratch(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "state")
	prev := usageBase
	usageBase = func() (string, error) { return base, nil }
	t.Cleanup(func() { usageBase = prev })
	return base
}

func recordOne(t *testing.T, ts string) error {
	t.Helper()
	ev := sampleUsageEvent()
	ev.TS = ts
	return RecordUsage(true, ev)
}

func TestUsageNamespace_CreatesExactModesAndNames(t *testing.T) {
	base := usageScratch(t)
	if err := recordOne(t, "2026-08-13T10:00:00Z"); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	leaf := filepath.Join(base, "forgectl")
	for path, wantMode := range map[string]os.FileMode{
		leaf: 0o700,
		filepath.Join(leaf, "launch-usage.jsonl"):      0o600,
		filepath.Join(leaf, "launch-usage.jsonl.lock"): 0o600,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s mode = %o, want %o", path, got, wantMode)
		}
	}

	// Exactly the two documented files, nothing else.
	entries, err := os.ReadDir(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("leaf holds %d entries, want exactly the data and lock files", len(entries))
	}
}

// Enabling statistics on a machine that has no ~/.local must not be what
// decides ~/.local is private. Only the state base forgectl owns is narrowed;
// directories it merely had to pass through keep the conventional mode.
func TestUsageNamespace_LeavesAncestorsConventional(t *testing.T) {
	// Pinned so the assertion measures the requested mode rather than whatever
	// umask the developer or runner happens to carry.
	prevMask := unix.Umask(0o022)
	t.Cleanup(func() { unix.Umask(prevMask) })

	root := t.TempDir()
	base := filepath.Join(root, "local", "state")
	prev := usageBase
	usageBase = func() (string, error) { return base, nil }
	t.Cleanup(func() { usageBase = prev })

	if err := recordOne(t, "2026-08-13T10:00:00Z"); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	for path, wantMode := range map[string]os.FileMode{
		filepath.Join(root, "local"):    0o755,
		base:                            0o700,
		filepath.Join(base, "forgectl"): 0o700,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Errorf("%s mode = %o, want %o", path, got, wantMode)
		}
	}
}

func TestUsageStore_DisabledCreatesNothing(t *testing.T) {
	base := usageScratch(t)
	if err := RecordUsage(false, sampleUsageEvent()); err != ErrUsageDisabled {
		t.Fatalf("disabled RecordUsage = %v, want ErrUsageDisabled", err)
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Fatalf("disabled collection created the state base: %v", err)
	}

	// An oversized event is refused before any open, too — the bound is an
	// encode-time contract, not a write-time one.
	ev := sampleUsageEvent()
	ev.Model = string(make([]byte, MaxUsageRowBytes))
	if err := RecordUsage(true, ev); err == nil {
		t.Fatal("oversized RecordUsage succeeded")
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Fatalf("a refused event created the state base: %v", err)
	}
}

func TestUsageStore_AbsentStoreReadsEmptyAndCreatesNothing(t *testing.T) {
	base := usageScratch(t)
	agg, err := ReadUsage(nil, time.Now())
	if err != nil {
		t.Fatalf("ReadUsage on an absent store: %v", err)
	}
	if agg.TotalAttempts != 0 || agg.SkippedRows != 0 {
		t.Fatalf("absent store aggregate = %+v, want an empty report", agg)
	}

	status, err := InspectUsage()
	if err != nil {
		t.Fatalf("InspectUsage: %v", err)
	}
	if status.LeafPresent || status.DataPresent || status.LockPresent || status.Refusal != nil {
		t.Fatalf("absent store status = %+v, want everything absent and no refusal", status)
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Fatalf("reading or inspecting created the state base: %v", err)
	}
}

func TestUsageStore_RoundTripsThroughReadUsage(t *testing.T) {
	usageScratch(t)
	if err := recordOne(t, "2026-08-13T10:00:00Z"); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := recordOne(t, "2026-08-13T11:00:00Z"); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	agg, err := ReadUsage(nil, mustTime(t, "2026-08-13T19:00:00Z"))
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if agg.TotalAttempts != 2 || agg.SkippedRows != 0 {
		t.Fatalf("aggregate = %+v, want 2 attempts and no skips", agg)
	}
	if agg.FirstTS == nil || *agg.FirstTS != "2026-08-13T10:00:00Z" {
		t.Fatalf("first_ts = %v", agg.FirstTS)
	}
}
