// Package docs is the ops layer for `forgectl docs` (#93): a pure-Go,
// server-side-rendered local markdown reader. It indexes a closed set of
// root directories, renders markdown to sanitized HTML, and serves both over
// loopback HTTP. It knows nothing of Cobra — that decoupling is the house
// pattern (see internal/tmux, internal/net).
//
// PR1 scope only: render + index, no live reload, no mermaid/pan-zoom SVG
// (forgectl#93 stages those as PR2/PR3).
package docs

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Root is one canonicalized root directory the server is willing to index
// and serve files from. Canonicalization happens once, at construction
// (NewIndex → CanonicalizeRoot) — every later request resolves against this
// already-EvalSymlinks'd value (security.go).
type Root struct {
	// Label identifies this root in URLs and the sidenav. Derived from the
	// directory's (or, for OnlyFile roots, the file's) base name,
	// disambiguated with a numeric suffix if two roots share one.
	Label string
	// Path is the canonical, symlink-resolved, trailing-separator-free
	// absolute directory — the traversal boundary ResolveInRoot enforces.
	// For an OnlyFile root this is the file's PARENT directory, never the
	// file itself (ResolveInRoot needs a directory to walk Join/EvalSymlinks
	// against).
	Path string
	// OnlyFile, when non-empty, is the canonical absolute path of the SOLE
	// file this root may ever serve — set when the user named a single
	// markdown file on the command line rather than a directory. Path is
	// still that file's parent directory (so traversal-checking machinery
	// is shared with directory roots), but Resolve additionally rejects
	// any resolved path other than OnlyFile: naming one file must not
	// silently grant access to every other file in its directory.
	OnlyFile string
}

// Doc is one indexed markdown file.
type Doc struct {
	// RootLabel is the Root.Label this doc was found under.
	RootLabel string
	// RelPath is the doc's path relative to its root, always forward-slash
	// separated regardless of host OS — it's used as a URL path segment.
	RelPath string
	// Title is the doc's first level-1 ("# ") heading, or its filename
	// (without extension) if none is found.
	Title string
	// ModTime is the file's last-modified time, used to order "recents".
	ModTime time.Time
}

// Index holds a closed set of Roots and the Docs discovered under them at
// construction time. It does not watch the filesystem — a changed tree needs
// a fresh NewIndex (live reload is forgectl#93 PR2).
type Index struct {
	roots []Root
	docs  []Doc
}

// NewIndex builds an Index over paths, each of which is either a directory
// (canonicalized and walked for markdown files, AllowedExt) or a single
// markdown file (canonicalized and indexed alone, without granting access to
// its sibling files — see Root.OnlyFile). A path that fails to canonicalize
// (doesn't exist, permission denied) or names a file with a disallowed
// extension is a hard error — a docs server should never silently start
// with fewer roots than the caller asked for.
func NewIndex(paths []string) (*Index, error) {
	idx := &Index{}
	labels := map[string]bool{}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("docs root %q: %w", p, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("docs root %q: %w", p, err)
		}

		if info.IsDir() {
			root, docs, err := indexDirRoot(labels, p)
			if err != nil {
				return nil, err
			}
			idx.roots = append(idx.roots, root)
			idx.docs = append(idx.docs, docs...)
			continue
		}

		root, doc, err := indexFileRoot(labels, p)
		if err != nil {
			return nil, err
		}
		idx.roots = append(idx.roots, root)
		idx.docs = append(idx.docs, doc)
	}

	sort.Slice(idx.docs, func(i, j int) bool { return idx.docs[i].ModTime.After(idx.docs[j].ModTime) })
	return idx, nil
}

// indexDirRoot canonicalizes dir and walks it for markdown files.
func indexDirRoot(labels map[string]bool, dir string) (Root, []Doc, error) {
	canonical, err := CanonicalizeRoot(dir)
	if err != nil {
		return Root{}, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	label := uniqueLabel(labels, filepath.Base(canonical))
	root := Root{Label: label, Path: canonical}
	docs, err := walkRoot(root)
	if err != nil {
		return Root{}, nil, fmt.Errorf("docs root %q: %w", dir, err)
	}
	return root, docs, nil
}

// indexFileRoot canonicalizes a single markdown file and indexes it alone.
// The returned Root's Path is the file's canonical PARENT directory (needed
// so ResolveInRoot has a directory to Join/EvalSymlinks against), but
// Root.OnlyFile pins the one path Resolve will ever hand back for it.
func indexFileRoot(labels map[string]bool, file string) (Root, Doc, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	real = filepath.Clean(real)
	if !AllowedExt(real) {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: not a markdown file", file)
	}

	parent, err := CanonicalizeRoot(filepath.Dir(real))
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}

	base := filepath.Base(real)
	label := uniqueLabel(labels, strings.TrimSuffix(base, filepath.Ext(base)))
	root := Root{Label: label, Path: parent, OnlyFile: real}

	fi, err := os.Stat(real)
	if err != nil {
		return Root{}, Doc{}, fmt.Errorf("docs root %q: %w", file, err)
	}
	doc := Doc{
		RootLabel: label,
		RelPath:   base,
		Title:     titleFor(real, base),
		ModTime:   fi.ModTime(),
	}
	return root, doc, nil
}

