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
	"github.com/cameronsjo/forgectl/internal/sandbox"
	"github.com/cameronsjo/forgectl/internal/step"
)

// Steps is quarantine's workflow step contribution (ADR-0005): the `strip`
// verb — the destructive sibling of this package's reversible Hide, built
// from the same canonical DefaultTargets so the two controls never drift.
// defaultGlobs is the fallback strip-list for a step that omits `globs`
// (wired from config's [workflow] strip_globs by the CLI layer); empty falls
// back to DefaultTargets.
func Steps(defaultGlobs []string) step.Registry {
	globs := defaultGlobs
	if len(globs) == 0 {
		globs = DefaultTargets
	}
	return step.Registry{
		// Globs is guarded (step.Def.GuardedFields): the strip-list IS the
		// clean-room control, so a ${param} in it would let an agent narrow the
		// redaction set at run time against an already-blessed file — smuggling a
		// repo's CLAUDE.md past the strip and into the reviewer.
		"strip": {Runner: newStripStep(globs), GuardedFields: []string{"Globs"}},
	}
}

// newStripStep builds the `strip` step.Runner, closing over the default
// strip-list to fall back to when a step omits `globs` (design doc: "omit
// globs → configured default set"). Globs are resolved ONLY inside
// ${workspace} — a path-escape guard rejects any glob containing ".." or an
// absolute path, per ADR-0003's "correctness-and-security requirement, spike
// or not".
func newStripStep(defaultGlobs []string) step.Runner {
	return func(_ context.Context, _ exec.Runner, wctx *step.Context, s step.PlanStep) error {
		workspace, ok := wctx.Get("workspace")
		if !ok || workspace == "" {
			slog.Warn("Strip step missing workspace context (requires worktree/clone step to run first).")
			return errors.New("strip step requires ${workspace} (run after a worktree/clone step)")
		}

		globs := s.Globs
		if len(globs) == 0 {
			globs = defaultGlobs
		}

		slog.Debug("Preparing to strip paths from workspace.", "workspace", workspace, "globCount", len(globs), "globs", globs)
		for _, g := range globs {
			if err := validateStripGlob(g); err != nil {
				slog.Warn("Invalid strip glob.", "glob", g, "error", err)
				return err
			}
			// Expand the pattern so a real glob (e.g. *.md) removes every match,
			// not only a file literally named "*.md". The strip-list is a
			// security control, so under-stripping would weaken the clean-room
			// defense. A literal entry that doesn't exist yields no matches — a
			// no-op, matching the pre-glob behavior.
			matches, err := filepath.Glob(filepath.Join(workspace, filepath.Clean(g)))
			if err != nil {
				slog.Warn("Bad strip pattern.", "glob", g, "error", err)
				return fmt.Errorf("strip pattern %q: %w", g, err)
			}
			for _, target := range matches {
				// A pattern with no ".." can still reach outside via a symlink
				// (e.g. a matched dir that links to /etc); re-check every match's
				// real path before deleting through it.
				if !sandbox.WithinWorkspace(workspace, target) {
					slog.Error("Strip match escapes workspace; refusing.", "glob", g, "target", target)
					return fmt.Errorf("strip match %q escapes workspace", target)
				}
				slog.Debug("Removing path.", "glob", g, "target", target)
				if err := os.RemoveAll(target); err != nil {
					slog.Error("Failed to remove path.", "glob", g, "target", target, "error", err)
					return fmt.Errorf("strip %s: %w", g, err)
				}
			}
		}
		slog.Debug("Successfully stripped paths from workspace.", "workspace", workspace, "globCount", len(globs))
		return nil
	}
}

// validateStripGlob rejects a glob that could escape ${workspace}: an
// absolute path, or any ".." path-traversal segment.
func validateStripGlob(g string) error {
	if g == "" {
		return errors.New("strip glob must not be empty")
	}
	if filepath.IsAbs(g) {
		return fmt.Errorf("strip glob %q must not be absolute", g)
	}
	// Normalize Windows separators so a "..\" segment is caught on any OS, then
	// reject any ".." path segment wherever it appears.
	normalized := strings.ReplaceAll(filepath.Clean(g), "\\", "/")
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return fmt.Errorf("strip glob %q must not traverse outside the workspace", g)
		}
	}
	return nil
}
