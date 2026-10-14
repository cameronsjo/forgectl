package pr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// reviewTmpPrefix names the private temp directory each Claude review window
// gets (forgectl#907). It is deliberately not sandbox.WorkspacePrefix, so
// nothing that recognizes a workspace by its prefix mistakes this for one.
const reviewTmpPrefix = "forgectl-rtmp-"

// reviewTmpEnvKeys are the variables a review window's temp directory is
// pinned through. Claude Code keeps its per-user temp dir (claude-<uid>), its
// sandbox's sockets, and the copy of an inline --settings document under
// CLAUDE_CODE_TMPDIR, and its sandbox lets Bash write only to the claude-<uid>
// dir inside it. TMPDIR is what every other process in the window reads.
var reviewTmpEnvKeys = []string{"TMPDIR", "CLAUDE_CODE_TMPDIR"}

// reviewTmpDirFor is the private temp directory of the review of workspace:
// a sibling of the workspace, named from the workspace's own random suffix.
// It is derived, not recorded, so every path that removes a workspace can find
// it again from the breadcrumb's workspace field alone. A workspace whose name
// lacks the sandbox prefix has none.
//
// The sibling sits outside the workspace on purpose. The sandbox denies
// writes to the whole workspace, and a deny there outranks any grant below it.
func reviewTmpDirFor(workspace string) (string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", errors.New("the review workspace path is not absolute")
	}
	clean := filepath.Clean(workspace)
	suffix, ok := strings.CutPrefix(filepath.Base(clean), sandbox.WorkspacePrefix)
	if !ok || suffix == "" {
		return "", fmt.Errorf("the review workspace %q does not carry the sandbox prefix", clean)
	}
	return filepath.Join(filepath.Dir(clean), reviewTmpPrefix+suffix), nil
}

// reviewTmpGOOS is the platform checkReviewTmpDirLength judges, a seam for
// its tests. It is not sandboxGOOS, which TestMain pins to linux for every
// test: on a macOS host the long system temp dir would then fail a check
// meant only for Linux.
var reviewTmpGOOS = runtime.GOOS

// unixSocketPathMax is the longest Unix socket path Linux accepts (sun_path
// is 108 bytes with its terminating NUL).
const unixSocketPathMax = 107

// claudeSocksSocketName is the longest socket name Claude Code's Linux sandbox
// creates directly in its temp directory: the socat bridge
// claude-socks-<16 hex>.sock.
const claudeSocksSocketName = "claude-socks-0123456789abcdef.sock"

// checkReviewTmpDirLength reports a private temp dir too long to hold the
// Linux sandbox's bridge sockets. Measured on Claude Code 2.1.296: with an
// 80-byte CLAUDE_CODE_TMPDIR, every sandboxed command failed with "Failed to
// create bridge sockets". failIfUnavailable makes that fail closed (the
// reviewer's Bash refuses to run, nothing runs unsandboxed), but inside a
// detached window nobody reads, so createReviewTmpDir logs this instead. It
// does not refuse: the dir sits beside the workspace, which already lives in
// the operator's TMPDIR, and a long TMPDIR was close to the limit before the
// private dir existed. The macOS sandbox has no socat bridge, and its socket
// names there are unverified, so it is not checked.
func checkReviewTmpDirLength(dir string) error {
	if reviewTmpGOOS != "linux" {
		return nil
	}
	if len(dir)+1+len(claudeSocksSocketName) > unixSocketPathMax {
		return fmt.Errorf("the review's private temp dir %q is too long for the sandbox's socket paths; "+
			"set TMPDIR to a shorter directory", dir)
	}
	return nil
}

// createReviewTmpDir makes workspace's private temp directory, 0700, and
// returns it. It refuses one that already exists: a review is dispatched once
// per prepared workspace, so an existing directory is something this review
// did not make, and reusing it would hand the reviewer whatever is in it.
func createReviewTmpDir(workspace string) (string, error) {
	dir, err := reviewTmpDirFor(workspace)
	if err != nil {
		return "", fmt.Errorf("refusing to make the review's private temp dir: %w", err)
	}
	if err := checkReviewTmpDirLength(dir); err != nil {
		slog.Warn("The review's sandboxed commands will likely fail to start.", "error", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("make the review's private temp dir: %w", err)
	}
	// Mkdir's mode is filtered by the umask; a umask can only remove bits, so
	// this is a no-op unless something unusual is in play, and then it is the
	// fix.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; 0700 is owner-only
		_ = os.Remove(dir)
		return "", fmt.Errorf("restrict the review's private temp dir: %w", err)
	}
	return dir, nil
}

// removeReviewTmpDir removes workspace's private temp directory, if there is
// one. A missing directory is not an error (a review that never launched, or
// one made before forgectl#907). It refuses anything at that path that is not
// a real directory, rather than following a symlink out of it.
func removeReviewTmpDir(workspace string) error {
	dir, err := reviewTmpDirFor(workspace)
	if err != nil {
		// No prefix, no private temp dir: nothing of ours to remove.
		return nil
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect the review's private temp dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to remove %q: it is not a directory", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove the review's private temp dir: %w", err)
	}
	return nil
}

// teardownWorkspace removes a review workspace and then its private temp
// directory. The temp dir goes only once the workspace teardown succeeded,
// which is also when sandbox.Teardown has vouched for the workspace path the
// sibling's name is derived from.
func teardownWorkspace(ctx context.Context, run exec.Runner, workspace string) error {
	if err := sandbox.Teardown(ctx, run, workspace); err != nil {
		return err
	}
	return removeReviewTmpDir(workspace)
}

// pinReviewTmpEnv returns env with every reviewTmpEnvKeys entry pointing at
// dir. Any entry env carries for one of them is dropped first. env is not
// modified.
func pinReviewTmpEnv(env []string, dir string) []string {
	out := make([]string, 0, len(env)+len(reviewTmpEnvKeys))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if slices.Contains(reviewTmpEnvKeys, key) {
			continue
		}
		out = append(out, e)
	}
	for _, k := range reviewTmpEnvKeys {
		out = append(out, k+"="+dir)
	}
	return out
}
