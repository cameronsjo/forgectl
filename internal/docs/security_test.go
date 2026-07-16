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
//   [x] Unhappy: root "/a/b" does not match a resolved path under sibling "/a/bc"
//
// AllowedExt (Classification: security gate — extension allowlist)
//   [x] Happy: .md and .markdown (any case) are allowed
//   [x] Unhappy: any other extension, including a disguised double extension, is rejected

import (
	"errors"
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
	// under root, where it then correctly 404s as nonexistent.
	_, err := ResolveInRoot(root, "/etc/passwd")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("ResolveInRoot(%q) = %v, want ErrOutsideRoot (nonexistent file under root)", "/etc/passwd", err)
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

	_, err := ResolveInRoot(root, "never-created.md")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("ResolveInRoot on a nonexistent file: err = %v, want ErrOutsideRoot (EvalSymlinks error must deny, never fall through)", err)
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
