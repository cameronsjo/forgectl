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
//   [x] Happy: a miss reached through in-root symlinks is ErrNotFound, up to
//       os.Root's 8-symlink limit; past it, or through an absolute target,
//       it denies as ErrOutsideRoot
//   [x] Unhappy: root "/a/b" does not match a resolved path under sibling "/a/bc"
//
// AllowedExt (Classification: security gate — extension allowlist)
//   [x] Happy: .md and .markdown (any case) are allowed
//   [x] Unhappy: any other extension, including a disguised double extension, is rejected

import (
	"errors"
	"fmt"
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
// symlinks too. os.Root is the judge: it follows at most 8 symlinks
// (rootMaxSymlinks in go1.26) and reads any absolute symlink target as an
// escape, so a longer chain or an absolute in-root target denies as
// ErrOutsideRoot. Both are fail-closed and say nothing about outside paths.
func TestResolveInRoot_MissingPathInsideRootIsNotFound(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"in":        "real",                               // relative, in-root
		"chain":     "in",                                 // in-root symlink to a symlink
		"dangin":    "missing.md",                         // dangling, relative, in-root
		"danginabs": filepath.Join(rootDir, "missing.md"), // dangling, absolute, in-root
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
	root := mustCanonicalRoot(t, rootDir)

	cases := []struct {
		rel  string
		want error
	}{
		{"missing.md", ErrNotFound},
		{"in/missing.md", ErrNotFound},
		{"chain/missing.md", ErrNotFound},
		{"dangin", ErrNotFound},
		{"real/up/missing.md", ErrNotFound},
		{"c8", ErrNotFound},
		{"c9", ErrOutsideRoot},
		{"c60", ErrOutsideRoot},
		{"danginabs", ErrOutsideRoot},
	}
	for _, c := range cases {
		got, err := ResolveInRoot(root, c.rel)
		if got != "" || !errors.Is(err, c.want) {
			t.Errorf("ResolveInRoot(%q) = %q, %v, want \"\", %v", c.rel, got, err, c.want)
		}
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
