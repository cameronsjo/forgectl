package pr

// Test plan for reviewtmp.go (forgectl#907)
//
// reviewSiblingName (Classification: path derivation)
//   [x] A sibling name from the workspace's random suffix plus "-", never
//       the workspace prefix
//   [x] Refuses a relative workspace and one without the sandbox prefix
// createReviewTmpDir (Classification: security control, filesystem)
//   [x] Makes the dir 0700 under a fresh name each time
//   [x] Never reuses or touches an entry made first under a matching name
//   [x] Reports, on Linux, a path too long for the sandbox's bridge sockets
// removeReviewTmpDir / teardownWorkspace (Classification: cleanup)
//   [x] A missing dir is not an error
//   [x] A symlink under a matching name is refused and its target left alone
//   [x] Another user's entry under a matching name is left alone (unix test)
//   [x] teardownWorkspace removes the workspace and then the private dir
// writeReviewSettings (Classification: security control, filesystem)
//   [x] A 0400 file in a 0500 sibling dir outside the private temp dir
//   [x] Refuses a second write; teardown removes the sealed dir
// pinReviewClaudeEnv
//   [x] Replaces ambient TMPDIR / CLAUDE_CODE_TMPDIR / scrub entries
// teardownWorkspace after a failure
//   [x] A failed teardown of a gone workspace still removes the private dir
//   [x] A failed teardown of a standing workspace keeps it
// launchInline after a failed new-window
//   [x] The dir stays for teardown, and teardown removes it
// launchInline wiring
//   [x] The window env points both variables at a fresh 0700 dir
//   [x] A dispatch refused before the window opens removes the dir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

func TestReviewSiblingName_DerivesFromTheWorkspaceSuffix(t *testing.T) {
	parent, name, err := reviewSiblingName("/tmp/"+sandbox.WorkspacePrefix+"123", reviewSettingsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if parent != "/tmp" || name != reviewSettingsPrefix+"123-" {
		t.Errorf("reviewSiblingName = %q, %q, want /tmp, %s123-", parent, name, reviewSettingsPrefix)
	}
	if strings.HasPrefix(name, sandbox.WorkspacePrefix) {
		t.Errorf("%q carries the workspace prefix, so it would read as a workspace", name)
	}
	for _, ws := range []string{"", "relative/" + sandbox.WorkspacePrefix + "1", "/tmp/not-a-workspace", "/tmp/" + sandbox.WorkspacePrefix} {
		if _, _, err := reviewSiblingName(ws, reviewTmpPrefix); err == nil {
			t.Errorf("reviewSiblingName(%q) derived a name, want a refusal", ws)
		}
	}
}

// reviewSiblings lists ws's siblings for prefix: every entry under the name
// reviewSiblingName (or, for the private temp dir, reviewTmpName) gives,
// whoever made it.
func reviewSiblings(t *testing.T, ws, prefix string) []string {
	t.Helper()
	if prefix == reviewTmpPrefix {
		name, err := reviewTmpName(ws)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, base := range reviewTmpBases(ws) {
			entries, err := os.ReadDir(base)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if isReviewTmpName(e.Name(), name) {
					out = append(out, filepath.Join(base, e.Name()))
				}
			}
		}
		return out
	}
	parent, name, err := reviewSiblingName(ws, prefix)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), name) {
			out = append(out, filepath.Join(parent, e.Name()))
		}
	}
	return out
}

// onlyReviewSibling is ws's one sibling for prefix.
func onlyReviewSibling(t *testing.T, ws, prefix string) string {
	t.Helper()
	got := reviewSiblings(t, ws, prefix)
	if len(got) != 1 {
		t.Fatalf("%s siblings of %s = %v, want exactly one", prefix, ws, got)
	}
	return got[0]
}

