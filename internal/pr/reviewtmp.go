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

// reviewEnvScrub turns on Claude Code's subprocess environment scrub for the
// reviewer. Without it, an allowed `cat /proc/self/environ` printed the whole
// window environment into the transcript (measured on Claude Code 2.1.296,
// with ANTHROPIC_* and AWS_SECRET_ACCESS_KEY among it). With it, Claude Code
// strips credential-shaped variables (ANTHROPIC_API_KEY, AWS_SECRET_ACCESS_KEY,
// any *_TOKEN or *_SECRET it classifies) and credential-path variables
// (GH_CONFIG_DIR, XDG_CONFIG_HOME and the like) from every command's
// environment, and refuses a command naming /proc/*/environ.
//
// Two things it does not do, measured on 2.1.296: it keeps GH_TOKEN and
// GITHUB_TOKEN, so gh still authenticates from them (and a reviewer that
// finds another way to print its environment still sees them); and it forces
// the permission mode to default, overriding the plan mode forgectl passes.
// In -p mode, default still refuses every tool call the allow-list does not
// grant, since there is nobody to ask.
const reviewEnvScrub = "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1"

// A review's private temp dir and its settings dir are siblings of its
// workspace, named prefix + the workspace's own random suffix + "-" + a fresh
// random part os.MkdirTemp picks (reviewSiblingName). The workspace suffix
// lets every teardown path find them again from the breadcrumb's workspace
// field alone; the fresh part means nobody who can see the workspace's name
// in a shared temp dir can predict theirs and make it first.
//
// Teardown matches by name, so it can meet an entry someone else made under a
// matching name. It removes only a real directory the operator owns, opened
// without following a symlink (openOwnReviewSibling); anything else under a
// matching name is left alone. A symlinked workspace finds its siblings
// beside the link, where they were made: the name derives from the workspace
// path as spelled, unresolved, which is the string the dispatch made them
// from, every teardown path holds, and sandbox.Teardown removes.
//
// They sit outside the workspace on purpose. The sandbox denies writes to the
// whole workspace, and a deny there outranks any grant below it.

// reviewSettingsPrefix names the directory that holds the reviewer's settings
// file, a second sibling of the workspace. It is NOT inside the private temp
// dir: that dir is CLAUDE_CODE_TMPDIR, and Claude Code's sandbox write set is
// the claude-<uid> dir inside it today. A file whose safety rests on its
// directory not being writable belongs where no sandbox grant reaches, so a
// wider write set in a later Claude Code cannot let the reviewer replace its
// own settings.
const reviewSettingsPrefix = "forgectl-rset-"

// reviewSiblingPrefixes are the prefixes of every sibling a review makes, with
// what each holds, in the order teardown removes them.
var reviewSiblingPrefixes = []struct{ prefix, what string }{
	{reviewTmpPrefix, "private temp dir"},
	{reviewSettingsPrefix, "settings dir"},
}

// reviewSiblingName returns the directory workspace's siblings are made in and
// the name prefix they carry for prefix: prefix + the workspace's random
// suffix + "-". The trailing "-" keeps workspace 12's siblings from matching
// workspace 123's.
func reviewSiblingName(workspace, prefix string) (parent, name string, err error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", "", errors.New("the review workspace path is not absolute")
	}
	clean := filepath.Clean(workspace)
	suffix, ok := strings.CutPrefix(filepath.Base(clean), sandbox.WorkspacePrefix)
	if !ok || suffix == "" {
		return "", "", fmt.Errorf("the review workspace %q does not carry the sandbox prefix", clean)
	}
	return filepath.Dir(clean), prefix + suffix + "-", nil
}

// errForeignReviewSibling marks an entry under a review sibling's name that
// is not a directory the operator owns, and so is not one forgectl made.
var errForeignReviewSibling = errors.New("not a directory forgectl made")

// openOwnReviewSibling opens dir, which must be a real directory owned by the
// operator, without following a symlink, and checks the open reached the
// entry Lstat saw (os.SameFile). Every chmod of a sibling goes through the
// returned handle, so a symlink swapped in after the check is never followed.
func openOwnReviewSibling(dir string) (*os.File, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !ownedByOperator(info) {
		return nil, fmt.Errorf("%q: %w", dir, errForeignReviewSibling)
	}
	f, err := openDirNoFollow(dir)
	if err != nil {
		return nil, err
	}
	reached, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(info, reached) {
		_ = f.Close()
		return nil, fmt.Errorf("%q: %w", dir, errDirRootMoved)
	}
	return f, nil
}

