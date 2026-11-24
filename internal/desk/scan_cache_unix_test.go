//go:build unix

package desk

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestScanCachesDoneAndSkipped(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close() //nolint:errcheck // test cleanup

	// Seed a done script item with meta + log + script, a legacy done log, and a skipped item.
	started := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(5 * time.Second)
	rc0 := 0
	if err := d.writeMeta(DirDone, "01-alpha", Meta{
		Kind:      KindScript,
		StartedAt: &started,
		EndedAt:   &ended,
		ExitCode:  &rc0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirDone, "01-alpha.sh"), []byte("#!/bin/bash\n# WHAT: alpha task\n# WHY: test\n# AFTER: 00-prep\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirDone, "01-alpha.log"), []byte("hi\nEXIT=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirDone, "legacy-old.log"), []byte("old\nEXIT=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.writeMeta(DirSkipped, "02-beta", Meta{
		Kind:       KindScript,
		SkipReason: SkipOperator,
		SkipNote:   "not needed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirSkipped, "02-beta.sh"), []byte("#!/bin/bash\n# WHAT: beta task\n# WHY: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	reads := map[scanCacheKey]int{}
	prevHook := onScanCacheRead
	onScanCacheRead = func(sub, name string) {
		mu.Lock()
		reads[scanCacheKey{dir: sub, name: name}]++
		mu.Unlock()
	}
	t.Cleanup(func() { onScanCacheRead = prevHook })

	resetReads := func() {
		mu.Lock()
		reads = map[scanCacheKey]int{}
		mu.Unlock()
	}

	// First scan: populates cache for all 3 items.
	snap1, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan 1: %v", err)
	}
	if len(snap1.Done) != 2 || len(snap1.Skipped) != 1 {
		t.Fatalf("Scan 1 counts: done=%d skipped=%d, want 2 and 1", len(snap1.Done), len(snap1.Skipped))
	}
	if len(reads) != 3 {
		t.Fatalf("Scan 1 reads = %v, want 3 items read", reads)
	}

	// Second scan: nothing changed -> zero disk reads for done/ and skipped/, identical snapshots.
	resetReads()
	snap2, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan 2: %v", err)
	}
	if len(reads) != 0 {
		t.Fatalf("Scan 2 re-read unchanged items: %v", reads)
	}
	if !reflect.DeepEqual(snap1.Done, snap2.Done) {
		t.Fatalf("Scan 2 Done mismatch:\ngot:  %+v\nwant: %+v", snap2.Done, snap1.Done)
	}
	if !reflect.DeepEqual(snap1.Skipped, snap2.Skipped) {
		t.Fatalf("Scan 2 Skipped mismatch:\ngot:  %+v\nwant: %+v", snap2.Skipped, snap1.Skipped)
	}

	// Mutating a returned Snapshot must not corrupt cached entries.
	for i := range snap2.Done {
		if snap2.Done[i].Name == "01-alpha" {
			snap2.Done[i].After[0] = "mutated"
			*snap2.Done[i].ExitCode = 99
		}
	}
	resetReads()
	snap3, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan 3: %v", err)
	}
	if len(reads) != 0 {
		t.Fatalf("Scan 3 re-read unchanged items: %v", reads)
	}
	if !reflect.DeepEqual(snap1.Done, snap3.Done) {
		t.Fatalf("cache was corrupted by caller mutation:\ngot:  %+v\nwant: %+v", snap3.Done, snap1.Done)
	}

	// Adding a new done item reads only the new item.
	if err := os.WriteFile(filepath.Join(dir, DirDone, "03-gamma.log"), []byte("gamma\nEXIT=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetReads()
	snap4, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan 4: %v", err)
	}
	if len(snap4.Done) != 3 {
		t.Fatalf("Scan 4 done = %d, want 3", len(snap4.Done))
	}
	wantReads := map[scanCacheKey]int{{dir: DirDone, name: "03-gamma"}: 1}
	if !reflect.DeepEqual(reads, wantReads) {
		t.Fatalf("Scan 4 reads = %v, want %v", reads, wantReads)
	}

	// Modifying an existing skipped item's meta invalidates only that item.
	if err := d.writeMeta(DirSkipped, "02-beta", Meta{
		Kind:       KindScript,
		SkipReason: SkipOperator,
		SkipNote:   "updated note",
	}); err != nil {
		t.Fatal(err)
	}
	resetReads()
	snap5, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan 5: %v", err)
	}
	wantReads = map[scanCacheKey]int{{dir: DirSkipped, name: "02-beta"}: 1}
	if !reflect.DeepEqual(reads, wantReads) {
		t.Fatalf("Scan 5 reads = %v, want %v", reads, wantReads)
	}
	if snap5.Skipped[0].Meta.SkipNote != "updated note" {
		t.Fatalf("Scan 5 SkipNote = %q, want %q", snap5.Skipped[0].Meta.SkipNote, "updated note")
	}

	// Removing items evicts them from scanCache.
	if err := os.Remove(filepath.Join(dir, DirDone, "03-gamma.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, DirSkipped, "02-beta.sh")); err != nil {
		t.Fatal(err)
	}
	resetReads()
	snap6, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan 6: %v", err)
	}
	if len(snap6.Done) != 2 || len(snap6.Skipped) != 0 {
		t.Fatalf("Scan 6 counts: done=%d skipped=%d, want 2 and 0", len(snap6.Done), len(snap6.Skipped))
	}
	if len(reads) != 0 {
		t.Fatalf("Scan 6 re-read surviving items: %v", reads)
	}
	d.scanMu.Lock()
	_, gammaCached := d.scanCache[scanCacheKey{dir: DirDone, name: "03-gamma"}]
	_, betaCached := d.scanCache[scanCacheKey{dir: DirSkipped, name: "02-beta"}]
	cacheLen := len(d.scanCache)
	d.scanMu.Unlock()
	if gammaCached || betaCached || cacheLen != 2 {
		t.Fatalf("eviction failed: gamma=%v beta=%v len=%d, want false, false, 2", gammaCached, betaCached, cacheLen)
	}
}

