package clean

// Test plan for cache.go
//
// Client.ScanCaches (Classification: concurrent-free scan over a fake
// runner, real temp-dir fixture for the size measurement)
//   [x] Happy: a detected tool's locate output resolves to a real dir, whose
//       size is reported via dirSize
//   [x] Unhappy: a tool whose locate command errors is Skipped, with a
//       reason, and its Size/Path stay zero/empty
//   [x] Unhappy: a tool whose locate command succeeds but prints nothing
//       (blank/whitespace) is also Skipped
//   [x] Happy: kinds filters which probes run; empty runs every probe
//
// Client.PruneCaches (Classification: I/O boundary, argv assertions via
// FakeRunner)
//   [x] Happy: each tool's prune command matches its documented argv exactly
//       (brew: plain "brew cleanup", no -s)
//   [x] Invariant: a Skipped item is never attempted — no Runner call for it
//   [x] Invariant: one tool's prune failing does not prevent the others from
//       being attempted and marked Applied (isolation)
//   [x] Happy: Apply never touches ScanCaches' own detection — PruneCaches
//       operates only on the items list handed to it
//   [x] Happy: Reclaimed is the MEASURED post-prune delta (dirSize before
//       vs after), not the pre-prune Size estimate
//   [x] Boundary: Reclaimed is floored at zero when the cache doesn't shrink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// cacheLocateFunc serves a canned locate response per tool name — the first
// arg to Run (npm/pnpm/pip/go/brew) — and errors for the rest, mirroring
// clean_test.go's statusByDir keyed-fake pattern.
func cacheLocateFunc(dirs map[string]string, failing map[string]bool) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		if failing[name] {
			return "", errors.New(name + ": command not found")
		}
		if dir, ok := dirs[name]; ok {
			return dir, nil
		}
		return "", nil
	}
}

func TestScanCaches_DetectedToolSized(t *testing.T) {
	npmCache := t.TempDir()
	if err := os.WriteFile(filepath.Join(npmCache, "blob"), make([]byte, 500), 0o644); err != nil {
		t.Fatalf("seed npm cache fixture: %v", err)
	}

	fake := &exec.FakeRunner{RunFunc: cacheLocateFunc(map[string]string{"npm": npmCache}, nil)}
	client := New(fake)

	items := client.ScanCaches(context.Background(), []CacheKind{CacheNpm})
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(items), items)
	}
	item := items[0]
	if item.Skipped {
		t.Fatalf("npm cache should be detected, got skipped: %+v", item)
	}
	if item.Path != npmCache {
		t.Errorf("Path = %q, want %q", item.Path, npmCache)
	}
	if item.Size != 500 {
		t.Errorf("Size = %d, want 500", item.Size)
	}
}

func TestScanCaches_UndetectedToolSkipped(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: cacheLocateFunc(nil, map[string]bool{"pnpm": true})}
	client := New(fake)

	items := client.ScanCaches(context.Background(), []CacheKind{CachePnpm})
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(items), items)
	}
	if !items[0].Skipped || items[0].SkipReason == "" {
		t.Errorf("undetected tool must be skipped with a reason: %+v", items[0])
	}
	if items[0].Path != "" || items[0].Size != 0 {
		t.Errorf("skipped item must have zero Path/Size: %+v", items[0])
	}
}

func TestScanCaches_BlankLocateOutputSkipped(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: cacheLocateFunc(map[string]string{"pip": "   "}, nil)}
	client := New(fake)

	items := client.ScanCaches(context.Background(), []CacheKind{CachePip})
	if len(items) != 1 || !items[0].Skipped {
		t.Errorf("blank locate output must be skipped: %+v", items)
	}
}

func TestScanCaches_KindsFilter(t *testing.T) {
	dirs := map[string]string{"npm": t.TempDir(), "pnpm": t.TempDir(), "pip": t.TempDir(), "go": t.TempDir(), "brew": t.TempDir()}
	fake := &exec.FakeRunner{RunFunc: cacheLocateFunc(dirs, nil)}
	client := New(fake)

	all := client.ScanCaches(context.Background(), nil)
	if len(all) != 5 {
		t.Fatalf("empty filter: got %d items, want 5 (all probes): %+v", len(all), all)
	}

	filtered := client.ScanCaches(context.Background(), []CacheKind{CacheGo, CacheBrew})
	if len(filtered) != 2 {
		t.Fatalf("filtered: got %d items, want 2", len(filtered))
	}
	for _, it := range filtered {
		if it.Kind != CacheGo && it.Kind != CacheBrew {
			t.Errorf("filter leaked an unfiltered kind: %+v", it)
		}
	}
}

