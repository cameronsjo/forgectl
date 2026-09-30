package docs

// Test plan for security.go — forgectl#93's traversal-defense chain.
//
// CanonicalizeRoot (Classification: security gate — root canonicalization)
//   [x] Happy: a plain directory resolves to its absolute, cleaned form
//   [x] Unhappy: a nonexistent directory errors (EvalSymlinks fails)
//
// ResolveInRoot (Classification: security gate — per-request traversal chain)
//   [x] Happy: a plain relative path inside root resolves
//   [x] Happy: a nested relative path inside root resolves
//   [x] Unhappy: ../ escape is neutralized and then rejected
//   [x] Unhappy: an absolute-looking rel ("/etc/passwd") stays anchored under root
//   [x] Unhappy: a symlink inside root pointing outside it is rejected
//   [x] Unhappy: a request for a nonexistent file is rejected (EvalSymlinks error denies, never falls through)
//       as ErrNotFound (wrapping fs.ErrNotExist), never ErrOutsideRoot
//   [x] Unhappy: a missing nested path is ErrNotFound; an existing file through
//       an escaping directory symlink stays ErrOutsideRoot
//   [x] Unhappy: a missing path reached through a symlink that leaves the root
//       is ErrOutsideRoot whether the outside target exists or not, including
//       a relative or absolute "s/../x" the kernel resolves outside
//   [x] Happy: a miss reached through in-root symlinks is ErrNotFound,
//       absolute canonical-root targets included; a 60-hop chain, or an
//       absolute target through an outside alias, denies as ErrOutsideRoot
//   [x] Unhappy: a chain that leaves the root and re-enters it is refused,
//       identically whether the outside directory exists (forgectl#611)
//   [x] Happy: in-root symlinks resolve to the path EvalSymlinks gives
//   [x] Unhappy: a component after a regular file, or a trailing slash on
//       one, is ErrNotFound, not ErrOutsideRoot (forgectl#611)
//   [x] Index.Open reads the resolved doc, refuses escapes, and refuses a
//       file swapped in between the walk and the open
//   [x] a directory swapped for an in-root symlink between Lstat and descent
//       is refused (openDirVerified); a root replaced after indexing is
//       refused by Open and Resolve (openPinnedRoot); a FIFO never blocks
//       openVerified (open_nonblock_unix_test.go)
//   [x] Unhappy: root "/a/b" does not match a resolved path under sibling "/a/bc"
//   [x] Unhappy: a doc maxHeldDirs directories deep resolves; one more level
//       denies as ErrOutsideRoot, so one request holds at most maxHeldDirs
//       directory fds (forgectl#743)
//   [x] Unhappy: openRootDir refuses a root path that is a symlink, so the
//       pin is the directory CanonicalizeRoot resolved (forgectl#743)
//
// AllowedExt (Classification: security gate — extension allowlist)
//   [x] Happy: .md and .markdown (any case) are allowed
//   [x] Unhappy: any other extension, including a disguised double extension, is rejected

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalizeRoot_PlainDir_ResolvesAbsolute(t *testing.T) {
	dir := t.TempDir()
	got, err := CanonicalizeRoot(dir)
	if err != nil {
		t.Fatalf("CanonicalizeRoot: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("CanonicalizeRoot(%q) = %q, want an absolute path", dir, got)
	}
}

func TestCanonicalizeRoot_NonexistentDir_Errors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := CanonicalizeRoot(dir); err == nil {
		t.Error("CanonicalizeRoot on a nonexistent directory: got nil error, want one")
	}
}

func mustCanonicalRoot(t *testing.T, dir string) string {
	t.Helper()
	root, err := CanonicalizeRoot(dir)
	if err != nil {
		t.Fatalf("CanonicalizeRoot(%q): %v", dir, err)
	}
	return root
}

func TestResolveInRoot_PlainRelPath_Resolves(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "readme.md")
	if err := os.WriteFile(target, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := mustCanonicalRoot(t, dir)

	got, err := ResolveInRoot(root, "readme.md")
	if err != nil {
		t.Fatalf("ResolveInRoot: %v", err)
	}
	wantResolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantResolved {
		t.Errorf("ResolveInRoot = %q, want %q", got, wantResolved)
	}
}