// plantedSibling returns a path under ws's sibling name for prefix, where a
// stranger could make an entry before or during a review. A private temp
// dir's tail must be reviewTmpRandLen long to match.
func plantedSibling(t *testing.T, ws, prefix, tail string) string {
	t.Helper()
	if prefix == reviewTmpPrefix {
		if len(tail) != reviewTmpRandLen {
			t.Fatalf("a private temp dir tail is %d characters, not %q", reviewTmpRandLen, tail)
		}
		name, err := reviewTmpName(ws)
		if err != nil {
			t.Fatal(err)
		}
		base, err := reviewTmpHome(name)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(base, name+tail)
	}
	parent, name, err := reviewSiblingName(ws, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, name+tail)
}

// fakeWorkspaceIn makes a workspace-shaped dir under parent. Its private
// temp dir goes in reviewShortTmpBase, which TestMain points at a short dir
// of its own, because a t.TempDir() parent is too long for claudeTmpDirMax.
func fakeWorkspaceIn(t *testing.T, parent string) string {
	t.Helper()
	ws, err := os.MkdirTemp(parent, sandbox.WorkspacePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	// The settings dir is left 0500, which t.TempDir's own cleanup cannot
	// empty without root; this cleanup runs before it.
	t.Cleanup(func() { _ = removeReviewTmpDir(ws) })
	return ws
}

// TestWriteReviewSettings_SealsADirOfItsOwn: the settings file lands 0400 in
// a 0500 sibling of the workspace that is not inside the private temp dir,
// a second write gets a dir of its own, and teardown removes the sealed dirs.
func TestWriteReviewSettings_SealsADirOfItsOwn(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	tmp, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	path, dir, err := writeReviewSettings(ws, `{"a":1}`)
	if err != nil {
		t.Fatalf("writeReviewSettings: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("settings file %q is not in the returned dir %q", path, dir)
	}
	if filepath.Dir(dir) != filepath.Dir(ws) || !strings.HasPrefix(filepath.Base(dir), reviewSettingsPrefix) {
		t.Errorf("settings dir %q is not a %s sibling of %q", dir, reviewSettingsPrefix, ws)
	}
	if rel, err := filepath.Rel(tmp, path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("settings file %q sits inside the private temp dir %q", path, tmp)
	}
	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0o500 {
		t.Errorf("settings dir mode = %v (%v), want 0500", info, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("settings file mode = %v (%v), want 0400", info, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != `{"a":1}` { //nolint:gosec // G304: the path the test just wrote
		t.Errorf("settings file = %q (%v)", data, err)
	}
	again, againDir, err := writeReviewSettings(ws, `{}`)
	if err != nil {
		t.Fatalf("a second writeReviewSettings: %v", err)
	}
	if againDir == dir || again == path {
		t.Errorf("a second writeReviewSettings reused %q", dir)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != `{"a":1}` { //nolint:gosec // G304: the path the test just wrote
		t.Errorf("the first settings file changed: %q (%v)", data, err)
	}
	if err := teardownWorkspace(context.Background(), &exec.FakeRunner{}, ws); err != nil {
		t.Fatalf("teardownWorkspace: %v", err)
	}
	for _, gone := range []string{ws, tmp, dir, againDir} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q survived teardown: %v", gone, err)
		}
	}
}

func TestCreateReviewTmpDir_MakesAPrivateDir(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatalf("createReviewTmpDir: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("private temp dir mode = %o, want 0700", perm)
	}
	if got := onlyReviewSibling(t, ws, reviewTmpPrefix); got != dir {
		t.Errorf("the dir teardown finds = %q, want %q", got, dir)
	}
	other, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if other == dir {
		t.Errorf("two private temp dirs share the name %q, so it is predictable", dir)
	}
}

// TestCreateReviewTmpDir_IgnoresEntriesUnderAMatchingName: a stranger who
// saw the workspace's name and made a dir or a symlink under a sibling name
// first neither blocks the review nor gets used or touched by it.
func TestCreateReviewTmpDir_IgnoresEntriesUnderAMatchingName(t *testing.T) {
	parent := t.TempDir()
	ws := fakeWorkspaceIn(t, parent)
	planted := plantedSibling(t, ws, reviewTmpPrefix, "plant")
	if err := os.Mkdir(planted, 0o700); err != nil {
		t.Fatal(err)
	}
	plantedFile := filepath.Join(planted, "planted")
	if err := os.WriteFile(plantedFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := plantedSibling(t, ws, reviewTmpPrefix, "link0")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatalf("createReviewTmpDir: %v", err)
	}
	if dir == planted || dir == link {
		t.Fatalf("createReviewTmpDir used %q, an entry it did not make", dir)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the new dir is not empty: %v (%v)", entries, err)
	}
	if _, err := os.Stat(plantedFile); err != nil {
		t.Errorf("making the dir touched the planted one: %v", err)
	}
}

// TestClaudeTmpDirFits: Claude Code's per-uid dir inside a private temp dir
// must fit claudeTmpDirMax, or Claude Code hands the reviewer's Bash a temp
// dir outside it.
func TestClaudeTmpDirFits(t *testing.T) {
	stubReviewUID(t, 501)
	// The longest name, for a 10-digit workspace suffix, is 13 bytes, the tail
	// 5, and "/claude-501" 11, so a base of up to 14 bytes fits.
	long := reviewTmpPrefix + "1234567890-"
	if !claudeTmpDirFits("/tmp", long) {
		t.Error("/tmp does not fit")
	}
	if !claudeTmpDirFits("/"+strings.Repeat("d", 13), long) {
		t.Error("a 14-byte base does not fit")
	}
	if claudeTmpDirFits("/"+strings.Repeat("d", 14), long) {
		t.Error("a 15-byte base fits")
	}
	if !claudeTmpDirFits("/"+strings.Repeat("d", 22), reviewTmpPrefix+"1-") {
		t.Error("a short suffix does not get the room it leaves")
	}
	if claudeTmpDirFits("/var/folders/36/tjdph2t965j8snz9_vkdnw0r0000gn/T", reviewTmpPrefix+"1-") {
		t.Error("macOS's per-user temp dir fits")
	}
	// The worst case /tmp must hold: a 10-digit suffix and a 10-digit uid.
	stubReviewUID(t, 4294967294)
	if !claudeTmpDirFits("/tmp", long) {
		t.Error("/tmp does not fit the longest name and uid")
	}
	stubReviewUID(t, -1)
	if !claudeTmpDirFits("/"+strings.Repeat("d", 200), long) {
		t.Error("without a uid nothing can be measured, so everything fits")
	}
}

// TestReviewTmpName_ExactAndSpellingFree: the private temp dir's name is the
// workspace's own suffix, so one workspace's teardown never matches another
// live review's dir (12 against 123), and the /var and /private/var
// spellings of one workspace name the same dir.
func TestReviewTmpName_ExactAndSpellingFree(t *testing.T) {
	a, err := reviewTmpName("/var/folders/x/T/" + sandbox.WorkspacePrefix + "12")
	if err != nil {
		t.Fatal(err)
	}
	b, err := reviewTmpName("/private/var/folders/x/T/" + sandbox.WorkspacePrefix + "12")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("two spellings of one workspace name %q and %q", a, b)
	}
	other, err := reviewTmpName("/var/folders/x/T/" + sandbox.WorkspacePrefix + "123")
	if err != nil {
		t.Fatal(err)
	}
	if isReviewTmpName(other+"abcde", a) || isReviewTmpName(a+"abcde", other) {
		t.Errorf("%q and %q match each other's dirs", a, other)
	}

	parent := t.TempDir()
	ws12 := filepath.Join(parent, sandbox.WorkspacePrefix+"12")
	ws123 := filepath.Join(parent, sandbox.WorkspacePrefix+"123")
	for _, ws := range []string{ws12, ws123} {
		if err := os.Mkdir(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = removeReviewTmpDir(ws) })
	}
	if _, err := createReviewTmpDir(ws12); err != nil {
		t.Fatal(err)
	}
	keep, err := createReviewTmpDir(ws123)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeReviewTmpDir(ws12); err != nil {
		t.Fatal(err)
	}
	if len(reviewSiblings(t, ws12, reviewTmpPrefix)) != 0 {
		t.Error("workspace 12's private dir survived its teardown")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("workspace 12's teardown removed workspace 123's private dir: %v", err)
	}
}

// TestCreateReviewTmpDir_GoesWhereClaudeCodeWillUseIt: a workspace root too
// long for claudeTmpDirMax sends the dir to reviewShortTmpBase, and with no
// base short enough the review is refused rather than handed a temp dir the
// reviewer's Bash would not get.
func TestCreateReviewTmpDir_GoesWhereClaudeCodeWillUseIt(t *testing.T) {
	stubReviewUID(t, 501)
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != reviewShortTmpBase {
		t.Errorf("private dir %s is not in the short base %s", dir, reviewShortTmpBase)
	}
	if per := len(filepath.Join(dir, "claude-501")); per > claudeTmpDirMax {
		t.Errorf("Claude Code's per-uid dir %d bytes > %d", per, claudeTmpDirMax)
	}

	orig := reviewShortTmpBase
	reviewShortTmpBase = "/" + strings.Repeat("d", 40)
	t.Cleanup(func() { reviewShortTmpBase = orig })
	if got, err := createReviewTmpDir(ws); err == nil {
		t.Errorf("createReviewTmpDir = %s with no short base, want a refusal", got)
	}
}

func TestRemoveReviewTmpDir(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		ws := fakeWorkspaceIn(t, t.TempDir())
		if err := removeReviewTmpDir(ws); err != nil {
			t.Errorf("removing a missing private dir = %v, want nil", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		parent := t.TempDir()
		ws := fakeWorkspaceIn(t, parent)
		dir := plantedSibling(t, ws, reviewSettingsPrefix, "link")
		target := filepath.Join(parent, "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(target, "keep")
		if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Sealed like a settings dir: a chmod that followed the link would
		// open it up.
		if err := os.Chmod(target, 0o500); err != nil { //nolint:gosec // G302: sealed like a settings dir, owner read and search only
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o700) }) //nolint:gosec // G302: test cleanup of its own dir
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		if err := removeReviewTmpDir(ws); err == nil {
			t.Error("removeReviewTmpDir accepted a symlink under a sibling name")
		}
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("the symlink's target lost a file: %v", err)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o500 {
			t.Errorf("the symlink's target mode = %v (%v), want it left 0500", info, err)
		}
		if _, err := os.Lstat(dir); err != nil {
			t.Errorf("the symlink was removed: %v", err)
		}
	})
}

func TestTeardownWorkspace_RemovesThePrivateTmpDir(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-settings-x.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := teardownWorkspace(context.Background(), &exec.FakeRunner{}, ws); err != nil {
		t.Fatalf("teardownWorkspace: %v", err)
	}
	for _, p := range []string{ws, dir} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived teardown (%v)", p, err)
		}
	}
}

