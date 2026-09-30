package docs

// Test plan for walkRoot's held-directory walk (forgectl#743)
//   [x] Security: a swap-race stress run (files and a directory swapped
//       between real entries and symlinks to outside or to another in-root
//       doc) never indexes outside content or another file's content under
//       a doc's name
//   [x] Security: a doc swapped for an outside symlink between the walk's
//       Lstat and its open is not read (deterministic, through the seam)
//   [x] Unhappy: a doc maxHeldDirs directories deep is indexed and opens; a
//       directory past that is recorded as skipped, not descended
//   [x] Happy: the walk lists docs in lexical path order, as
//       filepath.WalkDir did (index_order_test.go covers the recency sort)

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// swapper repeatedly renames a regular file and a symlink over path, until
// stop is closed. Each swap is an atomic rename, so path always names one or
// the other.
func swapper(t *testing.T, stop <-chan struct{}, wg *sync.WaitGroup, path, regularBody, linkTarget string, dir bool) {
	t.Helper()
	tmpReal := path + ".real"
	tmpLink := path + ".link"
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.RemoveAll(tmpLink)
			if err := os.Symlink(linkTarget, tmpLink); err == nil {
				_ = os.RemoveAll(path)
				_ = os.Rename(tmpLink, path)
			}
			_ = os.RemoveAll(tmpReal)
			var err error
			if dir {
				if err = os.Mkdir(tmpReal, 0o750); err == nil {
					err = os.WriteFile(filepath.Join(tmpReal, "page.md"), []byte(regularBody), 0o600)
				}
			} else {
				err = os.WriteFile(tmpReal, []byte(regularBody), 0o600)
			}
			if err == nil {
				_ = os.RemoveAll(path)
				_ = os.Rename(tmpReal, path)
			}
		}
	}()
}

func TestWalkRoot_SwapRaceNeverIndexesOutsideOrOtherFileContent(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	base := mustCanonicalRoot(t, t.TempDir())
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(outside, "secret.md"), "# OUTSIDE\n")
	writeFile(t, filepath.Join(outside, "page.md"), "# OUTSIDE\n")
	writeFile(t, filepath.Join(root, "b.md"), "# B\n")
	writeFile(t, filepath.Join(root, "a.md"), "# A\n")
	writeFile(t, filepath.Join(root, "c.md"), "# C\n")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "sub", "page.md"), "# SUB\n")
	if err := os.Symlink(outside, filepath.Join(base, "probe")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	swapper(t, stop, &wg, filepath.Join(root, "a.md"), "# A\n", filepath.Join(outside, "secret.md"), false)
	swapper(t, stop, &wg, filepath.Join(root, "c.md"), "# C\n", "b.md", false)
	swapper(t, stop, &wg, filepath.Join(root, "sub"), "# SUB\n", outside, true)
	defer func() {
		close(stop)
		wg.Wait()
	}()

	// Each doc's title must be its own content or its filename fallback.
	allowed := map[string][]string{
		"a.md":        {"A", "a"},
		"b.md":        {"B", "b"},
		"c.md":        {"C", "c"},
		"sub/page.md": {"SUB", "page"},
	}
	var builds, docsSeen atomic.Int64
	for range 400 {
		idx, err := NewIndex([]string{root})
		if err != nil {
			continue // a build can fail outright mid-swap; that is not a leak
		}
		builds.Add(1)
		for _, d := range idx.List() {
			docsSeen.Add(1)
			want, ok := allowed[d.RelPath]
			if !ok {
				if strings.Contains(d.RelPath, ".real") || strings.Contains(d.RelPath, ".link") {
					// The swapper's staging names: sub.real is a real
					// in-root directory; only its content is checked.
					if d.Title == "OUTSIDE" {
						t.Fatalf("staged doc %q indexed with outside content", d.RelPath)
					}
					continue
				}
				t.Fatalf("indexed unexpected doc %q (title %q)", d.RelPath, d.Title)
			}
			if d.Title != want[0] && d.Title != want[1] {
				t.Fatalf("doc %q indexed with title %q, want %q: content from another file", d.RelPath, d.Title, want)
			}
			if !strings.HasPrefix(d.AbsPath, root+string(filepath.Separator)) {
				t.Fatalf("doc %q indexed at %q, outside the root", d.RelPath, d.AbsPath)
			}
		}
	}
	if builds.Load() == 0 || docsSeen.Load() == 0 {
		t.Fatalf("stress run built %d indexes holding %d docs; it exercised nothing", builds.Load(), docsSeen.Load())
	}
}