func TestPruneCaches_ArgvMatchesDocumentedCommands(t *testing.T) {
	fake := &exec.FakeRunner{}
	client := New(fake)

	items := []CacheItem{
		{Kind: CacheNpm, Path: "/npm"},
		{Kind: CachePnpm, Path: "/pnpm"},
		{Kind: CachePip, Path: "/pip"},
		{Kind: CacheGo, Path: "/go"},
		{Kind: CacheBrew, Path: "/brew"},
	}
	result := client.PruneCaches(context.Background(), items)
	for _, item := range result {
		if item.Err != nil || !item.Applied {
			t.Errorf("%s: expected a successful prune, got %+v", item.Kind, item)
		}
	}

	want := map[CacheKind]string{
		CacheNpm:  "npm cache clean --force",
		CachePnpm: "pnpm store prune",
		CachePip:  "pip cache purge",
		CacheGo:   "go clean -cache",
		// No -s (scrub): the fix round dropped it — see the doc comment on
		// the brew cacheProbe. Note `brew cleanup` (with or without -s)
		// still touches the Cellar, not just the cache; that's disclosed
		// at the CLI layer (cacheDisplayName), not narrowed here.
		CacheBrew: "brew cleanup",
	}
	if len(fake.Calls) != len(want) {
		t.Fatalf("got %d Runner calls, want %d: %+v", len(fake.Calls), len(want), fake.Calls)
	}
	for _, call := range fake.Calls {
		argv := call.Name + " " + strings.Join(call.Args, " ")
		matched := false
		for _, w := range want {
			if argv == w {
				matched = true
			}
		}
		if !matched {
			t.Errorf("unexpected argv: %q", argv)
		}
	}
}

func TestPruneCaches_SkippedItemNeverAttempted(t *testing.T) {
	fake := &exec.FakeRunner{}
	client := New(fake)

	items := []CacheItem{{Kind: CacheNpm, Skipped: true, SkipReason: "not detected"}}
	result := client.PruneCaches(context.Background(), items)
	if len(fake.Calls) != 0 {
		t.Errorf("a skipped item must never reach the Runner; saw %d calls: %+v", len(fake.Calls), fake.Calls)
	}
	if result[0].Applied || result[0].Err != nil {
		t.Errorf("skipped item must stay untouched: %+v", result[0])
	}
}

func TestPruneCaches_OneFailureDoesNotBlockTheOthers(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "npm" {
			return "", errors.New("npm: EACCES")
		}
		return "", nil
	}}
	client := New(fake)

	items := []CacheItem{
		{Kind: CacheNpm, Path: "/npm"},
		{Kind: CachePip, Path: "/pip"},
	}
	result := client.PruneCaches(context.Background(), items)

	byKind := map[CacheKind]CacheItem{}
	for _, it := range result {
		byKind[it.Kind] = it
	}
	if byKind[CacheNpm].Err == nil || byKind[CacheNpm].Applied {
		t.Errorf("npm should have failed, not applied: %+v", byKind[CacheNpm])
	}
	if byKind[CachePip].Err != nil || !byKind[CachePip].Applied {
		t.Errorf("pip's prune must still run despite npm's failure (isolation): %+v", byKind[CachePip])
	}
}

// TestPruneCaches_ReclaimedIsMeasuredDelta pins that Reclaimed comes from a
// REAL post-prune dirSize measurement, not the pre-prune Size estimate —
// the fake prune actually deletes a file in the fixture cache dir (via
// os.Remove inside RunFunc), so a bug that reported the pre-prune Size
// verbatim would be caught: it would report 1000, not the true 400 freed.
func TestPruneCaches_ReclaimedIsMeasuredDelta(t *testing.T) {
	cacheDir := t.TempDir()
	keep := filepath.Join(cacheDir, "keep.bin")
	freed := filepath.Join(cacheDir, "freed.bin")
	if err := os.WriteFile(keep, make([]byte, 600), 0o644); err != nil {
		t.Fatalf("seed keep.bin: %v", err)
	}
	if err := os.WriteFile(freed, make([]byte, 400), 0o644); err != nil {
		t.Fatalf("seed freed.bin: %v", err)
	}

	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "npm" {
			return "", os.Remove(freed)
		}
		return "", nil
	}}
	client := New(fake)

	items := []CacheItem{{Kind: CacheNpm, Path: cacheDir, Size: 1000}}
	result := client.PruneCaches(context.Background(), items)
	if !result[0].Applied {
		t.Fatalf("expected a successful prune, got %+v", result[0])
	}
	if result[0].Reclaimed != 400 {
		t.Errorf("Reclaimed = %d, want 400 (the measured post-prune delta, not the pre-prune Size of 1000)", result[0].Reclaimed)
	}
}

// TestPruneCaches_ReclaimedFlooredAtZero pins the non-negative floor: a
// prune that succeeds without shrinking the directory must report 0
// reclaimed, never a negative number.
func TestPruneCaches_ReclaimedFlooredAtZero(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "blob"), make([]byte, 500), 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	fake := &exec.FakeRunner{} // prune "succeeds" but touches nothing on disk
	client := New(fake)

	items := []CacheItem{{Kind: CacheNpm, Path: cacheDir, Size: 500}}
	result := client.PruneCaches(context.Background(), items)
	if !result[0].Applied {
		t.Fatalf("expected a successful prune, got %+v", result[0])
	}
	if result[0].Reclaimed != 0 {
		t.Errorf("Reclaimed = %d, want 0 when the cache directory didn't shrink", result[0].Reclaimed)
	}
}