func TestPinReviewClaudeEnv_ReplacesAmbientEntries(t *testing.T) {
	env := []string{"TMPDIR=/shared", "PATH=/bin", "CLAUDE_CODE_TMPDIR=/shared2", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0"}
	got := pinReviewClaudeEnv(env, "/private")
	want := []string{"PATH=/bin", "TMPDIR=/private", "CLAUDE_CODE_TMPDIR=/private", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1"}
	if !slices.Equal(got, want) {
		t.Errorf("pinReviewClaudeEnv = %v, want %v", got, want)
	}
	if env[0] != "TMPDIR=/shared" {
		t.Error("pinReviewClaudeEnv modified its input")
	}
}

// TestTeardownWorkspace_FailedTeardownOfAGoneWorkspaceStillRemovesTheTmpDir:
// once the workspace is gone, nothing can find the temp dir again, so a
// failed teardown must not leak it.
func TestTeardownWorkspace_FailedTeardownOfAGoneWorkspaceStillRemovesTheTmpDir(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("teardown hit an error after removing the workspace")
	orig := workspaceTeardown
	workspaceTeardown = func(_ context.Context, _ exec.Runner, w string) error {
		_ = os.RemoveAll(w)
		return failure
	}
	t.Cleanup(func() { workspaceTeardown = orig })

	if err := teardownWorkspace(context.Background(), &exec.FakeRunner{}, ws); !errors.Is(err, failure) {
		t.Fatalf("teardownWorkspace = %v, want the teardown's error", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the private dir survived a failed teardown of a gone workspace (%v)", err)
	}
}

// TestTeardownWorkspace_FailedTeardownKeepsTheTmpDirWhileTheWorkspaceStands:
// a workspace still on disk can be torn down again, and the dir goes then.
func TestTeardownWorkspace_FailedTeardownKeepsTheTmpDirWhileTheWorkspaceStands(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	orig := workspaceTeardown
	workspaceTeardown = func(context.Context, exec.Runner, string) error { return errors.New("disk said no") }
	t.Cleanup(func() { workspaceTeardown = orig })

	if err := teardownWorkspace(context.Background(), &exec.FakeRunner{}, ws); err == nil {
		t.Fatal("teardownWorkspace swallowed the teardown's error")
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("the private dir went while its workspace still stands: %v", err)
	}
}

func TestLaunchInline_GivesTheWindowAPrivateTmpDir(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"),
		WithWindowEnv(func() ([]string, error) { return []string{"TMPDIR=/tmp"}, nil }))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	dir := onlyReviewSibling(t, sess.Workspace, reviewTmpPrefix)
	args := fake.Last().Args
	for _, key := range reviewTmpEnvKeys {
		if !argPair(args, "-e", key+"="+dir) {
			t.Errorf("tmux argv does not pin %s to the private dir %s: %v", key, dir, args)
		}
	}
	if !argPair(args, "-e", reviewEnvScrub) {
		t.Errorf("tmux argv does not turn on the subprocess environment scrub: %v", args)
	}
	if argPair(args, "-e", "TMPDIR=/tmp") {
		t.Errorf("the ambient TMPDIR survived beside the pin: %v", args)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("the private dir does not outlive the dispatch: %v", err)
	}
	if perm := info.Mode().Perm(); !info.IsDir() || perm != 0o700 {
		t.Errorf("private dir mode = %v, want a 0700 directory", info.Mode())
	}
}

func TestLaunchInline_RefusedDispatchRemovesThePrivateTmpDir(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	// tmux -V fails, so CheckDispatchCapability refuses after the private
	// dir was made and before any window opens.
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" {
			return "", errors.New("no tmux here")
		}
		return "", nil
	}}
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	// An entry a stranger made under a sibling name is not the dispatch's to
	// remove when it backs out.
	planted := plantedSibling(t, sess.Workspace, reviewTmpPrefix, "plant")
	if err := os.Mkdir(planted, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("Launch succeeded without tmux")
	}
	if got := reviewSiblings(t, sess.Workspace, reviewTmpPrefix); !slices.Equal(got, []string{planted}) {
		t.Errorf("after a refused dispatch the private dirs are %v, want only the planted %s", got, planted)
	}
}

