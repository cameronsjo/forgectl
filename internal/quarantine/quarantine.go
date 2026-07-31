// Package quarantine is the ops layer for reversibly hiding AI-instruction
// files (CLAUDE.md, AGENTS.md, …) from a workspace via os.Rename — distinct
// from workflow's `strip` step, which destructively os.RemoveAll's them for a
// throwaway clean-room sandbox. Quarantine is meant to be undone: Hide moves a
// target aside, Restore moves it back.
package quarantine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Scheme selects how a quarantined path is renamed.
type Scheme int

const (
	// PrefixUnderscore renames CLAUDE.md -> _CLAUDE.md.
	PrefixUnderscore Scheme = iota
	// SuffixQuarantined renames CLAUDE.md -> CLAUDE.md.quarantined.
	SuffixQuarantined
)

// String renders scheme as the CLI flag value that selects it.
func (s Scheme) String() string {
	switch s {
	case SuffixQuarantined:
		return "suffix"
	default:
		return "prefix"
	}
}

// ParseScheme parses a `--scheme` flag value ("prefix" or "suffix").
func ParseScheme(s string) (Scheme, error) {
	switch s {
	case "", "prefix":
		return PrefixUnderscore, nil
	case "suffix":
		return SuffixQuarantined, nil
	default:
		return PrefixUnderscore, fmt.Errorf("unknown quarantine scheme %q (want prefix or suffix)", s)
	}
}

// Move records one reversible rename: From is the original path, To is the
// quarantined path. Restore reverses it.
type Move struct {
	From string
	To   string
}

// DefaultTargets is the canonical list of AI-instruction paths (relative to a
// workspace root) that quarantine hides by default, and that workflow's
// `strip` step falls back to when a step omits `globs`.
//
// Entries are literal relative paths. The two that agents read RECURSIVELY as
// they descend a tree — see nestableBasenames — are expanded to every nested
// match by ExpandTargets; the rest are root-level only, which is where they
// are actually read from.
var DefaultTargets = []string{
	"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".claude/", ".cursor/rules", ".github/copilot-instructions.md",
}

// nestableBasenames are the instruction-file basenames a coding agent picks up
// from ANY directory as it descends, not just the workspace root. Quarantining
// only the root pair leaves a PR head carrying `src/AGENTS.md` or
// `packages/api/CLAUDE.md` free to inject instructions into the review — the
// exact thing the clean room exists to prevent.
var nestableBasenames = map[string]bool{
	"CLAUDE.md":       true,
	"CLAUDE.local.md": true,
	"AGENTS.md":       true,
}

// skipDirNames are directories ExpandTargets never descends into. `.git` holds
// no instruction file an agent reads, and walking a large object store on every
// hide/restore/status is pure cost.
var skipDirNames = map[string]bool{
	".git": true,
}

