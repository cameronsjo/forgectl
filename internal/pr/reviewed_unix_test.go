//go:build unix

package pr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Test plan for LoadReviewed's read (forgectl#765)
//
//   [x] A FIFO at the store path loads as an empty store, fast, instead of
//       blocking the open
//   [x] A store symlinked in (a dotfile manager's layout) still loads
//   [x] A store over the 8 KiB record bound still loads in full
//
// Test plan for persist's write (forgectl#791)
//
//   [x] A FIFO at the store path refuses Mark fast instead of blocking the
//       open, and leaves the FIFO in place
//   [x] The write replaces the store by rename (a new inode, 0600), leaving
//       no temp file behind
//   [x] A symlinked store is written through: the link stays a link and its
//       target gets the marks, including a dangling link's target

// writeReviewedStore writes a store holding n marks and returns the path.
func writeReviewedStore(t *testing.T, dir string, n int) string {
	t.Helper()
	at := make(map[string]time.Time, n)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		at[fmt.Sprintf("github.com/owner/repo#%d", i+1)] = base.Add(time.Duration(i) * time.Minute)
	}
	data, err := json.Marshal(at)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pr-reviewed.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A writer holds the FIFO open without writing, so the pipe never reaches EOF:
// without that, a reader that got past the open would read nothing and the
// test could not tell a skipped FIFO from an empty one.
//
// Mutations that turn it red: in LoadReviewed, read with os.ReadFile(path)
// (the pre-#765 form), or drop readReviewedFile's IsRegular check. Either way
// the read blocks on the idle writer, and mustFailFast fails the test.
func TestLoadReviewed_AFIFOLoadsEmptyAndFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// O_RDWR opens a FIFO without waiting for a reader (Linux and macOS).
	writer, err := os.OpenFile(filepath.Clean(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("hold the FIFO open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	var s *ReviewedStore
	_ = mustFailFast(t, "LoadReviewed on a FIFO", func() error {
		s = LoadReviewed(path)
		return nil
	})
	if len(s.at) != 0 {
		t.Errorf("a FIFO store loaded %d marks, want an empty store", len(s.at))
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the FIFO was disturbed: %v", err)
	}
}

// Mutation that turns it red: open with openNoFollowNonblock (readRecordFile's
// opener) instead of openNonblock. The symlink is refused with ELOOP, and the
// store loads empty.
func TestLoadReviewed_ASymlinkedStoreStillLoads(t *testing.T) {
	target := writeReviewedStore(t, t.TempDir(), 3)
	link := filepath.Join(t.TempDir(), "pr-reviewed.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := len(LoadReviewed(link).at); got != 3 {
		t.Errorf("a symlinked store loaded %d marks, want 3", got)
	}
}

// Mutation that turns it red: read through readBreadcrumbBytes (or
// readRecordFile) instead of io.ReadAll. The 8 KiB record cap refuses the
// store, and it loads empty.
func TestLoadReviewed_AStoreOverTheRecordBoundLoadsInFull(t *testing.T) {
	const n = 400
	path := writeReviewedStore(t, t.TempDir(), n)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= maxBreadcrumbRecordBytes {
		t.Fatalf("fixture is %d bytes; it must exceed the %d-byte record bound to test anything",
			info.Size(), maxBreadcrumbRecordBytes)
	}
	if got := len(LoadReviewed(path).at); got != n {
		t.Errorf("a %d-byte store loaded %d marks, want %d", info.Size(), got, n)
	}
}

// Nobody ever reads the FIFO, so an open for writing would wait forever.
//
// Mutations that turn it red: drop persist's non-regular Lstat refusal (the
// rename replaces the FIFO and Mark succeeds); or restore the pre-#791 form,
// os.WriteFile(s.path, data, 0o600) with no Lstat, whose open blocks for a
// reader until mustFailFast times out.
func TestReviewedPersist_AFIFORefusesFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	fifoAt(t, path)
	s := LoadReviewed(path)

	err := mustFailFast(t, "Mark on a FIFO store", func() error { return s.Mark(testRef(7)) })
	if !errors.Is(err, errRecordNotRegular) {
		t.Fatalf("Mark err = %v, want errRecordNotRegular", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the FIFO is gone: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the FIFO was replaced by a %v", info.Mode())
	}
}

// Mutation that turns it red: write in place with os.WriteFile(dest, data,
// 0o600) instead of writeFileAtomic (the inode survives the write).
func TestReviewedPersist_ReplacesTheStoreByRename(t *testing.T) {
	dir := t.TempDir()
	path := writeReviewedStore(t, dir, 2)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := LoadReviewed(path).Mark(testRef(7)); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the store was rewritten in place; want a new file renamed over it")
	}
	if perm := after.Mode().Perm(); perm != 0o600 {
		t.Errorf("store mode = %v, want 0600", perm)
	}
	if got := len(LoadReviewed(path).at); got != 3 {
		t.Errorf("store holds %d marks after Mark, want 3", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want only the store (no temp file left behind)", names)
	}
}

// Mutation that turns it red: in persist, use s.path as the destination
// instead of resolveStoreTarget(s.path). The Lstat then sees a symlink, which
// is not a regular file, and Mark refuses; renaming over it instead would
// replace the link with a plain file.
func TestReviewedPersist_WritesThroughASymlinkedStore(t *testing.T) {
	cases := map[string]bool{"existing target": true, "dangling link": false}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			targetDir := t.TempDir()
			target := filepath.Join(targetDir, "pr-reviewed.json")
			if existing {
				target = writeReviewedStore(t, targetDir, 2)
			}
			link := filepath.Join(t.TempDir(), "pr-reviewed.json")
			// A relative target, resolved against the link's dir as the kernel does.
			rel, err := filepath.Rel(filepath.Dir(link), target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, link); err != nil {
				t.Fatal(err)
			}
			if err := LoadReviewed(link).Mark(testRef(7)); err != nil {
				t.Fatalf("Mark through a symlinked store: %v", err)
			}
			info, err := os.Lstat(link)
			if err != nil {
				t.Fatalf("the store link is gone: %v", err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the store link was replaced by a %v", info.Mode())
			}
			want := 1
			if existing {
				want = 3
			}
			if got := len(LoadReviewed(target).at); got != want {
				t.Errorf("the link's target holds %d marks, want %d", got, want)
			}
		})
	}
}