func TestResolveInRoot_NestedRelPath_Resolves(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "sub", "deeper", "page.md")
	if err := os.WriteFile(target, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := mustCanonicalRoot(t, dir)

	if _, err := ResolveInRoot(root, "sub/deeper/page.md"); err != nil {
		t.Fatalf("ResolveInRoot: %v", err)
	}
}

func TestResolveInRoot_DotDotEscape_Rejected(t *testing.T) {
	dir := t.TempDir()
	root := mustCanonicalRoot(t, dir)

	cases := []string{
		"../../../../../../etc/passwd",
		"../outside.md",
		"sub/../../outside.md",
		"..%2f..%2fetc%2fpasswd", // not URL-decoded here; still must not escape as a literal rel
	}
	for _, rel := range cases {
		t.Run(rel, func(t *testing.T) {
			_, err := ResolveInRoot(root, rel)
			if err == nil {
				t.Fatalf("ResolveInRoot(%q): got nil error, want ErrOutsideRoot (or a not-exist EvalSymlinks failure)", rel)
			}
		})
	}
}

func TestResolveInRoot_AbsoluteLookingRel_StaysAnchoredUnderRoot(t *testing.T) {
	dir := t.TempDir()
	root := mustCanonicalRoot(t, dir)

	// "/etc/passwd" as the rel path must be treated as root-relative, not
	// filesystem-absolute — Join(root, Clean("/"+"/etc/passwd")) anchors it
	// under root, where it then correctly 404s as nonexistent. ErrNotFound,
	// not a path, is the proof it was looked up under root: the real
	// /etc/passwd exists.
	got, err := ResolveInRoot(root, "/etc/passwd")
	if !errors.Is(err, ErrNotFound) || got != "" {
		t.Errorf("ResolveInRoot(%q) = %q, %v, want \"\", ErrNotFound (nonexistent file under root)", "/etc/passwd", got, err)
	}
}

func TestResolveInRoot_SymlinkEscapingRoot_Rejected(t *testing.T) {
	if os.Getenv("CI") != "" && os.Getuid() == 0 {
		t.Skip("symlink creation may be restricted for root in some CI sandboxes")
	}
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(secret, []byte("# secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	rootDir := t.TempDir()
	link := filepath.Join(rootDir, "escape.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	root := mustCanonicalRoot(t, rootDir)

	_, err := ResolveInRoot(root, "escape.md")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("ResolveInRoot on a symlink escaping root: err = %v, want ErrOutsideRoot", err)
	}
}

func TestResolveInRoot_NonexistentFile_DeniesRatherThanFallingThrough(t *testing.T) {
	dir := t.TempDir()
	root := mustCanonicalRoot(t, dir)

	got, err := ResolveInRoot(root, "never-created.md")
	if got != "" || err == nil {
		t.Fatalf("ResolveInRoot on a nonexistent file = %q, %v, want a denial (EvalSymlinks error must deny, never fall through)", got, err)
	}
	// A missing file is reported as missing, not as an escape.
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ResolveInRoot on a nonexistent file: err = %v, want ErrNotFound wrapping fs.ErrNotExist", err)
	}
	if errors.Is(err, ErrOutsideRoot) {
		t.Errorf("ResolveInRoot on a nonexistent file: err = %v, must not claim the path escapes its root", err)
	}
}

// A missing file under a missing directory is still not-found, and a symlink
// escaping the root keeps ErrOutsideRoot even where its target is a directory
// the rel path descends into: the not-found split must not open a second
// route out of the root.
func TestResolveInRoot_NotFoundSplitKeepsEscapesClosed(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# s"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(rootDir, "out")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	root := mustCanonicalRoot(t, rootDir)

	if _, err := ResolveInRoot(root, "no/such/dir/page.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing nested path: err = %v, want ErrNotFound", err)
	}
	if got, err := ResolveInRoot(root, "out/secret.md"); !errors.Is(err, ErrOutsideRoot) || got != "" {
		t.Errorf("existing file through an escaping dir symlink = %q, %v, want \"\", ErrOutsideRoot", got, err)
	}
}