// ExpandTargets resolves targets against root, replacing each bare nestable
// basename ("CLAUDE.md", "AGENTS.md") with every match found by walking root.
// Other entries — and any entry carrying a directory component, which is an
// explicit path the caller meant literally — pass through untouched.
//
// It is direction-agnostic BY DESIGN, and that is what keeps quarantine
// reversible. A match is recorded when the walk finds either the original
// basename or its renamed form under scheme, and the returned path is always
// the ORIGINAL relative path. So the same call produces the same target list
// before Hide (tree holds `src/AGENTS.md`) and at teardown (tree holds
// `src/AGENTS.md.quarantined`), and ComputeMoves reproduces exactly the Move
// pairs Hide made.
//
// Each literal entry is preserved even with zero nested matches, so a caller
// that pairs targets with moves by index (e.g. `quarantine status`) keeps a row
// for a target that is simply absent.
//
// A walk error is not fatal: an unreadable subtree yields no matches rather
// than failing the whole operation, which would strand a workspace mid-review.
// Errors reaching root itself are returned.
func ExpandTargets(root string, scheme Scheme, targets []string) ([]string, error) {
	var nested []string
	if wantsExpansion(targets) {
		var err error
		if nested, err = walkNestable(root, scheme, targets); err != nil {
			return nil, err
		}
	}

	out := make([]string, 0, len(targets)+len(nested))
	seen := make(map[string]bool, len(targets)+len(nested))
	for _, t := range append(append([]string{}, targets...), nested...) {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}

// wantsExpansion reports whether targets contains at least one bare nestable
// basename — the only case that needs a walk at all.
func wantsExpansion(targets []string) bool {
	for _, t := range targets {
		if nestableBasenames[t] {
			return true
		}
	}
	return false
}

// walkNestable returns the original relative paths of every NESTED match for
// the nestable basenames present in targets. Root-level hits are excluded:
// they are already in targets verbatim.
func walkNestable(root string, scheme Scheme, targets []string) ([]string, error) {
	// Keyed on the LOWERCASED basename. APFS and NTFS are case-insensitive by
	// default, so an exact-string match is not the same predicate the agent
	// uses: a head carrying `src/agents.md` is missed by an exact walk, while
	// the reviewer's open("src/AGENTS.md") resolves to it and reads the
	// instructions. Matching case-insensitively everywhere costs nothing on a
	// case-sensitive filesystem (a genuinely distinct `agents.md` is also
	// quarantined, which is the safe direction) and closes the vector on the
	// filesystems forgectl actually runs on.
	//
	// The root level was already covered by accident — Hide's os.Lstat matches
	// case-insensitively — so only this walk was exposed.
	want := make(map[string]bool, len(targets)*2) // lowercased on-disk names to match
	for _, t := range targets {
		if !nestableBasenames[t] {
			continue
		}
		want[strings.ToLower(t)] = true
		want[strings.ToLower(filepath.Base(renamedPath(scheme, t)))] = true
	}

	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable subtree: skip it, don't fail the operation
		}
		if d.IsDir() {
			if path != root && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !want[strings.ToLower(d.Name())] {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			return nil // root-level hit: already carried literally in targets
		}
		// Return the ACTUAL on-disk spelling, not the canonical target
		// basename. Emitting "AGENTS.md" for a file named "agents.md" would
		// rename it to AGENTS.md.quarantined on a case-insensitive filesystem
		// (silently changing its case across the round-trip) and miss it
		// entirely on a case-sensitive one.
		found = append(found, filepath.Join(dir, undecorate(scheme, d.Name())))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s for nested instruction files: %w", root, err)
	}
	sort.Strings(found)
	return found, nil
}

// Client hides and restores instruction files under a root directory. It
// carries an exec.Runner for consistency with the rest of the ops layer
// (New(run exec.Runner) *Client), though Hide/Restore rename files directly
// via os.Rename rather than shelling out.
type Client struct {
	run exec.Runner
}

// New builds a Client.
func New(run exec.Runner) *Client {
	return &Client{run: run}
}