// A stow-folded config dir: ~/.config/forgectl is a link to
// ~/dotfiles/forgectl, and the store in it is a relative link whose target
// climbs with "..". The kernel applies that ".." to the PHYSICAL dir, so a
// load reads under ~/dotfiles; persist must write the same file, not the one
// a lexical Clean names under ~/.config. The "through a linked subdir" target
// puts a ".." after a symlinked component inside the link target itself, which
// only a resolver that never Cleans the raw target gets right.
//
// Mutations that turn it red: restore the lexical resolver (Join with
// filepath.Dir(cur), then Clean) (every "parent" case writes the decoy); in
// the dangling walk, skip the EvalSymlinks of the hop's directory (the
// dangling "parent" case); or join the link target with filepath.Join instead
// of appending it raw (the dangling "linked subdir" case).
func TestReviewedPersist_ADotDotLinkUnderASymlinkedDirHitsTheLoadedFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		// target is the store link's text; real and decoy are the physical
		// and the lexical destinations, relative to the home dir.
		target, real, decoy string
	}{
		{"parent, existing", true, "../shared/pr-reviewed.json",
			"dotfiles/shared/pr-reviewed.json", ".config/shared/pr-reviewed.json"},
		{"parent, dangling", false, "../shared/pr-reviewed.json",
			"dotfiles/shared/pr-reviewed.json", ".config/shared/pr-reviewed.json"},
		{"linked subdir, existing", true, "cfg/../shared/pr-reviewed.json",
			"dotfiles/deep/shared/pr-reviewed.json", "dotfiles/forgectl/shared/pr-reviewed.json"},
		{"linked subdir, dangling", false, "cfg/../shared/pr-reviewed.json",
			"dotfiles/deep/shared/pr-reviewed.json", "dotfiles/forgectl/shared/pr-reviewed.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			at := func(rel string) string { return filepath.Join(home, rel) }
			for _, d := range []string{"dotfiles/forgectl", "dotfiles/shared", "dotfiles/forgectl/shared",
				"dotfiles/deep/x", "dotfiles/deep/shared", ".config/shared"} {
				if err := os.MkdirAll(at(d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for link, target := range map[string]string{
				".config/forgectl":                   at("dotfiles/forgectl"),
				"dotfiles/forgectl/cfg":              at("dotfiles/deep/x"),
				"dotfiles/forgectl/pr-reviewed.json": tc.target,
			} {
				if err := os.Symlink(target, at(link)); err != nil {
					t.Fatal(err)
				}
			}
			want := 1
			if tc.existing {
				writeReviewedStore(t, filepath.Dir(at(tc.real)), 2)
				want = 3
			}
			store := at(".config/forgectl/pr-reviewed.json")

			if err := LoadReviewed(store).Mark(testRef(7)); err != nil {
				t.Fatalf("Mark: %v", err)
			}
			if got := len(LoadReviewed(store).at); got != want {
				t.Errorf("a load through the store path sees %d marks after Mark, want %d", got, want)
			}
			if _, err := os.Lstat(at(tc.decoy)); !os.IsNotExist(err) {
				t.Errorf("persist wrote the lexical destination %s (stat err %v)", tc.decoy, err)
			}
			if _, err := os.Stat(at(tc.real)); err != nil {
				t.Errorf("the physical destination %s is missing: %v", tc.real, err)
			}
		})
	}
}

// Mutation that turns it red: drop the syncStoreDir call from persist.
func TestReviewedPersist_SyncsTheDirectoryAfterTheRename(t *testing.T) {
	dir := t.TempDir()
	path := writeReviewedStore(t, dir, 1)
	original := syncStoreDir
	t.Cleanup(func() { syncStoreDir = original })
	var synced []string
	syncStoreDir = func(d string) error {
		synced = append(synced, d)
		if _, err := os.Stat(filepath.Join(d, ".")); err != nil {
			t.Errorf("synced dir %s: %v", d, err)
		}
		return original(d)
	}
	if err := LoadReviewed(path).Mark(testRef(7)); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != want {
		t.Errorf("synced %v, want exactly the store's dir %s", synced, want)
	}
}

// linkChain builds, in dir, the store link plus n-1 more: pr-reviewed.json ->
// l1 -> ... -> l(n-1) -> final.json, n symlinks in all (the review harness's
// "plain chain"). It returns the store path and final.json's path.
func linkChain(t *testing.T, dir string, n int) (store, final string) {
	t.Helper()
	prev := filepath.Join(dir, "pr-reviewed.json")
	for i := 1; i < n; i++ {
		name := fmt.Sprintf("l%d", i)
		if err := os.Symlink(name, prev); err != nil {
			t.Fatal(err)
		}
		prev = filepath.Join(dir, name)
	}
	if err := os.Symlink("final.json", prev); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "pr-reviewed.json"), filepath.Join(dir, "final.json")
}