// uniqueLabel returns base, or base suffixed with an incrementing counter if
// base is already taken — two configured roots sharing a base name (e.g. two
// different "docs" directories) must not collide in the URL/sidenav
// namespace.
func uniqueLabel(taken map[string]bool, base string) string {
	label := base
	for n := 2; taken[label]; n++ {
		label = fmt.Sprintf("%s-%d", base, n)
	}
	taken[label] = true
	return label
}

// hiddenOrVendorDir names directories the indexer never descends into.
var hiddenOrVendorDir = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

// walkRoot discovers markdown files under root. It does not follow symlinks
// for either directories or files during the walk — fs.WalkDir already
// doesn't descend into a symlinked directory, and a symlinked file is
// skipped outright here — so indexing can never itself be tricked into
// walking outside root. (Defense in depth only: the request-time
// ResolveInRoot chain in security.go re-verifies every serve regardless of
// what the index contains.)
func walkRoot(root Root) ([]Doc, error) {
	var docs []Doc
	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root.Path && (hiddenOrVendorDir[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never index a symlinked file; see doc comment above
		}
		if !AllowedExt(path) {
			return nil
		}

		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		docs = append(docs, Doc{
			RootLabel: root.Label,
			RelPath:   filepath.ToSlash(rel),
			Title:     titleFor(path, rel),
			ModTime:   info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// titleFor returns a doc's first level-1 Markdown heading ("# Title"), or —
// if none is found in the first 64 lines — its filename without extension.
// This is a cheap scan, not a full parse; render.go's goldmark pass is the
// source of truth for actual rendering.
func titleFor(absPath, relPath string) string {
	f, err := os.Open(absPath)
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for i := 0; i < 64 && scanner.Scan(); i++ {
			line := strings.TrimSpace(scanner.Text())
			if after, ok := strings.CutPrefix(line, "# "); ok {
				if t := strings.TrimSpace(after); t != "" {
					return t
				}
			}
		}
	}
	base := filepath.Base(relPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Roots returns the indexed roots in configuration order.
func (idx *Index) Roots() []Root {
	out := make([]Root, len(idx.roots))
	copy(out, idx.roots)
	return out
}

// List returns every indexed doc, most-recently-modified first.
func (idx *Index) List() []Doc {
	out := make([]Doc, len(idx.docs))
	copy(out, idx.docs)
	return out
}

// Find returns the indexed Doc for (rootLabel, relPath), if any. Used to
// look up display metadata (Title) for a doc the caller already resolved
// through Resolve — Find itself performs no traversal check, so callers
// MUST NOT use it as a substitute for Resolve when deciding whether to serve
// a file.
func (idx *Index) Find(rootLabel, relPath string) (Doc, bool) {
	for _, d := range idx.docs {
		if d.RootLabel == rootLabel && d.RelPath == relPath {
			return d, true
		}
	}
	return Doc{}, false
}

// ErrRootNotFound indicates a request named a root label the index doesn't
// have.
var ErrRootNotFound = errors.New("no such docs root")

// Resolve maps a (rootLabel, relPath) URL pair to a safe, on-disk absolute
// path: it looks up rootLabel among the indexed Roots, then runs relPath
// through the full ResolveInRoot traversal chain (security.go) against that
// root's canonical path, and finally checks the resolved path's extension
// against AllowedExt. For a single-file root (Root.OnlyFile set), any
// resolution other than that exact file is rejected — naming one file on
// the command line must not grant access to its siblings. Any failure
// returns a wrapped error; the HTTP layer maps all of them to 404 without
// distinguishing the cause to the client.
func (idx *Index) Resolve(rootLabel, relPath string) (string, error) {
	for _, r := range idx.roots {
		if r.Label != rootLabel {
			continue
		}
		resolved, err := ResolveInRoot(r.Path, relPath)
		if err != nil {
			return "", err
		}
		if r.OnlyFile != "" && resolved != r.OnlyFile {
			return "", ErrOutsideRoot
		}
		if !AllowedExt(resolved) {
			return "", ErrDisallowedExt
		}
		return resolved, nil
	}
	return "", ErrRootNotFound
}
