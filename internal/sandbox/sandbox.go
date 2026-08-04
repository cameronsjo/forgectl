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

// WorkspacePrefix is the os.MkdirTemp prefix every sandbox workspace carries.
// Sandbox produces it and Teardown requires it, so the producer and the guard
// read one constant and cannot drift apart. internal/pr's sandboxPrefix
// aliases it for the same reason.
const WorkspacePrefix = "forgectl-workflow-"

// ErrUnsafeTeardown is returned when Teardown refuses a path that is not
// identifiable as a forgectl sandbox workspace. Callers that ignore the error
// still get a slog.Error; nothing is removed.
var ErrUnsafeTeardown = errors.New("path is not a forgectl sandbox workspace")

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

	dir, err := os.MkdirTemp("", WorkspacePrefix+"*")
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
// already-removed dir is not an error. Anything else must look like a
// forgectl sandbox or it is refused with ErrUnsafeTeardown.
//
// LOAD-BEARING — VALIDATE RESOLVED, ACT UNRESOLVED, ENFORCED HERE. This is
// the sink: every caller reaches os.RemoveAll through it, and only the
// breadcrumb path passes pr.validateWorkspace first (workflow's
// cleanupSandbox and teardownStep do not). So the invariant is enforced at
// the sink rather than deferred to one caller's gate.
//
// The two halves and why the split is deliberate:
//
//   - VALIDATE RESOLVED. The prefix test runs on filepath.EvalSymlinks of
//     workspace, so a symlink NAMED with the prefix but POINTING at an
//     unprefixed directory is refused. Testing the literal base name first
//     would let the name alone authorize the removal.
//   - ACT UNRESOLVED. os.RemoveAll gets workspace exactly as recorded, never
//     the resolved path. Removing the resolved path would turn a
//     symlink-unlink into a real deletion of the target — the regression the
//     matching note on pr.validateWorkspace also forbids. Do NOT "tidy" this
//     into acting on what was validated.
//
// EvalSymlinks failure is fail-closed: a dangling symlink is refused rather
// than falling back to the literal name (pr.validateWorkspace falls back
// fail-open at that point; this is deliberately stricter, and the trade is
// that a sandbox whose target vanished must be removed by hand).
//
// TOCTOU, KNOWN AND BOUNDED: between EvalSymlinks and RemoveAll a PARENT
// component repointed after validation redirects the delete to a different
// prefixed directory. The FINAL component is safe — RemoveAll does not follow
// it. Closing this needs a same-uid write to the path, which is the same
// bounded exposure as the accepted symlinked-parent case below, not a new one.
//
// The prefix compare is BYTE-EXACT while APFS is case-insensitive, so
// FORGECTL-WORKFLOW-x is refused. That fails in the LEAK direction — a sandbox
// that needs removing by hand — never the delete direction. Normalizing the
// case here would be a LOOSENING of the guard, not a bug fix.
//
// KNOWN, ACCEPTED, UNCHANGED: a symlinked PARENT component is still followed.
// os.RemoveAll declines to follow only the FINAL component, so a workspace
// recorded as /tmp/plink/forgectl-workflow-x with plink -> $HOME/real still
// deletes $HOME/real/forgectl-workflow-x — it resolves to a prefixed base, so
// the guard passes it by design. Reaching it requires writing a breadcrumb
// into the 0700 session-state dir under $HOME, i.e. same-uid arbitrary write.
func Teardown(_ context.Context, _ exec.Runner, workspace string) error {
	if workspace == "" {
		slog.Debug("Teardown: no workspace, nothing to remove.")
		return nil
	}
	// A relative path's base name can carry the prefix while resolving
	// anywhere off the process cwd, so require an absolute path first.
	if !filepath.IsAbs(workspace) {
		slog.Error("Refused to tear down a relative workspace path.", "workspace", workspace)
		return fmt.Errorf("teardown %s: %w", workspace, ErrUnsafeTeardown)
	}
	if _, err := os.Lstat(workspace); err != nil {
		if os.IsNotExist(err) {
			slog.Debug("Teardown: workspace already removed.", "workspace", workspace)
			return nil
		}
		slog.Error("Refused to tear down an unstattable workspace.", "workspace", workspace, "error", err)
		return fmt.Errorf("teardown %s: %w", workspace, ErrUnsafeTeardown)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		slog.Error("Refused to tear down a workspace that could not be resolved.", "workspace", workspace, "error", err)
		return fmt.Errorf("teardown %s: %w", workspace, ErrUnsafeTeardown)
	}
	if !strings.HasPrefix(filepath.Base(resolved), WorkspacePrefix) {
		slog.Error("Refused to tear down a path lacking the sandbox prefix.", "workspace", workspace, "resolved", resolved, "prefix", WorkspacePrefix)
		return fmt.Errorf("teardown %s: %w", workspace, ErrUnsafeTeardown)
	}
	slog.Debug("Preparing to tear down workspace.", "workspace", workspace)
	// The UNRESOLVED string, per ACT UNRESOLVED above.
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

// RejectOptionLike guards a value that becomes a positional argument in an
// assembled argv: a leading '-' would be parsed as an option, so an unsigned
// shared workflow could smuggle a flag (e.g. --upload-pack) into a
// clone/worktree invocation, and a config value could smuggle one into a
// launched harness.
//
// field is the caller's own label for the value and carries the whole error
// message — the callers span git argv (workflow, docker, branch, pr refs) and
// the clean-room reviewer's claude argv, so it names no subsystem itself.
func RejectOptionLike(field, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q must not begin with '-'", field, value)
	}
	return nil
}
