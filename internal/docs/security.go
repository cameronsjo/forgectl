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

// maxHeldDirs caps how many directories one resolution holds open at once
// below its root, one os.Root (one fd) per level. A path nested deeper
// denies as ErrOutsideRoot, and the index walk skips a directory past the
// same depth (walkRoot), so every indexed doc stays servable. Without it a
// request's fd cost grew with the tree's depth, bounded only by
// maxSymlinkHops and the kernel's PATH_MAX (forgectl#743).
const maxHeldDirs = 64

// ResolveInRoot safely maps a request-supplied relative path onto a single
// canonical root, per forgectl#93's traversal chain:
//
//  1. filepath.Clean("/"+rel) neutralizes ../ segments by forcing the path
//     absolute-relative first — "../../etc/passwd" collapses to
//     "/etc/passwd" before it ever touches the filesystem.
//  2. The cleaned path is walked one component at a time (resolveIn), each
//     lookup a single name in a directory held open as an os.Root, so the
//     walk never touches anything outside the root, and a
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
	r, err := openDirRoot(root)
	if err != nil {
		return "", ErrOutsideRoot
	}
	defer func() { _ = r.Close() }()
	end, err := resolveIn(r, nil, root, rel)
	if err != nil {
		return "", err
	}
	end.close()
	return filepath.Join(root, end.name), nil
}

// walkLevel is one directory on resolveIn's path: an os.Root opened on it,
// its root-relative name ("" for the root), and its Lstat.
type walkLevel struct {
	r    *os.Root
	name string
	info fs.FileInfo
}

// walkEnd is where resolveIn stopped. dir is the directory holding the
// result, or the result itself when base is "". The caller calls close,
// which closes every Root the walk opened below the one it was handed.
type walkEnd struct {
	name  string // root-relative, "." for the root itself
	info  fs.FileInfo
	dir   *os.Root
	base  string
	stack []walkLevel
}

func (e *walkEnd) close() { closeLevels(e.stack[1:]) }

func closeLevels(levels []walkLevel) {
	for _, l := range levels {
		_ = l.r.Close()
	}
}

// resolveIn is ResolveInRoot's walk over an already-open Root r for root.
// rootInfo is r's own Stat when the caller already holds it
// (openPinnedRoot), or nil to take it here.
//
// Every lookup is a single name in a directory the walk holds open: each
// directory it descends into is opened as its own os.Root (openDirVerified)
// and must be the directory its Lstat saw. So no lookup follows a symlink
// the walk did not see and choose to follow, and a directory swapped for a
// symlink between the Lstat and the descent is refused rather than
// followed (forgectl#611 review). Only the walk follows symlinks, and only
// while they stay inside the root.
func resolveIn(r *os.Root, rootInfo fs.FileInfo, root, rel string) (*walkEnd, error) {
	wantDir := strings.HasSuffix(rel, "/") || strings.HasSuffix(rel, string(filepath.Separator))
	pending := splitPath(filepath.Clean(string(filepath.Separator) + rel))

	if rootInfo == nil {
		var err error
		if rootInfo, err = r.Stat("."); err != nil {
			return nil, ErrOutsideRoot
		}
	}
	stack := []walkLevel{{r: r, info: rootInfo}}
	fail := func(err error) (*walkEnd, error) {
		closeLevels(stack[1:])
		return nil, err
	}
	var fileBase string
	var fileInfo fs.FileInfo
	hops := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		if fileInfo != nil {
			// A component after a regular file: the kernel's ENOTDIR,
			// which is a miss, not an escape.
			return fail(ErrNotFound)
		}
		if c == ".." {
			// Only a symlink target puts ".." here; the request's own were
			// cleaned away above. Popping the held directory is the
			// kernel's "..", since every level is a real directory.
			if len(stack) == 1 {
				return fail(ErrOutsideRoot)
			}
			closeLevels(stack[len(stack)-1:])
			stack = stack[:len(stack)-1]
			continue
		}
		cur := stack[len(stack)-1]
		fi, err := cur.r.Lstat(c)
		if errors.Is(err, fs.ErrNotExist) {
			return fail(ErrNotFound)
		}
		if err != nil {
			// Left as a denial on purpose. A single-name Lstat cannot
			// leave the root, so these (EACCES, ENAMETOOLONG, a NUL's
			// EINVAL) are misses in fact, but telling them apart needs
			// per-OS errnos plus os.Root's unexported escape error, and a
			// wrong guess would call an escape "not found". Every caller
			// denies both the same way (a 404, a broken link).
			return fail(ErrOutsideRoot)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			if !fi.IsDir() {
				fileBase, fileInfo = c, fi
				continue
			}
			if len(stack) > maxHeldDirs {
				return fail(ErrOutsideRoot)
			}
			sub, err := openDirVerified(cur.r, c, fi)
			if err != nil {
				return fail(err)
			}
			stack = append(stack, walkLevel{r: sub, name: filepath.Join(cur.name, c), info: fi})
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return fail(ErrOutsideRoot)
		}
		target, err := cur.r.Readlink(c)
		if err != nil || target == "" {
			return fail(ErrOutsideRoot)
		}
		// A target with a volume, or rooted at a separator without one
		// (Windows' "\\x", rooted on the current drive, which filepath.IsAbs
		// calls relative), is matched against the root as an absolute path.
		if filepath.IsAbs(target) || filepath.VolumeName(target) != "" ||
			strings.HasPrefix(target, "/") || strings.HasPrefix(target, string(filepath.Separator)) {
			rest, ok := underRoot(root, target)
			if !ok {
				return fail(ErrOutsideRoot)
			}
			closeLevels(stack[1:])
			stack = stack[:1]
			pending = append(rest, pending...)
		} else {
			// A relative target resolves against the link's own
			// directory, which is the level held on top.
			pending = append(splitPath(target), pending...)
		}
	}
	top := stack[len(stack)-1]
	if fileInfo != nil {
		if wantDir {
			return fail(ErrNotFound)
		}
		return &walkEnd{name: filepath.Join(top.name, fileBase), info: fileInfo, dir: top.r, base: fileBase, stack: stack}, nil
	}
	name := top.name
	if name == "" {
		name = "."
	}
	return &walkEnd{name: name, info: top.info, dir: top.r, stack: stack}, nil
}

// openDirVerified opens the directory name in parent as its own Root and
// returns it only if it is the directory want describes. os.Root.OpenRoot
// follows a symlink that stays inside parent, so without the identity check
// a directory swapped for a symlink after its Lstat would redirect the
// walk to another directory under parent. The open is openChildDirRoot, so
// a FIFO swapped in after the Lstat is refused rather than waited on.
func openDirVerified(parent *os.Root, name string, want fs.FileInfo) (*os.Root, error) {
	sub, err := openChildDirRoot(parent, name)
	if err != nil {
		return nil, ErrOutsideRoot
	}
	got, err := sub.Stat(".")
	if err != nil || !got.IsDir() || !os.SameFile(want, got) {
		_ = sub.Close()
		return nil, ErrOutsideRoot
	}
	return sub, nil
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
