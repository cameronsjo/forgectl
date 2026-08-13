//go:build unix

package launch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func dataPath(base string) string { return filepath.Join(base, "forgectl", usageDataName) }

func swapUsageWrite(t *testing.T, fn func(*os.File, []byte) (int, error)) {
	t.Helper()
	prev := usageWrite
	usageWrite = fn
	t.Cleanup(func() { usageWrite = prev })
}

func TestUsageStore_PartialTailIsSeparatedBeforeNextEvent(t *testing.T) {
	base := usageScratch(t)
	leaf := leafFor(t, base)
	// A crash-partial fragment: a row whose single write never completed, so
	// it carries no terminating newline.
	fragment := `{"schema_version":1,"ts":"2026-08-13T09:00:00Z","event":"exec_at`
	if err := os.WriteFile(filepath.Join(leaf, usageDataName), []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recordOne(t, "2026-08-13T10:00:00Z"); err != nil {
		t.Fatalf("RecordUsage after a crash-partial tail: %v", err)
	}

	agg, err := ReadUsage(nil, mustTime(t, "2026-08-13T19:00:00Z"))
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if agg.TotalAttempts != 1 {
		t.Fatalf("total = %d, want the new row to be independently readable", agg.TotalAttempts)
	}
	if agg.SkippedRows != 1 {
		t.Fatalf("skipped = %d, want exactly the isolated fragment", agg.SkippedRows)
	}
	// The fragment must be terminated, not merged into the new row.
	raw, err := os.ReadFile(dataPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), fragment+"\n") {
		t.Fatalf("fragment was not isolated by a newline: %q", raw)
	}
}

func TestUsageStore_ShortRecoveryWriteStopsBeforeEvent(t *testing.T) {
	base := usageScratch(t)
	leaf := leafFor(t, base)
	fragment := `{"schema_version":1,"ts":"2026-08-13T09:00:00Z"`
	if err := os.WriteFile(filepath.Join(leaf, usageDataName), []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}
	swapUsageWrite(t, func(f *os.File, b []byte) (int, error) {
		if len(b) == 1 {
			return 0, nil // the recovery newline lands short
		}
		return f.Write(b)
	})

	if err := recordOne(t, "2026-08-13T10:00:00Z"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("RecordUsage after a short recovery write = %v, want io.ErrShortWrite", err)
	}
	raw, err := os.ReadFile(dataPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != fragment {
		t.Fatalf("log = %q, want the fragment alone — the event must not be appended to it", raw)
	}

	// A later writer with a working write repairs the fragment and lands its
	// own row independently.
	swapUsageWrite(t, func(f *os.File, b []byte) (int, error) { return f.Write(b) })
	if err := recordOne(t, "2026-08-13T11:00:00Z"); err != nil {
		t.Fatalf("RecordUsage after repair: %v", err)
	}
	agg, err := ReadUsage(nil, mustTime(t, "2026-08-13T19:00:00Z"))
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if agg.TotalAttempts != 1 || agg.SkippedRows != 1 {
		t.Fatalf("aggregate = %+v, want one good row and one isolated fragment", agg)
	}
}

func TestUsageStore_ShortEventWriteRecoveredByNextWriter(t *testing.T) {
	usageScratch(t)
	swapUsageWrite(t, func(f *os.File, b []byte) (int, error) {
		if len(b) > 1 {
			short, err := f.Write(b[:len(b)-3]) // the row's tail never lands
			return short, err
		}
		return f.Write(b)
	})
	if err := recordOne(t, "2026-08-13T10:00:00Z"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("RecordUsage with a short event write = %v, want io.ErrShortWrite", err)
	}

	swapUsageWrite(t, func(f *os.File, b []byte) (int, error) { return f.Write(b) })
	if err := recordOne(t, "2026-08-13T11:00:00Z"); err != nil {
		t.Fatalf("RecordUsage after a short predecessor: %v", err)
	}
	agg, err := ReadUsage(nil, mustTime(t, "2026-08-13T19:00:00Z"))
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if agg.TotalAttempts != 1 || agg.SkippedRows != 1 {
		t.Fatalf("aggregate = %+v, want the truncated row isolated and the later row counted", agg)
	}
}

func TestUsageStore_LockBusyReturnsImmediatelyWithoutMutation(t *testing.T) {
	base := usageScratch(t)
	if err := recordOne(t, "2026-08-13T10:00:00Z"); err != nil {
		t.Fatalf("seed RecordUsage: %v", err)
	}
	before, err := os.ReadFile(dataPath(base))
	if err != nil {
		t.Fatal(err)
	}

	release := holdUsageLock(t, filepath.Join(base, "forgectl", usageLockName))
	if err := recordOne(t, "2026-08-13T11:00:00Z"); !errors.Is(err, ErrUsageBusy) {
		t.Fatalf("contended RecordUsage = %v, want ErrUsageBusy", err)
	}
	if _, err := ReadUsage(nil, time.Now()); !errors.Is(err, ErrUsageBusy) {
		t.Fatalf("contended ReadUsage = %v, want ErrUsageBusy", err)
	}
	release()

	after, err := os.ReadFile(dataPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a busy writer still mutated the log:\n%q\nwas\n%q", after, before)
	}
}

func TestUsageStore_ConcurrentWritersOnlyEverWriteCompleteRows(t *testing.T) {
	usageScratch(t)
	const writers = 12
	var wg sync.WaitGroup
	recorded := make([]bool, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := sampleUsageEvent()
			ev.TS = "2026-08-13T10:00:00Z"
			recorded[i] = RecordUsage(true, ev) == nil
		}(i)
	}
	wg.Wait()

	want := 0
	for _, ok := range recorded {
		if ok {
			want++
		}
	}
	agg, err := ReadUsage(nil, mustTime(t, "2026-08-13T19:00:00Z"))
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	// Successful writes, not lock acquisitions, are what a count must equal.
	if agg.TotalAttempts != want {
		t.Fatalf("decoded %d events, want the %d writers that reported success", agg.TotalAttempts, want)
	}
	if agg.SkippedRows != 0 {
		t.Fatalf("skipped = %d, want 0 — concurrent appends must never interleave", agg.SkippedRows)
	}
	if want == 0 {
		t.Fatal("no writer succeeded; the contention test proved nothing")
	}
}

func holdUsageLock(t *testing.T, path string) func() {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := flockExclusive(file); err != nil {
		t.Fatal(err)
	}
	return func() { file.Close() } //nolint:errcheck // closing releases the lock
}