func TestScanCacheConcurrent(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close() //nolint:errcheck // test cleanup

	for i := range 10 {
		name := fmt.Sprintf("%02d-job", i+1)
		if err := os.WriteFile(filepath.Join(dir, DirDone, name+".log"), []byte("ok\nEXIT=0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, DirDone, name+".sh"), []byte("#!/bin/bash\n# WHAT: job\n# WHY: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				snap, err := d.Scan()
				if err != nil {
					t.Errorf("Scan: %v", err)
					return
				}
				if len(snap.Done) != 10 {
					t.Errorf("Done = %d, want 10", len(snap.Done))
					return
				}
			}
		})
	}
	wg.Wait()
}

func BenchmarkDeskScanCached(b *testing.B) {
	dir := b.TempDir()
	d, err := Open(dir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer d.Close() //nolint:errcheck // benchmark cleanup

	now := time.Now().UTC()
	rc := 0
	for i := range 50 {
		name := fmt.Sprintf("%02d-done", i+1)
		_ = d.writeMeta(DirDone, name, Meta{Kind: KindScript, StartedAt: &now, EndedAt: &now, ExitCode: &rc})
		_ = os.WriteFile(filepath.Join(dir, DirDone, name+".sh"), []byte("#!/bin/bash\n# WHAT: done task\n# WHY: bench\n"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, DirDone, name+".log"), []byte("done\nEXIT=0\n"), 0o600)
	}
	for i := range 20 {
		name := fmt.Sprintf("%02d-skip", i+1)
		_ = d.writeMeta(DirSkipped, name, Meta{Kind: KindScript, SkipReason: SkipOperator})
		_ = os.WriteFile(filepath.Join(dir, DirSkipped, name+".sh"), []byte("#!/bin/bash\n# WHAT: skipped task\n# WHY: bench\n"), 0o600)
	}

	// Warm the cache.
	if _, err := d.Scan(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.Scan(); err != nil {
			b.Fatal(err)
		}
	}
}