// makeReviewSibling makes a new sibling of workspace for prefix, 0700, and
// returns it open (openOwnReviewSibling) with its path.
func makeReviewSibling(workspace, prefix string) (*os.File, string, error) {
	parent, name, err := reviewSiblingName(workspace, prefix)
	if err != nil {
		return nil, "", err
	}
	dir, err := os.MkdirTemp(parent, name+"*")
	if err != nil {
		return nil, "", err
	}
	f, err := openOwnReviewSibling(dir)
	if err != nil {
		_ = os.Remove(dir)
		return nil, "", err
	}
	// MkdirTemp's 0700 is filtered by the umask, which can only remove bits,
	// so this is a no-op unless something unusual is in play, and then it is
	// the fix.
	if err := f.Chmod(0o700); err != nil {
		_ = f.Close()
		_ = os.Remove(dir)
		return nil, "", err
	}
	return f, dir, nil
}

// removeReviewSibling removes dir, a sibling this operator made. The settings
// dir is left 0500, so it is made writable again first, through a handle
// openOwnReviewSibling vouched for. A missing dir is not an error; one that is
// not a directory the operator owns is refused with errForeignReviewSibling
// and left alone.
func removeReviewSibling(dir string) error {
	f, err := openOwnReviewSibling(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = f.Chmod(0o700)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
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

// createReviewTmpDir makes workspace's private temp directory, 0700, under a
// name nobody could have made first (reviewSiblingName), and returns it.
func createReviewTmpDir(workspace string) (string, error) {
	f, dir, err := makeReviewSibling(workspace, reviewTmpPrefix)
	if err != nil {
		return "", fmt.Errorf("make the review's private temp dir: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = removeReviewSibling(dir)
		return "", fmt.Errorf("make the review's private temp dir: %w", err)
	}
	if err := checkReviewTmpDirLength(dir); err != nil {
		slog.Warn("The review's sandboxed commands will likely fail to start.", "error", err)
	}
	return dir, nil
}

// removeReviewTmpDir removes workspace's private temp and settings dirs, every
// sibling under their names that is a directory the operator owns. None is
// not an error (a review that never launched, or one made before
// forgectl#907). An entry under a matching name that is a symlink, another
// user's, or not a directory is not forgectl's: it is left alone, with a
// warning for another user's and an error for the rest.
func removeReviewTmpDir(workspace string) error {
	var errs []error
	for _, sib := range reviewSiblingPrefixes {
		parent, name, err := reviewSiblingName(workspace, sib.prefix)
		if err != nil {
			// No prefix, no siblings: nothing of ours to remove.
			return nil
		}
		entries, err := os.ReadDir(parent)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list the review's %s: %w", sib.what, err)
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), name) {
				continue
			}
			dir := filepath.Join(parent, e.Name())
			err := removeReviewSibling(dir)
			if err == nil {
				continue
			}
			if errors.Is(err, errForeignReviewSibling) {
				if info, lerr := os.Lstat(dir); lerr == nil && !ownedByOperator(info) {
					slog.Warn("Leaving an entry under a review sibling's name that another user owns.", "path", dir)
					continue
				}
				errs = append(errs, fmt.Errorf("refusing to remove %q: it is not a directory forgectl made", dir))
				continue
			}
			errs = append(errs, fmt.Errorf("remove the review's %s: %w", sib.what, err))
		}
	}
	return errors.Join(errs...)
}

// workspaceTeardown is sandbox.Teardown, a seam so a test can stage a
// teardown that fails after the workspace is gone.
var workspaceTeardown = sandbox.Teardown

// teardownWorkspace removes a review workspace and then its private temp
// directory. The temp dir goes once the workspace teardown succeeded, which is
// also when sandbox.Teardown has vouched for the workspace path, or when the
// teardown failed but the workspace is gone anyway: nothing is left for a
// retry to find the temp dir from, so leaving it would only leak it.
func teardownWorkspace(ctx context.Context, run exec.Runner, workspace string) error {
	if err := workspaceTeardown(ctx, run, workspace); err != nil {
		if workspace != "" {
			if _, lerr := os.Lstat(workspace); errors.Is(lerr, os.ErrNotExist) {
				if rerr := removeReviewTmpDir(workspace); rerr != nil {
					slog.Warn("Failed to remove the review's private temp dir.", "workspace", workspace, "error", rerr)
				}
			}
		}
		return err
	}
	return removeReviewTmpDir(workspace)
}

