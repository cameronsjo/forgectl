//go:build unix

package env

// How the stash check runs git (stash.go, "How git is run"). Real git, and a
// fake one where only a fake can hang on cue.
//
//   [x] In a partial clone, a refs/stash naming a missing commit, and a stash
//       whose untracked tree is missing, both refuse, and neither runs the
//       repository's core.sshCommand (a canary) through a lazy fetch
//   [x] An inherited GIT_DIR/GIT_WORK_TREE pointing at another repository does
//       not redirect the check: it still reads the target's own stashes
//   [x] stashGitEnv drops every variable git clears before entering another
//       repository, and the numbered GIT_CONFIG_KEY_n/VALUE_n pairs, and keeps
//       the rest
//   [x] A git that times out and leaves a grandchild holding its output pipe
//       returns within the wait delay, not when the grandchild exits

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// makePromisor turns repo into a partial clone whose promisor remote is
// reached over ssh, with a core.sshCommand that creates canary. Any lazy
// fetch runs it.
func makePromisor(t *testing.T, repo, canary string) {
	t.Helper()
	// GIT_SSH_COMMAND and GIT_SSH override core.sshCommand, so an inherited
	// one (CI and sandboxes set them) would run in place of the canary and
	// make the test vacuous. t.Setenv restores them; Unsetenv clears them.
	for _, key := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv %s: %v", key, err)
		}
	}
	for _, kv := range [][2]string{
		{"core.repositoryformatversion", "1"},
		{"extensions.partialClone", "origin"},
		{"remote.origin.url", "ssh://example.invalid/unreachable"},
		{"remote.origin.promisor", "true"},
		{"core.sshCommand", "touch '" + canary + "'; false"},
	} {
		if out, err := runEnvGit(t, repo, "config", kv[0], kv[1]); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
}

func assertCanaryNeverRan(t *testing.T, canary string) {
	t.Helper()
	if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repository's core.sshCommand ran during the stash check (canary %s: %v)", canary, err)
	}
}

func TestStashCheckNeverLazyFetches(t *testing.T) {
	t.Run("refs/stash names a missing commit", func(t *testing.T) {
		captureWarnings(t)
		repo := envGitRepo(t)
		commitControl(t, repo)
		canary := filepath.Join(t.TempDir(), "canary")
		makePromisor(t, repo, canary)
		if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "stash"), []byte(strings.Repeat("ab", 20)+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile refs/stash: %v", err)
		}

		err := setOn(t, repo, ".env")
		assertCanaryNeverRan(t, canary)
		if err == nil || !strings.Contains(err.Error(), errStashUnreadable.Error()) {
			t.Fatalf("set with an unreadable stash = %v, want the unreadable-stash refusal", err)
		}
	})

	t.Run("a stash's untracked tree is missing", func(t *testing.T) {
		captureWarnings(t)
		repo := envGitRepo(t)
		commitControl(t, repo)
		scratch := killedMidWrite(t, repo)
		stashAll(t, repo, filepath.Join(repo, scratch))
		tree, err := runEnvGit(t, repo, "rev-parse", "stash@{0}^3^{tree}")
		if err != nil {
			t.Fatalf("rev-parse: %v\n%s", err, tree)
		}
		tree = strings.TrimSpace(tree)
		loose := filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])
		if err := os.Remove(loose); err != nil {
			t.Fatalf("the untracked tree is not a loose object to remove: %v", err)
		}
		canary := filepath.Join(t.TempDir(), "canary")
		makePromisor(t, repo, canary)

		err = setOn(t, repo, ".env")
		assertCanaryNeverRan(t, canary)
		if err == nil || !strings.Contains(err.Error(), errStashUnreadable.Error()) {
			t.Fatalf("set with an unreadable stash tree = %v, want the unreadable-stash refusal", err)
		}
	})
}

func TestStashCheckIgnoresAnInheritedGitDir(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	commitControl(t, repo)
	scratch := killedMidWrite(t, repo)
	stashAll(t, repo, filepath.Join(repo, scratch))
	other := envGitRepo(t)
	commitControl(t, other)

	// What a git hook, or a caller that exported them, hands its children.
	// Set only now: runEnvGit above inherits the environment too.
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))

	err := setOn(t, repo, ".env")
	if err == nil || !strings.Contains(err.Error(), "stash@{0}") {
		t.Fatalf("with GIT_DIR pointing at another repository, set = %v; want the refusal naming stash@{0} in the target's own repository", err)
	}
}

func TestStashGitEnvScrubsRepositoryVariables(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/h", "GIT_CEILING_DIRECTORIES=/c", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=/evil"}
	for key := range stashGitScrubbed {
		in = append(in, key+"=x")
	}
	got := stashGitEnv(in)
	for _, kv := range got {
		key, _, _ := strings.Cut(kv, "=")
		if stashGitScrubbed[key] || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			t.Errorf("stashGitEnv kept %s", kv)
		}
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "GIT_CEILING_DIRECTORIES=/c", "GIT_NO_LAZY_FETCH=1"} {
		if !slices.Contains(got, want) {
			t.Errorf("stashGitEnv dropped or lacks %s: %v", want, got)
		}
	}
}

func TestStashGitReturnsWithinTheWaitDelay(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep is not on PATH: %v", err)
	}
	bin := t.TempDir()
	// The grandchild inherits stdout and outlives the killed git. sleep is
	// named by its absolute path: PATH is about to hold only the fake.
	script := "#!/bin/sh\n'" + sleep + "' 20 &\nexec '" + sleep + "' 20\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PATH", bin)
	prev := stashGitTimeout
	stashGitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stashGitTimeout = prev })

	start := time.Now()
	_, err = stashGit(t.TempDir(), "rev-parse", "--show-prefix")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a git that never finished reported success")
	}
	if elapsed < stashGitTimeout {
		t.Fatalf("stashGit returned after %v, before the timeout; the fake git did not hang, so this proves nothing", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("stashGit returned after %v; a timed-out call outlived its deadline by the grandchild's lifetime", elapsed)
	}
}
