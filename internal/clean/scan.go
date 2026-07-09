package clean

// Test plan for scan.go
//
// kindForName (Classification: pure function)
//   [x] Happy: every one of PR-1's ten target dir basenames maps to its
//       documented Kind (node/python/go/build)
//   [x] Edge: an unrecognized basename is not a target
//
// matchesTypeFilter (Classification: pure function)
//   [x] Happy: an empty filter matches every Kind
//   [x] Happy: a non-empty filter matches only its listed Kinds
//
// ParseKind (Classification: pure function)
//   [x] Happy: each of the four --type values round-trips
//   [x] Edge: an unrecognized value is a clear error, not a silent no-filter
//
// dirSize (Classification: pure function over a temp-dir fixture)
//   [x] Happy: sums regular file sizes recursively under a directory
//   [x] Edge: a broken symlink inside the tree doesn't abort the sum
//
// Scan (Classification: core scan logic, temp-dir fixture, no exec.Runner)
//   [x] Happy: a matched target dir is found and its size is the sum of the
//       files inside it
//   [x] Edge: Scan never descends into a matched dir — a target-named dir
//       nested INSIDE a match is not double-counted as a second target
//   [x] Edge: .git is never a target and never descended into, even nested
//       inside a directory that happens to share a target name (e.g. a repo
//       literally named "dist")
//   [x] Edge: a matched dir that is ITSELF a git working tree (.git as its
//       own immediate child) is never added to the Report at all — not even
//       as a --force-overridable skip, since guarantee #1 is unconditional
//   [x] Edge: a matched dir containing a git working tree NESTED deeper
//       inside it (e.g. build/my-experiment/.git — a real checkout placed
//       inside a dir that happens to match a reclaim name) is likewise never
//       added to the Report — the immediate-child case alone isn't enough
//   [x] Edge: a symlinked directory is never followed — matched-name or not,
//       inside or pointing outside root
//   [x] Edge: --older-than excludes a target newer than the cutoff from the
//       report, but still prunes descent into it
//   [x] Edge: --type filters which Kinds are collected, but still prunes
//       descent into every matched name regardless of type
//   [x] Edge: findProjectRoot finds the nearest ancestor .git for a target,
//       and returns "" when none exists within root

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Kind is the dep/build-dir ecosystem category a scanned target belongs to —
// what the `--type node|python|go|build` flag filters against.
type Kind string

const (
	KindNode   Kind = "node"
	KindPython Kind = "python"
	KindGo     Kind = "go"
	KindBuild  Kind = "build"
)

// targetDirNames maps each PR-1-scope reclaimable directory basename (issue
// #4's dep/build-dir list) to the Kind --type filters against.
//
// Three of the ten names don't map cleanly to a single ecosystem, and PR-1
// has no --type rust/java, so the bucketing is a deliberate call, documented
// here rather than left implicit:
//   - node:   node_modules, .next, .svelte-kit — JS/TS package managers and
//     their two most common framework build dirs.
//   - python: .venv, venv, __pycache__
//   - go:     vendor, target — vendor is unambiguous (go mod vendor); target
//     is predominantly a Rust (cargo) or Maven convention, not Go's. There is
//     no --type rust/java in PR-1's scope, so it's bucketed under go as the
//     nearest systems-language category rather than left unmatched. Revisit
//     if/when a dedicated rust/java --type is added.
//   - build:  dist, build — ecosystem-agnostic bundler/compiler output names
//     used across multiple toolchains; bucketed as a catch-all rather than
//     guessed into node.
var targetDirNames = map[string]Kind{
	"node_modules": KindNode,
	".next":        KindNode,
	".svelte-kit":  KindNode,
	".venv":        KindPython,
	"venv":         KindPython,
	"__pycache__":  KindPython,
	"vendor":       KindGo,
	"target":       KindGo,
	"dist":         KindBuild,
	"build":        KindBuild,
}

// kindForName reports the Kind a directory basename matches, if any.
func kindForName(name string) (Kind, bool) {
	k, ok := targetDirNames[name]
	return k, ok
}