// pinReviewClaudeEnv returns env with every reviewTmpEnvKeys entry pointing at
// dir and the environment scrub on (reviewEnvScrub). Any entry env carries
// for one of those variables is dropped first. env is not modified.
func pinReviewClaudeEnv(env []string, dir string) []string {
	scrubKey, _, _ := strings.Cut(reviewEnvScrub, "=")
	out := make([]string, 0, len(env)+len(reviewTmpEnvKeys)+1)
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if slices.Contains(reviewTmpEnvKeys, key) || key == scrubKey {
			continue
		}
		out = append(out, e)
	}
	for _, k := range reviewTmpEnvKeys {
		out = append(out, k+"="+dir)
	}
	return append(out, reviewEnvScrub)
}

// reviewSettingsFile is the name of the reviewer's settings document in its
// settings dir.
const reviewSettingsFile = "review-settings.json"

// writeReviewSettings writes doc as workspace's settings file and returns its
// path and its dir: a new 0700 settings dir (makeReviewSibling), the file
// 0400 in it, then the dir 0500 through the handle the dir was opened with,
// so neither the file nor the dir's entries can change without a chmod first.
// removeReviewTmpDir removes it with the temp dir.
func writeReviewSettings(workspace, doc string) (path, dir string, err error) {
	d, dir, err := makeReviewSibling(workspace, reviewSettingsPrefix)
	if err != nil {
		return "", "", fmt.Errorf("make the reviewer's settings dir: %w", err)
	}
	defer func() {
		if cerr := d.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("seal the reviewer's settings dir: %w", cerr)
		}
		if err != nil {
			_ = removeReviewSibling(dir)
		}
	}()
	path = filepath.Join(dir, reviewSettingsFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400) //nolint:gosec // G304: path is the 0700 settings dir forgectl just made, plus a constant name
	if err != nil {
		return "", "", fmt.Errorf("write the reviewer's settings file: %w", err)
	}
	if _, err := f.WriteString(doc); err != nil {
		_ = f.Close()
		return "", "", fmt.Errorf("write the reviewer's settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", "", fmt.Errorf("write the reviewer's settings file: %w", err)
	}
	if err := d.Chmod(0o500); err != nil {
		return "", "", fmt.Errorf("seal the reviewer's settings dir: %w", err)
	}
	return path, dir, nil
}

// pinEnv returns env with set applied and every unset variable removed: any
// entry env carries for a variable in either list is dropped first. A tmux
// window cannot unset a variable, so the removal itself is unsetEnvCommand's.
func pinEnv(env, set, unset []string) []string {
	drop := make(map[string]bool, len(set)+len(unset))
	for _, e := range set {
		key, _, _ := strings.Cut(e, "=")
		drop[key] = true
	}
	for _, k := range unset {
		drop[k] = true
	}
	out := make([]string, 0, len(env)+len(set))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if !drop[key] {
			out = append(out, e)
		}
	}
	return append(out, set...)
}

// envBinary is the env(1) that unsetEnvCommand runs the reviewer under. A
// seam for tests.
var envBinary = "/usr/bin/env"

// unsetEnvCommand wraps command so it runs with every variable in unset
// removed from its environment, inherited from the tmux server or not:
// `env -u NAME ... -- command`. tmux new-window can set a variable but not
// unset one, and an empty value is not the same thing to every consumer (an
// empty CLAUDE_CONFIG_DIR makes Claude Code use the working directory). env
// execs command, so the pane still runs claude itself.
func unsetEnvCommand(unset, command []string) ([]string, error) {
	if len(unset) == 0 {
		return command, nil
	}
	if _, err := os.Stat(envBinary); err != nil {
		return nil, fmt.Errorf("the review window needs %s to unset credential-store variables: %w", envBinary, err)
	}
	out := make([]string, 0, 2+2*len(unset)+len(command))
	out = append(out, envBinary)
	for _, k := range unset {
		out = append(out, "-u", k)
	}
	out = append(out, "--")
	return append(out, command...), nil
}