// Hide renames each of targets (paths relative to root) aside per scheme,
// returning the reversible Moves it made (or, in dry-run, would make). Every
// target is validated and its resolved path checked to stay within root
// BEFORE any rename — a ".."/absolute target, or a target whose resolved
// (symlink-following) path escapes root, is rejected with zero filesystem
// mutation. A missing target is a no-op: it is skipped, not an error.
func (c *Client) Hide(_ context.Context, root string, scheme Scheme, targets []string, dryRun bool) ([]Move, error) {
	slog.Debug("Preparing to quarantine instruction files.", "root", root, "scheme", scheme, "targetCount", len(targets), "dryRun", dryRun)
	var moves []Move
	for _, target := range targets {
		move, err := computeMove(root, scheme, target)
		if err != nil {
			slog.Warn("Invalid quarantine target.", "target", target, "error", err)
			return nil, err
		}

		if _, err := os.Lstat(move.From); err != nil {
			if os.IsNotExist(err) {
				slog.Debug("Quarantine target missing; skipping.", "target", target)
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", move.From, err)
		}

		// A target with no ".." can still reach outside root via a symlink;
		// re-check the resolved path before any mutation (mirrors workflow's
		// withinWorkspace guard on strip matches).
		if !withinRoot(root, move.From) {
			slog.Error("Quarantine target escapes root; refusing.", "target", target, "resolved", move.From)
			return nil, fmt.Errorf("quarantine target %q escapes root", target)
		}

		// os.Rename silently overwrites its destination, so a checkout crafted to
		// contain both CLAUDE.md and CLAUDE.md.quarantined would lose the latter.
		// Fetched PR content is hostile input; fail loud rather than clobber.
		if _, err := os.Lstat(move.To); err == nil {
			slog.Error("Quarantine destination already exists; refusing to clobber.", "target", target, "destination", move.To)
			return nil, fmt.Errorf("quarantine destination %q already exists", move.To)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", move.To, err)
		}

		if dryRun {
			moves = append(moves, move)
			continue
		}

		if err := os.Rename(move.From, move.To); err != nil {
			slog.Error("Failed to quarantine target.", "target", target, "error", err)
			return nil, fmt.Errorf("rename %s: %w", move.From, err)
		}
		moves = append(moves, move)
	}
	slog.Info("Successfully quarantined instruction files.", "root", root, "moved", len(moves), "dryRun", dryRun)
	return moves, nil
}

// Restore reverses each Move (quarantined path -> original path). It is
// idempotent: a Move whose quarantined path (To) no longer exists is a
// no-op, so Restore is safe to call twice (or on a partially-restored set).
func (c *Client) Restore(_ context.Context, moves []Move) error {
	slog.Debug("Preparing to restore quarantined files.", "moveCount", len(moves))
	for _, m := range moves {
		if _, err := os.Lstat(m.To); err != nil {
			if os.IsNotExist(err) {
				slog.Debug("Quarantine move already restored; skipping.", "to", m.To)
				continue
			}
			return fmt.Errorf("stat %s: %w", m.To, err)
		}
		// Symmetric with Hide's anti-clobber guard: os.Rename silently
		// overwrites. Teardown recomputes targets from the LIVE tree, so a file
		// the review itself created at x/CLAUDE.md.quarantined would otherwise
		// be renamed over a sibling x/CLAUDE.md. Confined to a disposable
		// workspace today, but "restore" must never destroy.
		if _, err := os.Lstat(m.From); err == nil {
			slog.Error("Restore destination already exists; refusing to clobber.", "from", m.From, "to", m.To)
			return fmt.Errorf("restore destination %q already exists", m.From)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", m.From, err)
		}
		if err := os.Rename(m.To, m.From); err != nil {
			slog.Error("Failed to restore quarantined file.", "from", m.From, "to", m.To, "error", err)
			return fmt.Errorf("rename %s: %w", m.To, err)
		}
	}
	slog.Info("Successfully restored quarantined files.", "moveCount", len(moves))
	return nil
}

// ComputeMoves resolves each target (validated, relative to root) to its Move
// pair without touching the filesystem — used by callers (e.g. the CLI's
// restore/status verbs) that need the same From/To mapping Hide computes,
// without performing a Hide.
func ComputeMoves(root string, scheme Scheme, targets []string) ([]Move, error) {
	moves := make([]Move, 0, len(targets))
	for _, target := range targets {
		move, err := computeMove(root, scheme, target)
		if err != nil {
			return nil, err
		}
		moves = append(moves, move)
	}
	return moves, nil
}

// computeMove validates target and resolves it (and its renamed form) against
// root, without touching the filesystem.
func computeMove(root string, scheme Scheme, target string) (Move, error) {
	if err := validateQuarantineTarget(target); err != nil {
		return Move{}, err
	}
	cleanRel := filepath.Clean(target)
	return Move{
		From: filepath.Join(root, cleanRel),
		To:   filepath.Join(root, renamedPath(scheme, cleanRel)),
	}, nil
}

// undecorate is the inverse of renamedPath's basename transform: given a name
// the walk matched, it returns the pre-quarantine spelling. A name that is not
// decorated (the pre-Hide case) is returned unchanged, so the walk yields the
// same original-relative path in both directions — the property ExpandTargets
// depends on for reversibility.
func undecorate(scheme Scheme, base string) string {
	switch scheme {
	case SuffixQuarantined:
		return strings.TrimSuffix(base, ".quarantined")
	default:
		return strings.TrimPrefix(base, "_")
	}
}

// renamedPath applies scheme to the base name of a cleaned relative path,
// leaving any parent directory component untouched — a nested target like
// ".github/copilot-instructions.md" renames only its base name.
func renamedPath(scheme Scheme, cleanRel string) string {
	dir, base := filepath.Split(cleanRel)
	var newBase string
	switch scheme {
	case SuffixQuarantined:
		newBase = base + ".quarantined"
	default:
		newBase = "_" + base
	}
	return filepath.Join(dir, newBase)
}

// withinRoot reports whether target, after resolving symlinks, stays inside
// root. Ported from workflow's withinWorkspace: a target with no ".." can
// still be a symlink pointing outside root, so every match is re-checked here
// before any rename.
func withinRoot(root, target string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		realTarget = target
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateQuarantineTarget rejects a target that could escape root: an
// absolute path, or any ".." path-traversal segment. Ported from workflow's
// validateStripGlob.
func validateQuarantineTarget(target string) error {
	if target == "" {
		return errors.New("quarantine target must not be empty")
	}
	if filepath.IsAbs(target) {
		return fmt.Errorf("quarantine target %q must not be absolute", target)
	}
	normalized := strings.ReplaceAll(filepath.Clean(target), "\\", "/")
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return fmt.Errorf("quarantine target %q must not traverse outside root", target)
		}
	}
	return nil
}
