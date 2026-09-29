package docs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideRoot indicates a resolved path escaped its declared root — via
// ../ traversal, a symlink pointing outside the root, or an EvalSymlinks
// failure other than a missing path. All three collapse to the same signal:
// the HTTP layer maps it to a bare 404, never explaining which case it was
// (path structure is not a debugging aid to hand a stranger on the loopback
// interface).
var ErrOutsideRoot = errors.New("path escapes its configured root")

// ErrNotFound indicates that the requested path is missing, and that the
// missing component lies inside its root: every directory above it, and every
// symlink on the way, stays within the root. A path missing beyond a symlink
// that leaves the root is ErrOutsideRoot instead, whether or not the outside
// target exists, so the choice between the two errors never reveals anything
// about the filesystem outside the root. It wraps fs.ErrNotExist, so
// errors.Is(err, fs.ErrNotExist) holds. It is a denial like ErrOutsideRoot,
// never a path to serve: it only keeps a missing file from being reported as
// an escape.
var ErrNotFound = fmt.Errorf("no such file under its configured root: %w", fs.ErrNotExist)

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
//     symlink living inside root but pointing outside it. Every EvalSymlinks
//     error denies; it never falls through to serving a not-yet-resolved
//     path. A missing path (fs.ErrNotExist) denies with whatever
//     classifyMissing decides, and any other failure with ErrOutsideRoot.
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
	if errors.Is(err, fs.ErrNotExist) {
		return "", classifyMissing(root, cleaned)
	}
	if err != nil {
		return "", ErrOutsideRoot
	}
	resolved = filepath.Clean(resolved)

	if !withinRoot(root, resolved) {
		return "", ErrOutsideRoot
	}
	return resolved, nil
}

// maxSymlinkHops bounds the symlinks classifyMissing follows, as the OS bounds
// its own resolution (ELOOP). A path needing more denies as an escape.
const maxSymlinkHops = 40

// classifyMissing picks the denial for a path EvalSymlinks reported missing,
// without letting the answer depend on anything outside root. It walks rel
// (cleaned, rooted at root) one component at a time with Lstat, from root:
//
//   - A missing component is ErrNotFound. The walk reached its parent
//     without leaving root, so the miss is inside the root.
//   - A symlink whose target lies lexically outside root is ErrOutsideRoot,
//     whether that target exists or not. It is never followed, so nothing
//     outside the root is ever looked at.
//   - A symlink whose target lies inside root restarts the walk at that
//     target, so a chain of in-root symlinks is judged hop by hop.
//   - Anything else (another Lstat error, too many hops, a walk that finds
//     every component present after all) is ErrOutsideRoot: fail closed.
//
// The walk only descends through real directories: rel carries no ".."
// after ResolveInRoot's Clean, and a relative symlink target is joined onto
// the symlink-free directory holding it, so the lexical check is exact.
func classifyMissing(root, rel string) error {
	parts := splitRel(rel)
	cur := root
	hops := 0
	for i := 0; i < len(parts); i++ {
		next := filepath.Join(cur, parts[i])
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		if err != nil {
			return ErrOutsideRoot
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return ErrOutsideRoot
		}
		dest, err := os.Readlink(next)
		if err != nil {
			return ErrOutsideRoot
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(cur, dest)
		}
		dest = filepath.Clean(dest)
		if !withinRoot(root, dest) {
			return ErrOutsideRoot
		}
		destRel, err := filepath.Rel(root, dest)
		if err != nil {
			return ErrOutsideRoot
		}
		parts = append(splitRel(destRel), parts[i+1:]...)
		cur = root
		i = -1
	}
	return ErrOutsideRoot
}

// splitRel splits a root-relative path into its components, dropping empty
// and "." ones, so the root itself splits to none.
func splitRel(rel string) []string {
	var out []string
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}