// TestLaunchInline_FailedNewWindowLeavesTheTmpDirForTeardown: once tmux was
// asked to open the window, a claude may be running in it, so a failed
// new-window keeps the private dir; the session is parked, and the teardown
// that settles it (pr repair --rollback, pr teardown) removes it.
func TestLaunchInline_FailedNewWindowLeavesTheTmpDirForTeardown(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	base := successfulLaunchRunner()
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "new-window" {
			return "", errors.New("tmux: create window failed")
		}
		return base.RunFunc(name, args)
	}}
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("Launch succeeded although new-window failed")
	}
	dir := onlyReviewSibling(t, sess.Workspace, reviewTmpPrefix)
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("a failed new-window removed the private dir a running claude may use: %v", err)
	}
	if err := sandboxTeardown(context.Background(), fake, sess.Workspace); err != nil {
		t.Fatalf("sandboxTeardown: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("teardown left the private dir behind (%v)", err)
	}
}

// stubCredentialEnv makes credentialEnv answer from env alone.
func stubCredentialEnv(t *testing.T, env map[string]string) {
	t.Helper()
	orig := credentialEnv
	credentialEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	t.Cleanup(func() { credentialEnv = orig })
}

func TestReviewRelocationEnv_SetsUsableValuesAndUnsetsTheRest(t *testing.T) {
	stubCredentialEnv(t, map[string]string{
		"CODEX_HOME":        "/data/codex",
		"KUBECONFIG":        "/k/a" + string(os.PathListSeparator) + "/k/b",
		"CLAUDE_CONFIG_DIR": "relative",
		"GNUPGHOME":         "",
		"DOCKER_CONFIG":     "/home/o/link/../docker",
	})
	set, unset := reviewRelocationEnv()
	for _, want := range []string{"CODEX_HOME=/data/codex", "KUBECONFIG=/k/a" + string(os.PathListSeparator) + "/k/b"} {
		if !slices.Contains(set, want) {
			t.Errorf("set %v missing %q", set, want)
		}
	}
	for _, want := range []string{"CLAUDE_CONFIG_DIR", "GNUPGHOME", "DOCKER_CONFIG", "XDG_CONFIG_HOME", "GH_CONFIG_DIR", "SOPS_AGE_KEY_FILE"} {
		if !slices.Contains(unset, want) {
			t.Errorf("unset %v missing %q", unset, want)
		}
	}
	seen := map[string]bool{}
	for _, e := range set {
		k, _, _ := strings.Cut(e, "=")
		seen[k] = true
	}
	for _, k := range unset {
		if seen[k] {
			t.Errorf("%s is both set and unset", k)
		}
		seen[k] = true
	}
	for _, r := range relocations {
		if !seen[r.variable] {
			t.Errorf("%s is followed by the denies but neither set nor unset in the window", r.variable)
		}
	}
}

