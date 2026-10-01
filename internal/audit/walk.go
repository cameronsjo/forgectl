package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// This file is the confined walk every audit scan shares (forgectl#14): the
// injection inventory and the secret-hygiene scan see the same tree through
// the same fsOps, with the same caps, so neither can follow a link or list a
// directory the other would refuse.
//
// The walk lists each directory once through fsOps.names, skips .git (and
// takes its presence as the mark of a git working tree), Lstats every other
// entry through fsOps.lstat, and hands each entry to the scan's visit. A
// directory the visit does not claim is descended into, below the depth cap.
// A symlink is never a directory under Lstat, so it is never descended.

// walkStats is what the walk counts, whatever the scan looks for.
type walkStats struct {
	// Repos lists the git working trees the walk entered, relative to the
	// root and slash-separated ("." for the root itself), in walk order.
	Repos []string
	// Entries counts directory entries examined.
	Entries int
	// Unreadable counts directories the walk could not list and entries it
	// could not Lstat.
	Unreadable int
	// CappedBy names each cap that was hit, in the order first hit.
	CappedBy []string
	// DepthSkipped counts directories left unwalked at the depth cap.
	DepthSkipped int
}

// capped records that the named cap was hit, once.
func (s *walkStats) capped(name string) {
	for _, c := range s.CappedBy {
		if c == name {
			return
		}
	}
	s.CappedBy = append(s.CappedBy, name)
}

// entry is one directory entry the walk hands to a scan.
type entry struct {
	// rel is the entry's slash-separated path below the root.
	rel string
	// segs are rel's segments.
	segs []string
	// info is the entry's Lstat.
	info fs.FileInfo
	// repo is the nearest git working tree at or above the entry's parent,
	// relative to the root: "" for none, "." for the root itself.
	repo string
	// vendored is set when a dependency directory encloses the entry.
	vendored bool
}

// walker is one confined walk. visit is called once per entry other than
// .git; it returns claimed=true to keep a directory from being descended
// (a carrier reported as a unit), and errStop to end the walk at a cap.
type walker struct {
	ops        fsOps
	root       string
	maxEntries int
	maxDepth   int
	stats      *walkStats
	visit      func(e entry) (claimed bool, err error)
	// onRepo, when set, is called once for each git working tree as the
	// walk enters it, with the tree's own entry names, before any entry is
	// visited or counted against a cap.
	onRepo func(dir string, names []string)
	// repoNames, when set, reports a name onRepo needs to see, so a listing
	// the cap bounds keeps it whatever it sorts as.
	repoNames func(name string) bool
}

// keepName reports the names a bounded listing keeps beyond its sorted
// prefix: .git, which marks a working tree, and any name onRepo checks.
func (w *walker) keepName(name string) bool {
	return name == ".git" || (w.repoNames != nil && w.repoNames(name))
}

// run walks the whole tree. It errors only when the root itself cannot be
// listed; errStop from visit ends the walk cleanly.
func (w *walker) run() error {
	if err := w.walk(".", nil, "", false, 0); err != nil && !errors.Is(err, errStop) {
		return err
	}
	return nil
}

// walk lists dir (slash-separated, relative to the root) and visits each
// entry. segs are dir's own segments; repo is the nearest git working tree
// at or above dir, relative to the root, with "" meaning none and "." the
// root itself. depth is dir's own depth, the root being 0: with maxDepth N
// the walk lists directories at depths 0 through N-1.
func (w *walker) walk(dir string, segs []string, repo string, vendored bool, depth int) error {
	// One name past what the cap has left is enough to trip it: the loop
	// below counts every name, so a directory longer than that stops the
	// walk inside this listing, and the listing holds no more names than
	// that, plus the kept ones below (#994). It is the sorted prefix a whole read would give, so a capped
	// walk visits the entries it always did. .git and the names onRepo
	// checks are kept whatever they sort as, so neither the repo nor its
	// scanner config is missed. The +1 saturates rather than wrap when the
	// cap is math.MaxInt.
	left := w.maxEntries - w.stats.Entries
	limit := left + 1
	if limit < left {
		limit = left
	}
	names, err := w.ops.names(dir, limit, w.keepName)
	if err != nil {
		if dir == "." {
			return fmt.Errorf("read audit root %s: %w", termsafe.QuotePath(w.root), termsafe.Error(err))
		}
		w.stats.Unreadable++
		return nil
	}
	sort.Strings(names)
	for _, name := range names {
		if name == ".git" {
			repo = dir
			w.stats.Repos = append(w.stats.Repos, dir)
			if w.onRepo != nil {
				w.onRepo(dir, names)
			}
			break
		}
	}

	for _, name := range names {
		w.stats.Entries++
		if w.stats.Entries > w.maxEntries {
			w.stats.capped(CapEntries)
			return errStop
		}
		if name == ".git" {
			continue
		}
		rel := path.Join(dir, name)
		info, err := w.ops.lstat(rel)
		if err != nil {
			w.stats.Unreadable++ // vanished or unreadable mid-walk: counted, not classified
			continue
		}
		childSegs := append(append(make([]string, 0, len(segs)+1), segs...), name)
		claimed, err := w.visit(entry{rel: rel, segs: childSegs, info: info, repo: repo, vendored: vendored})
		if err != nil {
			return err
		}
		if claimed || !info.Mode().IsDir() { // Lstat: a symlink is never IsDir, so it is never followed
			continue
		}
		if depth+1 >= w.maxDepth {
			w.stats.capped(CapDepth)
			w.stats.DepthSkipped++
			continue
		}
		if err := w.walk(rel, childSegs, repo, vendored || dependencyDirs[name], depth+1); err != nil {
			return err
		}
	}
	return nil
}