// The kernel follows at most 40 symlinks in one lookup, so every load of a
// 41-link chain reads ELOOP and loads empty. A Mark that wrote anyway would
// overwrite the store with one entry, and every later load would still read
// nothing: silent loss. It must refuse, and leave the store's bytes alone.
//
// Mutation that turns it red: none alone. The kernel pre-stat and the
// resolver's own 40-link cap both refuse this shape; it pins that the loud
// refusal holds as the resolver changes.
func TestReviewedPersist_AnExistingChainPastTheKernelCapRefuses(t *testing.T) {
	dir := t.TempDir()
	store, final := linkChain(t, dir, 41)
	body := []byte(`{"github.com/owner/repo#1":"2026-09-01T00:00:00Z"}`)
	if err := os.WriteFile(final, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture: the kernel's stat of a 41-link chain = %v, want ELOOP", err)
	}
	if err := LoadReviewed(store).Mark(testRef(7)); err == nil {
		t.Fatal("Mark through a 41-link chain succeeded; every load of it reads ELOOP")
	}
	got, err := os.ReadFile(filepath.Clean(final))
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("the store behind the chain changed: %q, %v", got, err)
	}
}

// Each of 21 hops goes through a directory link back to r itself, so the
// kernel counts 42 links and a load reads ELOOP, while the resolver's
// final-link count is only 21. The kernel pre-stat refuses before anything
// is written.
//
// Mutation that turns it red: drop persist's kernel pre-stat. The resolver
// then writes r/m21, and although the post-write stat still refuses, the
// write already happened.
func TestReviewedPersist_ADanglingChainThroughDirLinksPastTheCapRefusesUnwritten(t *testing.T) {
	r := t.TempDir()
	const n = 21
	for i := 1; i <= n; i++ {
		if err := os.Symlink(r, filepath.Join(r, fmt.Sprintf("d%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(r, "pr-reviewed.json")
	if err := os.Symlink("d1/m1", store); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		if err := os.Symlink(fmt.Sprintf("d%d/m%d", i+1, i+1), filepath.Join(r, fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(store); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture: the kernel's stat of a %d-link lookup = %v, want ELOOP", 2*n, err)
	}
	before, err := os.ReadDir(r)
	if err != nil {
		t.Fatal(err)
	}

	if err := LoadReviewed(store).Mark(testRef(7)); err == nil {
		t.Fatal("Mark through a chain the kernel cannot resolve succeeded")
	}
	after, err := os.ReadDir(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the refused Mark wrote into the store's dir: %d entries before, %d after", len(before), len(after))
	}
}

// A dangling chain of exactly 40 links is inside the kernel's cap: its lookup
// reaches the missing final name, so a write there is what every later load
// reads. It must write, and load back.
//
// Mutation that turns it red: cap resolveStoreTarget's walk at
// maxStoreLinkHops iterations instead of maxStoreLinkHops+1 (the 40th link's
// target is never looked up, and Mark refuses a store the kernel resolves).
func TestReviewedPersist_ADanglingChainAtTheKernelCapWritesAndLoadsBack(t *testing.T) {
	store, final := linkChain(t, t.TempDir(), 40)
	if _, err := os.Stat(store); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fixture: the kernel's stat of a dangling 40-link chain = %v, want ENOENT", err)
	}
	if err := LoadReviewed(store).Mark(testRef(7)); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if got := len(LoadReviewed(store).at); got != 1 {
		t.Errorf("a load through the chain sees %d marks, want 1", got)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("the chain's final target was not written: %v", err)
	}
}

// A resolver that disagrees with the kernel, simulated through the
// resolveStore seam, writes a file the store path does not name. The
// post-write kernel stat must catch it and refuse, rather than report a mark
// no load will see.
//
// Mutation that turns it red: drop persist's post-write os.Stat/SameFile
// check.
func TestReviewedPersist_AResolverDivergenceIsReportedNotClaimed(t *testing.T) {
	dir := t.TempDir()
	store := writeReviewedStore(t, dir, 1)
	original := resolveStore
	t.Cleanup(func() { resolveStore = original })
	resolveStore = func(string) (string, error) { return filepath.Join(dir, "elsewhere.json"), nil }

	err := LoadReviewed(store).Mark(testRef(7))
	if !errors.Is(err, errStoreNotWritten) {
		t.Fatalf("Mark err = %v, want errStoreNotWritten", err)
	}
}

// The rename is the commit point: a directory fsync that fails after it must
// not turn a mark that is on disk into a reported failure.
//
// Mutation that turns it red: return the syncStoreDir error from persist
// again (Mark fails although the mark is written).
func TestReviewedPersist_AFailedDirSyncStillMarksWithAWarning(t *testing.T) {
	dir := t.TempDir()
	store := writeReviewedStore(t, dir, 1)
	original := syncStoreDir
	t.Cleanup(func() { syncStoreDir = original })
	syncStoreDir = func(string) error { return errors.New("injected dir fsync failure") }
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := LoadReviewed(store).Mark(testRef(7)); err != nil {
		t.Fatalf("Mark = %v, want success: the mark is on disk", err)
	}
	if got := len(LoadReviewed(store).at); got != 2 {
		t.Errorf("store holds %d marks, want 2", got)
	}
	out := logs.String()
	if !strings.Contains(out, "durability could not be confirmed") || !strings.Contains(out, "injected dir fsync failure") {
		t.Errorf("no durability warning logged; logs:\n%s", out)
	}
	if strings.Contains(out, "Failed to mark reviewed") {
		t.Errorf("the mark was logged as failed; logs:\n%s", out)
	}
}