func TestUnsetEnvCommand(t *testing.T) {
	got, err := unsetEnvCommand([]string{"A", "B"}, []string{"/bin/claude", "-p", "x"})
	if err != nil {
		t.Skipf("no %s here: %v", envBinary, err)
	}
	want := []string{envBinary, "-u", "A", "-u", "B", "--", "/bin/claude", "-p", "x"}
	if !slices.Equal(got, want) {
		t.Errorf("unsetEnvCommand = %v, want %v", got, want)
	}
	if got, _ := unsetEnvCommand(nil, []string{"/bin/claude"}); !slices.Equal(got, []string{"/bin/claude"}) {
		t.Errorf("with nothing to unset = %v, want the command alone", got)
	}
}

// TestLaunchInline_PinsTheRelocationsTheDeniesFollowed is forgectl#715's
// window/deny agreement: a tmux server holding CODEX_HOME=/data/codex must not
// reach a reviewer whose denies were built with CODEX_HOME unset, and a value
// forgectl does hold reaches both.
func TestLaunchInline_PinsTheRelocationsTheDeniesFollowed(t *testing.T) {
	if _, err := os.Stat(envBinary); err != nil {
		t.Skipf("no %s here", envBinary)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	stubCredentialEnv(t, map[string]string{"AWS_CONFIG_FILE": "/data/aws/config"})
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	if !argPair(args, "-e", "AWS_CONFIG_FILE=/data/aws/config") {
		t.Errorf("the followed AWS_CONFIG_FILE is not pinned into the window: %v", args)
	}
	term := slices.Index(args, "--")
	if term < 0 || term+1 >= len(args) || args[term+1] != envBinary {
		t.Fatalf("the window command does not start with %s: %v", envBinary, args)
	}
	cmd := args[term+1:]
	if !argPair(cmd, "-u", "CODEX_HOME") || !argPair(cmd, "-u", "CLAUDE_CONFIG_DIR") {
		t.Errorf("the window command does not unset the relocations forgectl lacks: %v", cmd)
	}
	if argPair(cmd, "-u", "AWS_CONFIG_FILE") {
		t.Errorf("the window command unsets a relocation it also pins: %v", cmd)
	}
	s, _ := settingsFlagFrom(t, args)
	if !slices.Contains(s.Permissions.Deny, "Read(//data/aws/config)") || !slices.Contains(s.Sandbox.Filesystem.DenyRead, "/data/aws/config") {
		t.Errorf("the pinned AWS_CONFIG_FILE is not denied: %v / %v", s.Permissions.Deny, s.Sandbox.Filesystem.DenyRead)
	}
}
