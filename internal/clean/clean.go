// Package clean is the ops layer for `forgectl clean`: scan.go's pure walk
// finds reclaimable dep/build directories under --root; this file adds the
// two effects that need a seam — git dirty-tree detection (exec.Runner) and
// the actual on-disk deletion (os.RemoveAll, gated by
// sandbox.WithinWorkspace) — plus dry-run/apply orchestration. It knows
// nothing of Cobra — that decoupling is the house pattern (see internal/net,
// internal/branch, internal/docker).
//
// PR-1 scope only: dep/build-dir reclaim. Package-manager caches (npm/pip/go
// build cache/brew) and docker prune are explicitly out of scope here —
// issue #4's follow-on PR.
//
// This is a delete-adjacent package. Every os.RemoveAll call in it is
// preceded by:
//  1. Scan (scan.go) never having matched or descended into .git.
//  2. Scan never having followed a symlink out of --root.
//  3. A dirty-tree skip unless --force (gitDirty below).
//  4. A sandbox.WithinWorkspace containment re-check on the resolved path,
//     immediately before the one os.RemoveAll call in the package (delete
//     below) — never a shelled-out `rm -rf`.
package clean

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// defaultRootSubdir is appended to the user's home directory for New's
// built-in --root default, absent a --root flag or [clean] default_root.
const defaultRootSubdir = "Projects"

// Client scans and reclaims dep/build directories.
type Client struct {
	run exec.Runner

	home  string // resolved once at construction; "" if unresolvable
	root  string // fully resolved default root; "" if unresolvable and unconfigured
	types []Kind // default --type filter; empty means every Kind
}

// Option configures a Client at construction.
type Option func(*Client)

// WithRoot overrides the default root New derives from the user's home
// directory. root may use a leading ~/ — expanded against the resolved home
// directory the same way WithCleanConfig does.
func WithRoot(root string) Option {
	return func(c *Client) {
		if root != "" {
			c.root = expandTilde(root, c.home)
		}
	}
}

// WithTypes overrides the default --type filter New leaves empty (every
// Kind).
func WithTypes(types []Kind) Option {
	return func(c *Client) {
		if len(types) > 0 {
			c.types = types
		}
	}
}

// WithCleanConfig applies the [clean] config section, filling in any field
// left zero with New's built-in default rather than overwriting it.
func WithCleanConfig(cc config.CleanConfig) Option {
	return func(c *Client) {
		if cc.DefaultRoot != "" {
			c.root = expandTilde(cc.DefaultRoot, c.home)
		}
		if cc.DefaultType != "" {
			if k, err := ParseKind(cc.DefaultType); err == nil {
				c.types = []Kind{k}
			} else {
				slog.Warn("Ignoring invalid [clean] default_type in config.", "value", cc.DefaultType, "error", err)
			}
		}
	}
}

