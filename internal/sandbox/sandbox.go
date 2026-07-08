// Package sandbox is the ops layer for isolating a repo@ref checkout into a
// throwaway workspace — the worktree/clone half of workflow's clean-room
// control (ADR-0003), promoted here so internal/pr can share it without
// depending on internal/workflow's step-runner plumbing.
package sandbox

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

// Sandbox creates an isolated checkout of repo@ref in a fresh os.MkdirTemp dir
// and returns its path. alwaysClone forces `git clone` even for a local repo;
// otherwise a local repo uses a cheap `git worktree add` and a remote clones.
// repo and ref are guarded by RejectOptionLike before reaching git as
// positionals.
func Sandbox(ctx context.Context, run exec.Runner, repo, ref string, alwaysClone bool) (string, error) {
	if repo == "" {
		slog.Warn("Sandbox missing required repo.")
		return "", errors.New("worktree/clone step requires repo")
	}
	// A workflow file's repo/ref reach git as positional args. A leading '-'
	// would be parsed as a git option (e.g. repo="--upload-pack=…" turns a
	// clone into arbitrary command execution). Workflow files are shared and,
	// in the spike, unsigned (#10), so reject option-like values outright.
	if err := RejectOptionLike("repo", repo); err != nil {
		return "", err
	}
	if err := RejectOptionLike("ref", ref); err != nil {
		return "", err
	}
	slog.Debug("Preparing to create workspace sandbox.", "repo", repo, "ref", ref, "alwaysClone", alwaysClone)

	dir, err := os.MkdirTemp("", "forgectl-workflow-*")
	if err != nil {
		slog.Error("Failed to create sandbox directory.", "error", err)
		return "", fmt.Errorf("create sandbox dir: %w", err)
	}
	slog.Debug("Created sandbox directory.", "sandbox", dir)

	if !alwaysClone && isLocalRepo(repo) {
		useRef := ref
		if useRef == "" {
			useRef = "HEAD"
		}
		slog.Debug("Sandboxing local repo via git worktree.", "repo", repo, "ref", useRef)
		// -- ends option parsing so a crafted dir/ref can't inject a flag.
		if _, err := run.Run(ctx, "git", "-C", repo, "worktree", "add", "--", dir, useRef); err != nil {
			slog.Error("Failed to create git worktree.", "repo", repo, "sandbox", dir, "ref", useRef, "error", err)
			return "", fmt.Errorf("git worktree add: %w", err)
		}
	} else {
		slog.Debug("Sandboxing repo via git clone.", "repo", repo, "ref", ref)
		// Clone the default branch when no ref was given; git clone --branch
		// wants a real branch/tag name, so "HEAD" can't stand in for it. The --
		// separator ends option parsing before the repo/dir positionals.
		args := []string{"clone", "--", repo, dir}
		if ref != "" {
			args = []string{"clone", "--branch", ref, "--", repo, dir}
		}
		if _, err := run.Run(ctx, "git", args...); err != nil {
			slog.Error("Failed to clone repo.", "repo", repo, "sandbox", dir, "error", err)
			return "", fmt.Errorf("git clone: %w", err)
		}
	}

	slog.Debug("Successfully created workspace sandbox.", "repo", repo, "workspace", dir)
	return dir, nil
}

// isLocalRepo reports whether repo looks like a filesystem path (vs. an
// owner/repo remote reference) — an absolute/relative path, or one that
// exists on disk.
func isLocalRepo(repo string) bool {
	if strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "./") || strings.HasPrefix(repo, "../") || repo == "." {
		return true
	}
	if _, err := os.Stat(repo); err == nil {
		return true
	}
	return false
}

// Teardown removes a sandbox dir. Idempotent: an empty workspace or an
// already-removed dir is not an error.
func Teardown(_ context.Context, _ exec.Runner, workspace string) error {
	if workspace == "" {
		slog.Debug("Teardown: no workspace, nothing to remove.")
		return nil
	}
	slog.Debug("Preparing to tear down workspace.", "workspace", workspace)
	if err := os.RemoveAll(workspace); err != nil {
		slog.Error("Failed to tear down workspace.", "workspace", workspace, "error", err)
		return fmt.Errorf("teardown %s: %w", workspace, err)
	}
	slog.Debug("Successfully tore down workspace.", "workspace", workspace)
	return nil
}

// WithinWorkspace reports whether target, after resolving symlinks, stays
// inside workspace. filepath.Glob can match a symlink whose target escapes the
// sandbox; deleting through it would reach outside workspace, so every match
// should be re-checked here before removal.
func WithinWorkspace(workspace, target string) bool {
	realWS, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		realWS = workspace
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		realTarget = target
	}
	rel, err := filepath.Rel(realWS, realTarget)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RejectOptionLike guards a value that becomes a positional git argument: a
// leading '-' would be parsed as a git option, so an unsigned shared workflow
// could smuggle a flag (e.g. --upload-pack) into a clone/worktree invocation.
func RejectOptionLike(field, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("workflow %s %q must not begin with '-'", field, value)
	}
	return nil
}
