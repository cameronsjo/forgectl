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

// ErrNotFound indicates that the requested path is missing, and that os.Root
// reached the missing component without leaving the root: no symlink or ".."
// on the way pointed outside it. A path missing beyond a symlink that leaves
// the root is ErrOutsideRoot instead, whether or not the outside target
// exists, so the choice between the two errors never reveals anything about
// the filesystem outside the root. It wraps fs.ErrNotExist, so
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

// maxSymlinkHops bounds how many symlinks one resolution follows before it
// denies as ErrOutsideRoot. It is forgectl's own limit, Linux's MAXSYMLINKS,
// so it does not move with os.Root's unexported one.
const maxSymlinkHops = 40

// ResolveInRoot safely maps a request-supplied relative path onto a single
// canonical root, per forgectl#93's traversal chain:
//
//  1. filepath.Clean("/"+rel) neutralizes ../ segments by forcing the path
//     absolute-relative first — "../../etc/passwd" collapses to
//     "/etc/passwd" before it ever touches the filesystem.
//  2. The cleaned path is walked one component at a time through an os.Root
//     opened on root (resolveIn). Every lstat and readlink goes through
//     that Root, so the walk never touches anything outside the root, and a
//     symlink is followed only while its target stays inside it: a relative
//     target whose ".." climbs above the root, or an absolute target that
//     does not name a path under root, denies as ErrOutsideRoot before
//     anything outside is looked at. A chain that leaves the root and comes
//     back is refused at the point it leaves (forgectl#611), and the answer
//     never depends on whether an outside path exists.
//  3. A missing component reached without leaving the root is ErrNotFound,
//     and so is a path that walks through a regular file ("f.md/x") or
//     names a regular file with a trailing slash ("f.md/"), as the kernel
//     would answer ENOTDIR. Any other failure denies as ErrOutsideRoot.
//
// The returned path is root joined with the symlink-free relative path the
// walk ended on, the same canonical path filepath.EvalSymlinks would give
// for any in-root chain. An absolute symlink naming a path inside the root
// keeps resolving, as it did under EvalSymlinks, but only when it spells
// the canonical root: an alias such as /tmp for /private/tmp is refused.
//
// root MUST already be canonical (CanonicalizeRoot). The extension allowlist
// is a separate check the caller applies to this function's result via
// AllowedExt — resolution and extension policy are independent gates.
// Resolving is still a check: a caller that reads the file opens it with
// Index.Open, which opens through the same Root and verifies the file it
// opened is the one this walk approved.
func ResolveInRoot(root, rel string) (string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", ErrOutsideRoot
	}
	defer func() { _ = r.Close() }()
	name, _, err := resolveIn(r, root, rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// resolveIn is ResolveInRoot's walk over an already-open Root for root. It
// returns the symlink-free root-relative name ("." for the root itself) and
// the Lstat of what it names, taken during the walk.
func resolveIn(r *os.Root, root, rel string) (string, fs.FileInfo, error) {
	wantDir := strings.HasSuffix(rel, "/") || strings.HasSuffix(rel, string(filepath.Separator))
	pending := splitPath(filepath.Clean(string(filepath.Separator) + rel))

	cur := "" // symlink-free, relative to root; "" is the root itself
	var info fs.FileInfo
	isDir := true
	hops := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		if !isDir {
			// A component after a regular file: the kernel's ENOTDIR,
			// which is a miss, not an escape.
			return "", nil, ErrNotFound
		}
		if c == ".." {
			// Only a symlink target puts ".." here; the request's own were
			// cleaned away above. cur is symlink-free, so popping it
			// lexically is the kernel's "..".
			if cur == "" {
				return "", nil, ErrOutsideRoot
			}
			cur = parentOf(cur)
			info, isDir = nil, true
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := r.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, ErrNotFound
		}
		if err != nil {
			return "", nil, ErrOutsideRoot
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur, info, isDir = next, fi, fi.IsDir()
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return "", nil, ErrOutsideRoot
		}
		target, err := r.Readlink(next)
		if err != nil || target == "" {
			return "", nil, ErrOutsideRoot
		}
		// A target with a volume, or rooted at a separator without one
		// (Windows' "\\x", rooted on the current drive, which filepath.IsAbs calls
		// relative), is matched against the root as an absolute path.
		if filepath.IsAbs(target) || filepath.VolumeName(target) != "" ||
			strings.HasPrefix(target, "/") || strings.HasPrefix(target, string(filepath.Separator)) {
			rest, ok := underRoot(root, target)
			if !ok {
				return "", nil, ErrOutsideRoot
			}
			cur = ""
			pending = append(rest, pending...)
		} else {
			// A relative target resolves against the link's own directory,
			// which is cur.
			pending = append(splitPath(target), pending...)
		}
		info, isDir = nil, true
	}
	if cur == "" {
		cur = "."
	}
	if info == nil {
		fi, err := r.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, ErrNotFound
		}
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return "", nil, ErrOutsideRoot
		}
		info = fi
	}
	if wantDir && !info.IsDir() {
		return "", nil, ErrNotFound
	}
	return cur, info, nil
}

// splitPath splits p on both "/" and the OS separator, dropping empty and
// "." components. ".." is kept: in a symlink target it means the kernel's
// "..", which resolveIn applies to the walked path, never lexically to the
// target string.
func splitPath(p string) []string {
	parts := strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == filepath.Separator
	})
	out := parts[:0]
	for _, c := range parts {
		if c != "." {
			out = append(out, c)
		}
	}
	return out
}

// parentOf is filepath.Dir for resolveIn's root-relative names, with the
// root spelled "".
func parentOf(name string) string {
	d := filepath.Dir(name)
	if d == "." {
		return ""
	}
	return d
}

// underRoot reports whether the absolute symlink target names a path under
// root, and returns the components below it. It matches root's own
// components one for one, before any ".." is applied: target
// "<root>/s/../x" yields [s .. x], so the walk resolves s first and the
// ".." climbs from wherever s really is. Cleaning the target first would
// collapse it to "<root>/x" and hide an s that points outside. Any ".."
// within the root prefix fails the match, so it denies.
func underRoot(root, target string) ([]string, bool) {
	vol := filepath.VolumeName(root)
	if filepath.VolumeName(target) != vol {
		return nil, false
	}
	want := splitPath(root[len(vol):])
	got := splitPath(target[len(vol):])
	if len(got) < len(want) {
		return nil, false
	}
	for i, c := range want {
		if got[i] != c {
			return nil, false
		}
	}
	return got[len(want):], true
}
