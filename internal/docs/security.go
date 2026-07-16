package docs

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrOutsideRoot indicates a resolved path escaped its declared root — via
// ../ traversal, a symlink pointing outside the root, or an EvalSymlinks
// failure. All three collapse to the same signal: the HTTP layer maps it to
// a bare 404, never explaining which case it was (path structure is not a
// debugging aid to hand a stranger on the loopback interface).
var ErrOutsideRoot = errors.New("path escapes its configured root")

// ErrDisallowedExt indicates the resolved path's extension is not in the
// docs-serving allowlist.
var ErrDisallowedExt = errors.New("file extension is not served")

// allowedExt is the extension allowlist (forgectl#93 security-chain item 4).
// Checked against the SYMLINK-RESOLVED path, never the raw request path, so
// a symlink can't present a ".md" name while its target resolves to
// something else entirely.
var allowedExt = map[string]bool{
	".md":       true,
	".markdown": true,
}

// AllowedExt reports whether path's extension (case-insensitive) is in the
// docs-serving allowlist. Callers MUST apply this to an EvalSymlinks-resolved
// path — see ResolveInRoot's doc comment for why.
func AllowedExt(path string) bool {
	return allowedExt[strings.ToLower(filepath.Ext(path))]
}

// CanonicalizeRoot resolves dir to its canonical, absolute, symlink-free,
// trailing-separator-free form (forgectl#93 security-chain item 2). Root
// canonicalization happens ONCE, here, at config-load time — every later
// per-request traversal check in ResolveInRoot compares canonical-to-
// canonical against this value, never a raw config path.
func CanonicalizeRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// withinRoot reports whether candidate is root itself or a descendant of it.
// The trailing-separator guard on the prefix check is deliberate: root
// "/a/b" must not match a candidate of "/a/bc" — a bare strings.HasPrefix
// would let a sibling directory whose name happens to start with the root's
// basename slip through.
func withinRoot(root, candidate string) bool {
	if candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, root+string(filepath.Separator))
}

// ResolveInRoot safely maps a request-supplied relative path onto a single
// canonical root, per forgectl#93's traversal chain:
//
//  1. filepath.Clean("/"+rel) neutralizes ../ segments by forcing the path
//     absolute-relative first — "../../etc/passwd" collapses to
//     "/etc/passwd" before it ever touches the filesystem.
//  2. filepath.Join(root, cleaned) anchors the cleaned path under root.
//  3. filepath.EvalSymlinks resolves any symlink IN the joined path — a
//     symlink living inside root but pointing outside it. An EvalSymlinks
//     error (including "no such file") denies with ErrOutsideRoot; it never
//     falls through to serving a not-yet-resolved path.
//  4. The resolved path is re-checked against the canonical root with
//     withinRoot's trailing-separator guard — step 2's Join alone doesn't
//     catch a symlink hop discovered in step 3.
//
// root MUST already be canonical (CanonicalizeRoot). The extension allowlist
// is a separate check the caller applies to this function's result via
// AllowedExt — resolution and extension policy are independent gates.
func ResolveInRoot(root, rel string) (string, error) {
	cleaned := filepath.Clean(string(filepath.Separator) + rel)
	joined := filepath.Join(root, cleaned)

	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", ErrOutsideRoot
	}
	resolved = filepath.Clean(resolved)

	if !withinRoot(root, resolved) {
		return "", ErrOutsideRoot
	}
	return resolved, nil
}