// Through an escaping symlink, ResolveInRoot must answer the same whether or
// not the outside path exists; otherwise "no such file" versus "escapes"
// reveals it. Each escape case runs twice, outside target missing and then
// present, and must be ErrOutsideRoot both times.
func TestResolveInRoot_MissingPathThroughEscapeIsNoOracle(t *testing.T) {
	for _, present := range []bool{false, true} {
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outside, "d1", "d2"), 0o750); err != nil {
			t.Fatal(err)
		}
		if present {
			for _, f := range []string{filepath.Join("d1", "x"), "target.md", "gone.md"} {
				if err := os.WriteFile(filepath.Join(outside, f), []byte("# s"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		rootDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(rootDir, "real"), 0o750); err != nil {
			t.Fatal(err)
		}
		// Built with sep, never filepath.Join: Join would Clean "s/../x" to
		// "x" and erase the case under test.
		sep := string(filepath.Separator)
		links := map[string]string{
			"out":     outside,                                         // dir symlink leaving the root
			"dangout": filepath.Join(outside, "gone.md"),               // dangling unless present
			"s":       filepath.Join(outside, "d1", "d2"),              // the ".." below climbs from d2
			"a":       "s" + sep + ".." + sep + "x",                    // kernel: outside/d1/x; lexical: root/x
			"abs":     rootDir + sep + "s" + sep + ".." + sep + "x",    // the same, absolute
			"hop":     "real" + sep + ".." + sep + "out" + sep + "tgt", // in-root hop onto an escape
		}
		for name, dest := range links {
			if err := os.Symlink(dest, filepath.Join(rootDir, name)); err != nil {
				t.Skipf("symlink not supported in this environment: %v", err)
			}
		}
		root := mustCanonicalRoot(t, rootDir)

		for _, rel := range []string{"a", "abs", "out/target.md", "out/no/such/dir.md", "dangout", "hop"} {
			got, err := ResolveInRoot(root, rel)
			if got != "" || !errors.Is(err, ErrOutsideRoot) || errors.Is(err, fs.ErrNotExist) {
				t.Errorf("outside present=%v: ResolveInRoot(%q) = %q, %v, want \"\", ErrOutsideRoot", present, rel, got, err)
			}
		}
	}
}

// A miss reached without leaving the root is ErrNotFound, through in-root
// symlinks too, including an absolute target that spells the canonical root.
// A chain past maxSymlinkHops denies as ErrOutsideRoot; only the 60-hop
// chain is asserted, so no case depends on where the limit sits. An
// absolute target that reaches the root through an alias outside it is
// refused: matching it would mean following a symlink outside the root.
func TestResolveInRoot_MissingPathInsideRootIsNotFound(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	root := mustCanonicalRoot(t, rootDir)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	links := map[string]string{
		"in":        "real",                                 // relative, in-root
		"chain":     "in",                                   // in-root symlink to a symlink
		"dangin":    "missing.md",                           // dangling, relative, in-root
		"danginabs": filepath.Join(root, "missing.md"),      // dangling, absolute, canonical root
		"viaalias":  filepath.Join(alias, "missing.md"),     // absolute, through an outside alias
		"absreal":   filepath.Join(root, "real", "gone.md"), // absolute, nested, in-root
	}
	// Relative to real/, the directory holding it: read from the root,
	// "../in" would leave it.
	links[filepath.Join("real", "up")] = filepath.Join("..", "in")
	prev := "missing.md"
	for i := 1; i <= 60; i++ {
		name := fmt.Sprintf("c%d", i)
		links[name] = prev
		prev = name
	}
	for name, dest := range links {
		if err := os.Symlink(dest, filepath.Join(rootDir, name)); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
	}

	cases := []struct {
		rel  string
		want error
	}{
		{"missing.md", ErrNotFound},
		{"in/missing.md", ErrNotFound},
		{"chain/missing.md", ErrNotFound},
		{"dangin", ErrNotFound},
		{"real/up/missing.md", ErrNotFound},
		{"danginabs", ErrNotFound},
		{"absreal", ErrNotFound},
		{"c60", ErrOutsideRoot},
		{"viaalias", ErrOutsideRoot},
	}
	for _, c := range cases {
		got, err := ResolveInRoot(root, c.rel)
		if got != "" || !errors.Is(err, c.want) {
			t.Errorf("ResolveInRoot(%q) = %q, %v, want \"\", %v", c.rel, got, err, c.want)
		}
	}
}

// forgectl#611: a chain that leaves the root and comes back into it is
// refused where it leaves, even though its end is an existing in-root doc,
// and the answer is the same whether the outside directory it passes
// through exists. The old EvalSymlinks success path served "ret/f.md" and
// answered "reenter" differently depending on whether outside/ existed.
func TestResolveInRoot_ChainLeavingAndReenteringTheRootIsRefused(t *testing.T) {
	for _, present := range []bool{false, true} {
		base := mustCanonicalRoot(t, t.TempDir())
		rootDir := filepath.Join(base, "root")
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(rootDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootDir, "f.md"), []byte("# f"), 0o600); err != nil {
			t.Fatal(err)
		}
		if present {
			if err := os.MkdirAll(outside, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rootDir, filepath.Join(outside, "back")); err != nil {
				t.Skipf("symlink not supported in this environment: %v", err)
			}
		}
		sep := string(filepath.Separator)
		links := map[string]string{
			"ret":       filepath.Join(outside, "back"),
			"reenter":   ".." + sep + "outside" + sep + ".." + sep + "root" + sep + "f.md",
			"absreturn": rootDir + sep + ".." + sep + "outside" + sep + ".." + sep + "root" + sep + "f.md",
		}
		for name, dest := range links {
			if err := os.Symlink(dest, filepath.Join(rootDir, name)); err != nil {
				t.Skipf("symlink not supported in this environment: %v", err)
			}
		}
		for _, rel := range []string{"ret/f.md", "reenter", "absreturn"} {
			got, err := ResolveInRoot(rootDir, rel)
			if got != "" || !errors.Is(err, ErrOutsideRoot) {
				t.Errorf("outside present=%v: ResolveInRoot(%q) = %q, %v, want \"\", ErrOutsideRoot", present, rel, got, err)
			}
		}
	}
}