// ParseKind parses a --type flag value into a Kind. Callers treat an empty
// string as "no filter" before ever calling this — ParseKind itself has no
// notion of "any".
func ParseKind(s string) (Kind, error) {
	switch Kind(s) {
	case KindNode, KindPython, KindGo, KindBuild:
		return Kind(s), nil
	default:
		return "", fmt.Errorf("unknown --type %q (want one of: node, python, go, build)", s)
	}
}

// matchesTypeFilter reports whether kind passes types. An empty types slice
// is the --type flag's default (unset) and matches every Kind.
func matchesTypeFilter(kind Kind, types []Kind) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if t == kind {
			return true
		}
	}
	return false
}

// Target is one reclaimable directory Scan found.
type Target struct {
	Path        string // absolute path to the matched directory
	Kind        Kind
	Size        int64     // recursive size in bytes, at scan time
	ModTime     time.Time // the matched directory's own mtime
	ProjectRoot string    // nearest ancestor containing .git; "" if none within Root
}

// ScanOptions configures Scan.
type ScanOptions struct {
	// Root is the directory to scan. Must already be an absolute, symlink-
	// resolved path — Scan itself does no expansion or resolution; Client.Clean
	// owns that so every downstream containment check compares against the
	// same resolved root Scan actually walked.
	Root string
	// Types filters which Kinds are collected. Empty means every Kind.
	Types []Kind
	// OlderThan filters out a target dir whose mtime is more recent than this
	// duration ago. Zero means no age filter.
	OlderThan time.Duration
}

// Report is Scan's result: every matched (and filter-passing) target, plus
// the aggregate size across all of them.
type Report struct {
	Targets   []Target
	TotalSize int64
}

// Scan walks opts.Root ONCE, collecting every directory whose basename
// matches a known reclaimable dep/build dir. It never descends into a match
// (fs.SkipDir) — the same "prune on match" shape issue #4 cites from the
// shell `find ROOT -type d \( -name node_modules -o … \) -prune` pattern,
// reimplemented here in Go with filepath.WalkDir rather than shelling out.
//
// WalkDir + SkipDir was chosen over exec.Runner + `find` for three reasons:
// it needs no Runner at all, so this file stays fully pure and unit-testable
// against a temp-dir fixture with no fake process double; it behaves
// identically on macOS/Linux/Windows, where a bundled `find` binary and its
// flag dialect are not guaranteed; and it keeps the memory-safety guarantees
// of Go's standard library on a path that ends in os.RemoveAll, rather than
// parsing subprocess output back into paths we're about to delete. The
// tradeoff is a small constant-factor slower walk than a C `find` binary on
// a very large tree — acceptable for a --root scoped to a projects
// directory, not a whole filesystem.
//
// Two of the four non-negotiable safety guarantees live here:
//   - .git is never matched or descended into (guarantee #1) — checked
//     BEFORE the target-name match, so a repo literally named "dist" still
//     never has its .git touched.
//   - a symlinked directory is never followed or descended into (guarantee
//     #2) — filepath.WalkDir already never follows symlinks on its own
//     (a symlink DirEntry's Type() carries ModeSymlink, not ModeDir, so
//     WalkDir doesn't recurse into it), but this walk refuses explicitly
//     with fs.SkipDir rather than relying on that being merely the library
//     default, so the guarantee is visible and testable in one place instead
//     of implied by an unlinked stdlib behavior.
func Scan(opts ScanOptions) (Report, error) {
	var report Report

	walkErr := filepath.WalkDir(opts.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A permission-denied or vanished entry mid-walk doesn't abort
			// the whole scan — best-effort, skip it and keep going.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == opts.Root {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Safety guarantee #2.
			return fs.SkipDir
		}
		if d.Name() == ".git" {
			// Safety guarantee #1.
			return fs.SkipDir
		}

		kind, ok := kindForName(d.Name())
		if !ok {
			return nil
		}

		if hasNestedGit(path) {
			// Guarantee #1, strict form: a matched dir that contains a git
			// working tree ANYWHERE inside it — as its own immediate .git
			// (e.g. a dependency someone accidentally vendored with its VCS
			// metadata intact, or a repo that just happens to be named
			// "dist"/"build"/etc.), or nested deeper (e.g.
			// build/my-experiment/.git — a real checkout someone happened to
			// place inside a dir that also matches a reclaim name) — is
			// never treated as a reclaim target at all, unconditionally.
			// This is deliberately NOT the same gate as the
			// (force-overridable) dirty-tree check in clean.go: "never
			// delete .git, not the directory itself, not anything inside it"
			// is one of the four non-negotiable guarantees, so it can't be
			// bypassed by --force the way an ordinary dirty tree can. The dir
			// is simply never added to the Report — not listed, not sized,
			// not skipped-with-a-reason — because it was never a candidate.
			// A nested .git this deep is exactly the case a bare
			// immediate-child check (the prior hasOwnGit) missed: Scan
			// prunes descent on a name match BEFORE this point, so nothing
			// downstream (findProjectRoot's upward-only walk, delete's
			// containsGitComponent on the target's OWN path) ever sees it —
			// this check is the only place in the package positioned to
			// catch it, which is why it has to look downward, not just at
			// the immediate child.
			return fs.SkipDir
		}

		// Matched by name: prune descent regardless of whether the type/age
		// filters below end up excluding it from the report (the func always
		// returns fs.SkipDir past this point). A matched dir's internals are
		// never worth walking further either way — this is the perf win
		// issue #4 cites, applied uniformly rather than only when the match
		// happens to be reported.
		var modTime time.Time
		if info, ierr := d.Info(); ierr == nil {
			modTime = info.ModTime()
		}

		if matchesTypeFilter(kind, opts.Types) && (opts.OlderThan <= 0 || time.Since(modTime) >= opts.OlderThan) {
			size := dirSize(path)
			report.Targets = append(report.Targets, Target{
				Path:        path,
				Kind:        kind,
				Size:        size,
				ModTime:     modTime,
				ProjectRoot: findProjectRoot(opts.Root, path),
			})
			report.TotalSize += size
		}

		return fs.SkipDir
	})
	if walkErr != nil {
		return Report{}, walkErr
	}
	return report, nil
}