// swapAfterInfo is a DirEntry whose Info returns the real Lstat and then
// runs swap, landing the swap exactly between the walk's stat and its open.
type swapAfterInfo struct {
	os.DirEntry
	swap func()
}

func (e swapAfterInfo) Info() (os.FileInfo, error) {
	info, err := e.DirEntry.Info()
	e.swap()
	return info, err
}

func TestWalkRoot_DocSwappedBetweenStatAndOpenIsNotRead(t *testing.T) {
	base := mustCanonicalRoot(t, t.TempDir())
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "secret.md")
	writeFile(t, secret, "# OUTSIDE\n")
	doc := filepath.Join(root, "a.md")
	writeFile(t, doc, "# A\n")
	swap := func() {
		if err := os.Remove(doc); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(secret, doc); err != nil {
			t.Error(err)
		}
	}
	withWalk(t, func(rt *os.Root, rootPath string, fn walkFunc) error {
		return walkHeld(rt, rootPath, func(path string, dir *os.Root, d os.DirEntry, err error) error {
			if err == nil && path == doc {
				d = swapAfterInfo{DirEntry: d, swap: swap}
			}
			return fn(path, dir, d, err)
		})
	})
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	docs := idx.List()
	if len(docs) != 1 || docs[0].Title == "OUTSIDE" {
		t.Errorf("indexed %+v, want a.md without the outside file's content", docs)
	}
}

func TestWalkRoot_DepthCapMatchesResolution(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	atCap := "ok/" + nestedDoc(t, filepath.Join(root, "ok"), maxHeldDirs-1)
	past := "deep/" + nestedDoc(t, filepath.Join(root, "deep"), maxHeldDirs)
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label
	if _, ok := idx.Find(label, atCap); !ok {
		t.Fatalf("a doc %d directories deep was not indexed", maxHeldDirs)
	}
	f, _, err := idx.Open(label, atCap)
	if err != nil {
		t.Fatalf("the indexed doc %d directories deep does not open: %v", maxHeldDirs, err)
	}
	_ = f.Close()
	if d, ok := idx.Find(label, past); ok {
		t.Errorf("a doc %d directories deep was indexed (%q), want its directory skipped", maxHeldDirs+1, d.RelPath)
	}
	skipped := idx.Skipped()
	if len(skipped) != 1 || !strings.HasPrefix(past, skipped[0].Rel+"/") || skipped[0].Reason != errTooDeep.Error() {
		t.Errorf("Skipped() = %+v, want the one directory past the cap, reason %q", skipped, errTooDeep)
	}
}

func TestWalkHeld_VisitsInLexicalOrder(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	for _, f := range []string{"b.md", "a/z.md", "a.md", "A.md", "a/b/c.md"} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, "# x\n")
	}
	var fromRoot, fromWalkDir []string
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	if err := walkHeld(rt, root, func(path string, _ *os.Root, _ os.DirEntry, err error) error {
		fromRoot = append(fromRoot, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		fromWalkDir = append(fromWalkDir, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fromRoot, "\n") != strings.Join(fromWalkDir, "\n") {
		t.Errorf("walkHeld order:\n%s\nfilepath.WalkDir order:\n%s", strings.Join(fromRoot, "\n"), strings.Join(fromWalkDir, "\n"))
	}
}

// openHeldSubdir maps openChildDirRoot's two "the child changed" refusals to
// errDirChanged and passes any other error through as the skip reason. The
// changes themselves need a swap inside the Lstat-to-open window, so the
// mapping is tested where it lives, in heldOpenErr.
//
// Mutation that turns it red: drop the errors.Is line for either sentinel
// from heldOpenErr (each row fails on its own).
func TestHeldOpenErr_MapsChangedChildToDirChanged(t *testing.T) {
	other := &os.PathError{Op: "open", Path: "sub", Err: os.ErrPermission}
	for name, tc := range map[string]struct{ in, want error }{
		"not a directory": {&os.PathError{Op: "open", Path: "sub", Err: errNotADirectory}, errDirChanged},
		"root moved":      {&os.PathError{Op: "open", Path: "sub", Err: errDirRootMoved}, errDirChanged},
		"other error":     {other, other},
	} {
		t.Run(name, func(t *testing.T) {
			if got := heldOpenErr(tc.in); got != tc.want {
				t.Errorf("heldOpenErr(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