// In-root symlinks keep resolving to the same canonical path EvalSymlinks
// gives: relative file and directory links, a ".." that stays inside, an
// absolute target spelling the canonical root, and a 20-hop chain.
func TestResolveInRoot_InRootSymlinksStillResolve(t *testing.T) {
	rootDir := t.TempDir()
	root := mustCanonicalRoot(t, rootDir)
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "real", "doc.md")
	if err := os.WriteFile(want, []byte("# d"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"file.md":                         filepath.Join("real", "doc.md"),
		"d":                               "real",
		"abs.md":                          want,
		filepath.Join("real", "up"):       filepath.Join("..", "d"),
		filepath.Join("real", "self.md"):  filepath.Join("..", "real", "doc.md"),
		filepath.Join("real", "absdirln"): filepath.Join(root, "d"),
	}
	prev := "file.md"
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("h%d", i)
		links[name] = prev
		prev = name
	}
	for name, dest := range links {
		if err := os.Symlink(dest, filepath.Join(root, name)); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
	}
	for _, rel := range []string{
		"real/doc.md", "file.md", "d/doc.md", "abs.md", "real/up/doc.md",
		"real/self.md", "real/absdirln/doc.md", "h20",
	} {
		got, err := ResolveInRoot(root, rel)
		if err != nil || got != want {
			t.Errorf("ResolveInRoot(%q) = %q, %v, want %q", rel, got, err, want)
			continue
		}
		if ev, err := filepath.EvalSymlinks(filepath.Join(root, rel)); err != nil || ev != got {
			t.Errorf("ResolveInRoot(%q) = %q, EvalSymlinks gives %q, %v", rel, got, ev, err)
		}
	}
	if got, err := ResolveInRoot(root, "d/"); err != nil || got != filepath.Join(root, "real") {
		t.Errorf("ResolveInRoot(%q) = %q, %v, want %q", "d/", got, err, filepath.Join(root, "real"))
	}
}

