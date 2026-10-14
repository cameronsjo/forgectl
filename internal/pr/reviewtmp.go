package pr

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// reviewTmpPrefix starts the name of the private temp directory each Claude
// review window gets (forgectl#907): reviewTmpPrefix, the workspace's own
// random suffix and "-" (reviewSiblingName), then reviewTmpRandLen random
// characters. It is short on purpose (see claudeTmpDirMax) and deliberately
// not sandbox.WorkspacePrefix, so nothing that recognizes a workspace by its
// prefix mistakes this for one.
const reviewTmpPrefix = "fr"

// reviewTmpRandLen is the length of the random tail of a private temp dir's
// name.
const reviewTmpRandLen = 5

// claudeTmpDirMax is the longest per-uid temp dir, CLAUDE_CODE_TMPDIR plus
// "/claude-<uid>", that Claude Code gives the sandboxed Bash as its TMPDIR;
// a longer one is too long for its AF_UNIX sockets. What it falls back to
// differs by version, read from the bundles:
//
//   - 2.1.289 (darwin-arm64): `if(Buffer.byteLength(t)<=elr)return t;let
//     r="/tmp"` with elr=44, then claude-<uid> under that literal /tmp. The
//     fallback is the shared /tmp/claude-<uid> whatever TMPDIR says.
//   - 2.1.299: the same test with gMr=44, but the fallback is claude-<uid>
//     under os.tmpdir(), which is TMPDIR. forgectl pins TMPDIR to the private
//     dir as well, so under its pins that fallback stays inside the private
//     dir: a live probe with a 54-byte private dir and both variables pinned
//     gave the reviewer <private dir>/claude-0. (A 54-byte dir with TMPDIR
//     left at /tmp gave /tmp/claude-0; a 35-byte one gave the private dir.)
//
// So keeping the per-uid dir within 44 bytes is what keeps 2.1.289 off the
// shared dir, and it is harmless on 2.1.299. That this is why the macOS
// reviewer in forgectl#907 (2.1.289, a private dir under /var/folders) saw
// TMPDIR=/tmp/claude-501 fits the bundle, but is unconfirmed until a live
// macOS re-check: TMPDIR not reaching the shell there would look the same.
const claudeTmpDirMax = 44

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

// A review's settings dir is a sibling of its workspace, named prefix + the
// workspace's own random suffix + "-" + a fresh random part os.MkdirTemp
// picks (reviewSiblingName). Its private temp dir is made in the first of
// the workspace root and reviewShortTmpBase whose path keeps Claude Code's
// per-uid dir within claudeTmpDirMax, named from the workspace's own random
// suffix (reviewTmpName). Both derive from the workspace path alone, so every
// teardown path finds them again from the breadcrumb's workspace field; the
// fresh random part means nobody who can see the workspace's name in a shared
// temp dir can predict theirs and make it first, and a name someone made
// first is never reused (it fails EEXIST).
//
// Teardown matches by name, so it can meet an entry someone else made under a
// matching name. It removes only a real directory the operator owns, opened
// without following a symlink (openOwnReviewSibling); anything else under a
// matching name is left alone. The names derive from the workspace path as
// spelled, unresolved, which is the string the dispatch made them from,
// every teardown path holds, and sandbox.Teardown removes.
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

// reviewTmpName returns the name prefix of workspace's private temp dir:
// reviewTmpPrefix, the workspace's own random suffix, and "-", as
// reviewSiblingName gives every sibling. It names exactly one workspace in a
// root (no hash to collide, so teardown cannot match another live review's
// dir), and it is the same for every spelling of the workspace path (/var or
// /private/var). The suffix os.MkdirTemp picks is at most 10 digits. A
// workspace without the sandbox prefix has none.
//
// Teardown finds the dir by this name rather than from a path recorded in
// the breadcrumb: the breadcrumb is a file a hostile process might rewrite,
// and a removal path read from it would let that process pick what goes.
func reviewTmpName(workspace string) (string, error) {
	_, name, err := reviewSiblingName(workspace, reviewTmpPrefix)
	return name, err
}

// isReviewTmpName reports whether name is a private temp dir name for name
// prefix.
func isReviewTmpName(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && len(name) == len(prefix)+reviewTmpRandLen
}