// New builds a Client over the given Runner. The default root is
// <home>/Projects; if the home directory can't be resolved, root is left
// empty and Clean requires an explicit CleanOptions.Root.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{run: run}
	if home, err := os.UserHomeDir(); err == nil {
		c.home = home
		c.root = filepath.Join(home, defaultRootSubdir)
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// expandTilde expands a leading ~ or ~/ to home. Mirrors internal/config's
// and internal/launch's identical small helper — kept local per house
// convention (see internal/config/config.go's own comment on why) rather
// than introducing a shared util package for four lines of code.
func expandTilde(path, home string) string {
	if home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// CleanOptions configures Clean.
type CleanOptions struct {
	// Root overrides the Client's configured/default root for this call.
	Root string
	// Types overrides the Client's configured/default --type filter for this
	// call. Empty means every Kind.
	Types []Kind
	// OlderThan filters out targets newer than this. Zero means no filter.
	OlderThan time.Duration
	// Apply deletes reclaimable targets. false (the default) is a dry run:
	// Scan and classify only, os.RemoveAll is never called.
	Apply bool
	// Force skips the dirty-tree check (safety guarantee #3) — a project
	// with uncommitted changes is cleaned instead of skipped.
	Force bool
}

// Item is one scanned target plus what Clean decided/did with it.
type Item struct {
	Target

	// Skipped is true when the target was left alone — currently only ever
	// because its owning project's git tree is dirty and Force was false.
	Skipped    bool
	SkipReason string

	// Deleted is true only once os.RemoveAll actually succeeded (Apply
	// only). Err carries a failed delete attempt.
	Deleted bool
	Err     error
}

// Result is Clean's outcome: every target Scan found, tagged with what
// happened to it, plus running totals.
type Result struct {
	Items []Item

	// TotalScanned is the sum of every matched target's size, skipped or
	// not — what a full, unrestricted reclaim would free.
	TotalScanned int64
	// TotalReclaimable is the sum of every NON-skipped target's size — what
	// this call would delete (dry run) or attempted to delete (--apply).
	TotalReclaimable int64
	// TotalReclaimed is the sum of sizes for targets os.RemoveAll actually
	// succeeded on. Zero for a dry run by construction (Apply gates every
	// delete attempt) — never inferred from TotalReclaimable.
	TotalReclaimed int64
}

// Clean scans opts.Root (or the Client's configured default) for reclaimable
// dep/build dirs, skips anything inside a dirty git tree unless opts.Force,
// and — only when opts.Apply — deletes the rest, reporting actual reclaimed
// bytes. A dry run (the default) never calls os.RemoveAll; TotalReclaimed
// stays zero and every filesystem entry Scan found is left exactly as it
// was — asserted in clean_test.go against a real temp-dir fixture, not just
// documented here.
func (c *Client) Clean(ctx context.Context, opts CleanOptions) (Result, error) {
	root := opts.Root
	if root == "" {
		root = c.root
	}
	if root == "" {
		return Result{}, fmt.Errorf("no root to scan: pass --root, set [clean] default_root, or ensure the home directory is resolvable")
	}
	root = expandTilde(root, c.home)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Result{}, fmt.Errorf("resolve root %s: %w", root, err)
	}
	// Resolve symlinks in root itself up front, once, so every containment
	// check downstream (Scan's walk, delete's WithinWorkspace re-check)
	// compares against the exact same real path.
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		// A root that doesn't exist yet (or a dangling symlink) isn't a
		// reclaim-safety concern — Scan itself will fail below with a clear
		// "no such file" error. Fall back to the unresolved absolute path
		// rather than erroring here.
		resolvedRoot = absRoot
	}

	types := opts.Types
	if len(types) == 0 {
		types = c.types
	}

	slog.Debug("Preparing to scan for reclaimable directories.",
		"root", resolvedRoot, "types", types, "olderThan", opts.OlderThan, "apply", opts.Apply, "force", opts.Force)

	report, err := Scan(ScanOptions{
		Root:      resolvedRoot,
		Types:     types,
		OlderThan: opts.OlderThan,
	})
	if err != nil {
		slog.Error("Failed to scan for reclaimable directories.", "root", resolvedRoot, "error", err)
		return Result{}, fmt.Errorf("scan %s: %w", resolvedRoot, err)
	}

	result := Result{}
	// Memoized per project root: a project with several matched targets
	// (e.g. node_modules AND dist) only needs one `git status` call, not one
	// per target.
	dirtyCache := make(map[string]bool)

	for _, t := range report.Targets {
		item := Item{Target: t}
		result.TotalScanned += t.Size

		if !opts.Force && t.ProjectRoot != "" {
			dirty, cached := dirtyCache[t.ProjectRoot]
			if !cached {
				d, derr := gitDirty(ctx, c.run, t.ProjectRoot)
				if derr != nil {
					// Safety guarantee #3, fail-safe direction: unable to
					// CONFIRM the tree is clean is treated as dirty, not as
					// clean. A `git status` failure (missing git binary,
					// corrupt repo, permissions) must never silently
					// downgrade to "safe to delete".
					slog.Warn("Could not confirm git tree is clean; skipping conservatively.",
						"project", t.ProjectRoot, "target", t.Path, "error", derr)
					d = true
				}
				dirtyCache[t.ProjectRoot] = d
				dirty = d
			}
			if dirty {
				item.Skipped = true
				item.SkipReason = "dirty/uncommitted git tree (pass --force to override)"
				result.Items = append(result.Items, item)
				continue
			}
		}

		result.TotalReclaimable += t.Size

		if opts.Apply {
			if derr := c.delete(resolvedRoot, t.Path); derr != nil {
				item.Err = derr
				slog.Error("Failed to reclaim directory.", "path", t.Path, "error", derr)
			} else {
				item.Deleted = true
				result.TotalReclaimed += t.Size
				slog.Info("Successfully reclaimed directory.", "path", t.Path, "kind", t.Kind, "size", t.Size)
			}
		}
		result.Items = append(result.Items, item)
	}

	slog.Info("Successfully scanned for reclaimable directories.",
		"root", resolvedRoot, "matched", len(report.Targets), "reclaimable", result.TotalReclaimable, "apply", opts.Apply)
	return result, nil
}

// delete removes target after re-validating it resolves to a path still
// inside root — safety guarantee #4. This is deliberate defense-in-depth on
// top of Scan already having refused to descend into .git or any symlinked
// directory: target came from our own Scan, but this is the ONE call in the
// package that actually mutates the filesystem, so it re-checks rather than
// trusting the caller. Always os.RemoveAll on a validated absolute path —
// never a shelled-out `rm -rf`.
func (c *Client) delete(root, target string) error {
	if containsGitComponent(target) {
		// Belt-and-suspenders: Scan can never produce a Target with a .git
		// path component (it refuses to descend into .git before ever
		// reaching the target-name match), but this is the last line of
		// defense before a real deletion, so the invariant is asserted here
		// too rather than trusted from upstream.
		return fmt.Errorf("refusing to delete %s: .git is never a reclaim target", target)
	}
	if !sandbox.WithinWorkspace(root, target) {
		return fmt.Errorf("refusing to delete %s: resolves outside root %s", target, root)
	}

	slog.Debug("Preparing to reclaim directory.", "path", target)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove %s: %w", target, err)
	}
	return nil
}

// containsGitComponent reports whether any path component of target is
// literally ".git" — a stronger, path-component-aware version of "don't
// delete .git" than a basename check, so a hypothetical future caller can't
// hand delete a path like <root>/proj/.git/hooks and have only the basename
// checked.
func containsGitComponent(target string) bool {
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}

// gitDirty runs `git status --porcelain` in dir and reports whether the
// working tree has any uncommitted changes (modified, staged, or untracked
// files) — safety guarantee #3. Mirrors internal/projects' gitStatus shape
// (git -C <dir> status --porcelain), re-implemented locally rather than
// imported: that function is unexported and returns a richer GitStatus
// (Modified/Untracked/Ahead counts) this package doesn't need — clean only
// ever needs a clean/dirty boolean.
func gitDirty(ctx context.Context, run exec.Runner, dir string) (bool, error) {
	out, err := run.Run(ctx, "git", "-C", dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status --porcelain in %s: %w", dir, err)
	}
	return strings.TrimSpace(out) != "", nil
}