// forgectl#611 items 2 and 3: walking through a regular file, or naming one
// with a trailing slash, is the kernel's ENOTDIR, a miss inside the root.
// It is ErrNotFound, never ErrOutsideRoot, and never the file itself.
func TestResolveInRoot_NonDirectoryComponentIsNotFound(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.md"), []byte("# f"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dest := range map[string]string{
		"lf.md":  "f.md",
		"dotdot": "f.md" + string(filepath.Separator) + "..",
	} {
		if err := os.Symlink(dest, filepath.Join(root, name)); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
	}
	for _, rel := range []string{"f.md/x", "f.md/", "lf.md/", "lf.md/x", "sub/../f.md/", "dotdot"} {
		got, err := ResolveInRoot(root, rel)
		if got != "" || !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveInRoot(%q) = %q, %v, want \"\", ErrNotFound", rel, got, err)
		}
	}
	if got, err := ResolveInRoot(root, "sub/"); err != nil || got != filepath.Join(root, "sub") {
		t.Errorf("ResolveInRoot(%q) = %q, %v, want the directory", "sub/", got, err)
	}
}

// Index.Open opens the doc Resolve approves, through the root, and refuses
// the escapes Resolve refuses.
func TestIndexOpen_ReadsTheResolvedDocAndRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := mustCanonicalRoot(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "page.md"), []byte("# page"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "leak.md")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label

	f, resolved, err := idx.Open(label, "page.md")
	if err != nil {
		t.Fatalf("Open(page.md): %v", err)
	}
	b, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(b) != "# page" || resolved != filepath.Join(root, "page.md") {
		t.Errorf("Open(page.md) read %q, %v, resolved %q", b, err, resolved)
	}
	if f, _, err := idx.Open(label, "leak.md"); err == nil {
		_ = f.Close()
		t.Error("Open(leak.md) through an escaping symlink succeeded, want a denial")
	}
	if _, _, err := idx.Open("no-such-root", "page.md"); !errors.Is(err, ErrRootNotFound) {
		t.Errorf("Open on an unknown root: err = %v, want ErrRootNotFound", err)
	}
}