// reviewTmpBases are the directories workspace's private temp dir may be in,
// for teardown to look through: the workspace's own parent (the workspace
// root it was made in, whatever TMPDIR is now), the workspace root by
// today's TMPDIR, and reviewShortTmpBase.
func reviewTmpBases(workspace string) []string {
	bases := []string{filepath.Dir(filepath.Clean(workspace))}
	if root, err := reviewWorkspaceRoot(); err == nil {
		bases = append(bases, root)
	}
	if reviewShortTmpBase != "" {
		bases = append(bases, reviewShortTmpBase)
	}
	var out []string
	for _, b := range bases {
		if b = filepath.Clean(b); !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// claudeTmpDirFits reports whether a private temp dir named name plus the
// random tail, made in base, keeps Claude Code's per-uid dir inside it within
// claudeTmpDirMax.
func claudeTmpDirFits(base, name string) bool {
	uid := reviewUID()
	if uid < 0 {
		return true
	}
	perUID := len(filepath.Clean(base)) + 1 + len(name) + reviewTmpRandLen + len("/claude-") + len(strconv.Itoa(uid))
	return perUID <= claudeTmpDirMax
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
	return openMadeReviewDir(dir)
}

// openMadeReviewDir opens dir, which this call just made, and makes it 0700
// through the handle. The made dir's mode is filtered by the umask, which can
// only remove bits, so the chmod is a no-op unless something unusual is in
// play, and then it is the fix.
func openMadeReviewDir(dir string) (*os.File, string, error) {
	f, err := openOwnReviewSibling(dir)
	if err != nil {
		_ = os.Remove(dir)
		return nil, "", err
	}
	if err := f.Chmod(0o700); err != nil {
		_ = f.Close()
		_ = os.Remove(dir)
		return nil, "", err
	}
	return f, dir, nil
}

// reviewTmpAlphabet is what the random tail of a private temp dir's name is
// drawn from.
const reviewTmpAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// makeReviewTmpDir makes a new dir named prefix plus a random tail in base,
// 0700, never reusing one that exists.
func makeReviewTmpDir(base, prefix string) (string, error) {
	for range 64 {
		var b [reviewTmpRandLen]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = reviewTmpAlphabet[int(b[i])%len(reviewTmpAlphabet)]
		}
		dir := filepath.Join(base, prefix+string(b[:]))
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		f, dir, err := openMadeReviewDir(dir)
		if err != nil {
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = removeReviewSibling(dir)
			return "", err
		}
		return dir, nil
	}
	return "", fmt.Errorf("no free private temp dir name in %q", base)
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

// createReviewTmpDir makes workspace's private temp directory, 0700, under a
// name nobody could have made first, and returns it. It goes in the
// workspace root when Claude Code's per-uid dir inside it fits
// claudeTmpDirMax, else in reviewShortTmpBase. When neither fits, the review
// is refused: Claude Code would hand the reviewer's Bash a temp dir outside
// the private one, which the sandbox denies (sharedClaudeTmpDirs), and every
// command would fail.
func createReviewTmpDir(workspace string) (string, error) {
	name, err := reviewTmpName(workspace)
	if err != nil {
		return "", fmt.Errorf("make the review's private temp dir: %w", err)
	}
	base, err := reviewTmpHome(name)
	if err != nil {
		return "", err
	}
	dir, err := makeReviewTmpDir(base, name)
	if err != nil {
		return "", fmt.Errorf("make the review's private temp dir: %w", err)
	}
	return dir, nil
}

// reviewTmpHome is the directory a new private temp dir named name goes in:
// the workspace root when it fits claudeTmpDirMax, else reviewShortTmpBase.
func reviewTmpHome(name string) (string, error) {
	var bases []string
	if root, err := reviewWorkspaceRoot(); err == nil {
		bases = append(bases, root)
	}
	if reviewShortTmpBase != "" {
		bases = append(bases, reviewShortTmpBase)
	}
	for _, base := range bases {
		if claudeTmpDirFits(base, name) {
			return base, nil
		}
	}
	return "", fmt.Errorf("refusing to make the review's private temp dir: none of %q is short enough "+
		"for Claude Code to use it (its per-user temp dir must fit %d bytes)", bases, claudeTmpDirMax)
}

// removeReviewTmpDir removes workspace's private temp and settings dirs, every
// sibling under their names that is a directory the operator owns. None is
// not an error (a review that never launched, or one made before
// forgectl#907). An entry under a matching name that is a symlink, another
// user's, or not a directory is not forgectl's: it is left alone, with a
// warning for another user's and an error for the rest.
func removeReviewTmpDir(workspace string) error {
	tmpName, err := reviewTmpName(workspace)
	if err != nil {
		// No prefix, no siblings: nothing of ours to remove.
		return nil
	}
	settingsParent, settingsName, err := reviewSiblingName(workspace, reviewSettingsPrefix)
	if err != nil {
		return nil
	}
	type place struct {
		parent, what string
		match        func(string) bool
	}
	var places []place
	for _, base := range reviewTmpBases(workspace) {
		places = append(places, place{base, "private temp dir", func(n string) bool { return isReviewTmpName(n, tmpName) }})
	}
	places = append(places, place{settingsParent, "settings dir", func(n string) bool { return strings.HasPrefix(n, settingsName) }})

	var errs []error
	for _, p := range places {
		entries, err := os.ReadDir(p.parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("list the review's %s: %w", p.what, err))
			continue
		}
		for _, e := range entries {
			if !p.match(e.Name()) {
				continue
			}
			dir := filepath.Join(p.parent, e.Name())
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
			errs = append(errs, fmt.Errorf("remove the review's %s: %w", p.what, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveReviewDirs removes the private temp dir and settings dir a Claude
// dispatch made for workspace, as teardown does, without touching the
// workspace itself. It is for a caller left holding a workspace path whose
// session was never torn down, such as a test harness.
func RemoveReviewDirs(workspace string) error {
	return removeReviewTmpDir(workspace)
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