// dirSize sums the size of every regular file under root, recursively. A
// walk error on any individual entry (permission denied, a file that
// vanished mid-walk) is swallowed — this is a best-effort reclaimable-size
// ESTIMATE for the dry-run report, not an exact accounting; the bytes a
// --apply run reports as actually reclaimed come from summing this same
// estimate only for directories os.RemoveAll actually succeeded on, in
// clean.go. Symlinks are skipped entirely rather than counted: a symlink's
// own Lstat size is the byte length of its target STRING, not a meaningful
// disk-usage figure, and a broken symlink's target can't be stat'd at all.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// hasNestedGit reports whether dir contains a .git entry anywhere within
// it — as its own immediate child, or nested arbitrarily deep (e.g. a real
// checkout someone placed inside a dir that happens to match a reclaim
// name, like build/my-experiment/.git) — used by Scan to unconditionally
// refuse a matched target containing a git working tree anywhere inside
// (guarantee #1's strict form; see the call site's comment). Matches a
// .git DIRECTORY (a normal repo root) or a .git FILE (a worktree/submodule
// gitdir pointer) — either one means real, addressable git state lives
// there.
//
// This is the one deliberate exception to Scan's own prune-on-match
// performance strategy: it walks dir's full subtree rather than stopping at
// the first level, because correctness on a path that ends in os.RemoveAll
// outweighs the extra walk cost for the (typically much smaller) set of
// dirs that actually match a reclaim name in the first place. The walk
// exits at the first .git found (fs.SkipAll) rather than always completing.
func hasNestedGit(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A permission-denied or vanished entry doesn't change the
			// answer either way — best-effort, keep walking.
			return nil
		}
		if d.Name() == ".git" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// findProjectRoot walks up from path (a matched target dir) toward root
// looking for the nearest ancestor containing a .git entry — the directory
// clean's dirty-tree check (safety guarantee #3) runs `git status` against.
// Returns "" when no ancestor between root and path has one (e.g. an
// unversioned scratch directory) — Client.Clean treats that as "no git tree
// to protect" rather than as "dirty".
func findProjectRoot(root, path string) string {
	dir := filepath.Dir(path) // one level up from the matched dir itself
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if dir == root || dir == filepath.Dir(dir) {
			return ""
		}
		dir = filepath.Dir(dir)
	}
}