// openVerified is the half of Open that closes the swap between resolving
// and opening: a file renamed over the approved path after the walk, even
// an in-root one, is refused, because it is not the file the walk saw.
func TestOpenVerified_RefusesAFileSwappedInAfterTheWalk(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	doc := filepath.Join(root, "doc.md")
	if err := os.WriteFile(doc, []byte("# approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other"), []byte("SWAPPED"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	end, err := resolveIn(r, nil, root, "doc.md")
	if err != nil {
		t.Fatal(err)
	}
	defer end.close()
	name, info := end.base, end.info
	f, err := openVerified(end.dir, name, info)
	if err != nil {
		t.Fatalf("openVerified on the unswapped doc: %v", err)
	}
	_ = f.Close()

	if err := os.Rename(filepath.Join(root, "other"), doc); err != nil {
		t.Fatal(err)
	}
	if f, err := openVerified(end.dir, name, info); !errors.Is(err, ErrOutsideRoot) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("openVerified after a swap: err = %v, want ErrOutsideRoot", err)
	}
}

func TestResolveInRoot_SiblingPrefixCollision_Rejected(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "a", "b")
	siblingDir := filepath.Join(base, "a", "bc")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(siblingDir, "file.md")
	if err := os.WriteFile(target, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := mustCanonicalRoot(t, rootDir)

	// A rel path can't literally spell "../bc/file.md" and pass Clean("/"+rel)
	// unescaped, but withinRoot itself must still reject a would-be prefix
	// collision directly, since it's the last line of defense.
	canonicalSibling := mustCanonicalRoot(t, siblingDir)
	if withinRoot(root, filepath.Join(canonicalSibling, "file.md")) {
		t.Errorf("withinRoot(%q, sibling-under-%q) = true, want false (prefix-collision guard)", root, canonicalSibling)
	}
}

func TestAllowedExt_MarkdownExtensions_Allowed(t *testing.T) {
	cases := []string{"doc.md", "DOC.MD", "doc.markdown", "doc.Markdown"}
	for _, name := range cases {
		if !AllowedExt(name) {
			t.Errorf("AllowedExt(%q) = false, want true", name)
		}
	}
}

func TestAllowedExt_DisallowedExtensions_Rejected(t *testing.T) {
	cases := []string{"doc.txt", "doc.html", "doc.env", "doc.md.env", "doc", "doc.MDX"}
	for _, name := range cases {
		if AllowedExt(name) {
			t.Errorf("AllowedExt(%q) = true, want false", name)
		}
	}
}

// forgectl#611 review: a directory swapped for a symlink to another in-root
// directory between the walk's Lstat and its descent is refused, not
// followed. os.Root.OpenRoot alone would follow it, since the target stays
// inside the parent; the identity check is what refuses it.
func TestOpenDirVerified_RefusesADirectorySwappedForASymlink(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	for _, d := range []string{"sub", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	info, err := r.Lstat("sub")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := openDirVerified(r, "sub", info)
	if err != nil {
		t.Fatalf("openDirVerified on the unswapped directory: %v", err)
	}
	_ = sub.Close()

	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("node_modules", filepath.Join(root, "sub")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	if sub, err := openDirVerified(r, "sub", info); !errors.Is(err, ErrOutsideRoot) {
		if sub != nil {
			_ = sub.Close()
		}
		t.Errorf("openDirVerified after a swap: err = %v, want ErrOutsideRoot", err)
	}
}

// forgectl#611 review: the root path itself is pinned. Moving an indexed
// root aside and putting a symlink to another directory in its place must
// not make Open or Resolve read through the symlink.
func TestIndexOpen_RefusesARootReplacedAfterIndexing(t *testing.T) {
	base := mustCanonicalRoot(t, t.TempDir())
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "page.md"), []byte("# page"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "page.md"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label
	f, _, err := idx.Open(label, "page.md")
	if err != nil {
		t.Fatalf("Open before the swap: %v", err)
	}
	_ = f.Close()

	if err := os.Rename(root, root+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	if f, _, err := idx.Open(label, "page.md"); err == nil {
		b, _ := io.ReadAll(f)
		_ = f.Close()
		t.Errorf("Open through a replaced root read %q, want a denial", b)
	}
	if _, err := idx.Resolve(label, "page.md"); err == nil {
		t.Error("Resolve through a replaced root succeeded, want a denial")
	}
}

// nestedDoc creates depth directories below root, each named "d", with a
// doc.md in the deepest, and returns the doc's root-relative path.
func nestedDoc(t *testing.T, root string, depth int) string {
	t.Helper()
	rel := ""
	for range depth {
		rel = filepath.Join(rel, "d")
	}
	if err := os.MkdirAll(filepath.Join(root, rel), 0o750); err != nil {
		t.Fatal(err)
	}
	rel = filepath.Join(rel, "doc.md")
	if err := os.WriteFile(filepath.Join(root, rel), []byte("# deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

func TestResolveInRoot_DepthCapBoundsHeldDirectories(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	atCap := nestedDoc(t, filepath.Join(root, "ok"), maxHeldDirs-1)
	if _, err := ResolveInRoot(root, "ok/"+atCap); err != nil {
		t.Errorf("a doc %d directories deep: err = %v, want it resolved", maxHeldDirs, err)
	}
	past := nestedDoc(t, filepath.Join(root, "deep"), maxHeldDirs)
	if got, err := ResolveInRoot(root, "deep/"+past); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("a doc %d directories deep resolved to %q, err = %v, want ErrOutsideRoot", maxHeldDirs+1, got, err)
	}
}

func TestOpenRootDir_RefusesASymlinkAtTheRootPath(t *testing.T) {
	base := mustCanonicalRoot(t, t.TempDir())
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rt, info, err := openRootDir(realDir)
	if err != nil {
		t.Fatalf("openRootDir on a plain directory: %v", err)
	}
	_ = rt.Close()
	if !info.IsDir() {
		t.Errorf("pinned info is not a directory: %v", info.Mode())
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	if rt, _, err := openRootDir(link); err == nil {
		_ = rt.Close()
		t.Error("openRootDir pinned a symlink, want a refusal")
	}
}
