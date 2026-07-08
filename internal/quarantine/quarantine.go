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
	"log/slog"
	"os"
	"path/filepath"
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
var DefaultTargets = []string{
	"CLAUDE.md", "AGENTS.md", ".claude/", ".cursor/rules", ".github/copilot-instructions.md",
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
