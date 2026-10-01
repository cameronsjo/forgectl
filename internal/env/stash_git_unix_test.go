//go:build unix

package env

// How the stash check runs git (stash.go, "How git is run"). Real git, and a
// fake one where only a fake can hang on cue.
//
//   [x] In a partial clone, a refs/stash naming a missing commit, and a stash
//       whose untracked tree is missing, both refuse, and neither runs the
//       repository's transport command (a canary) through a lazy fetch: not
//       core.sshCommand, and not an ext:: URL the repository allows with
//       protocol.ext.allow=always, which `-c protocol.allow=never` does not
//       override, and not a remote helper for a transport named "none".
//       Modelled on a git without GIT_NO_LAZY_FETCH, and again
//       with an inherited GIT_ALLOW_PROTOCOL that would allow ext
//   [x] An inherited GIT_DIR/GIT_WORK_TREE pointing at another repository does
//       not redirect the check: it still reads the target's own stashes
//   [x] The environment git runs with is gitenv's Local profile (its own
//       tests pin the scrub and the pins)
//   [x] A git that times out and leaves a grandchild holding its output pipe
//       returns within the wait delay, not when the grandchild exits

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// makePromisor turns repo into a partial clone whose promisor remote runs a
// command that creates canary: over ssh through core.sshCommand, through an
// ext:: URL the repository itself allows with protocol.ext.allow=always, or
// through a git-remote-none helper on PATH. Any lazy fetch runs it.
func makePromisor(t *testing.T, repo, canary, transport string) {
	t.Helper()
	// GIT_SSH_COMMAND and GIT_SSH override core.sshCommand, so an inherited
	// one (CI and sandboxes set them) would run in place of the canary and
	// make the test vacuous. t.Setenv restores them; Unsetenv clears them.
	for _, key := range []string{"GIT_SSH_COMMAND", "GIT_SSH", "GIT_ALLOW_PROTOCOL"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv %s: %v", key, err)
		}
	}
	config := [][2]string{
		{"core.repositoryformatversion", "1"},
		{"extensions.partialClone", "origin"},
		{"remote.origin.promisor", "true"},
	}
	switch transport {
	case "ssh":
		config = append(config,
			[2]string{"remote.origin.url", "ssh://example.invalid/unreachable"},
			[2]string{"core.sshCommand", "touch '" + canary + "'; false"})
	case "none":
		// A remote helper for a transport literally named "none", which an
		// allowlist of "none" would admit.
		bin := t.TempDir()
		helper := "#!/bin/sh\ntouch '" + canary + "'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, "git-remote-none"), []byte(helper), 0o700); err != nil { //nolint:gosec // G306: an executable stub
			t.Fatalf("WriteFile git-remote-none: %v", err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		config = append(config, [2]string{"remote.origin.url", "none::unreachable"})
	case "ext":
		// git's own `protocol.allow=never` on the command line loses to this
		// repository-level setting.
		config = append(config,
			[2]string{"protocol.ext.allow", "always"},
			[2]string{"remote.origin.url", "ext::sh -c touch% " + canary})
	default:
		t.Fatalf("unknown transport %q", transport)
	}
	for _, kv := range config {
		if out, err := runEnvGit(t, repo, "config", kv[0], kv[1]); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
}

func assertCanaryNeverRan(t *testing.T, canary string) {
	t.Helper()
	if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repository's transport command ran during the stash check (canary %s: %v)", canary, err)
	}
}

// withoutLazyFetchPin models a git older than 2.44, which ignores
// GIT_NO_LAZY_FETCH, by running git through a wrapper that drops it. Every
// other pin stays.
func withoutLazyFetchPin(t *testing.T) {
	t.Helper()
	gitenvtest.WithoutLazyFetchPin(t)
}

// missingStashCommit points refs/stash at a commit the repository lacks, so
// `git stash list` must fetch it.
func missingStashCommit(t *testing.T, repo string) {
	t.Helper()
	commitControl(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "stash"), []byte(strings.Repeat("ab", 20)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile refs/stash: %v", err)
	}
}

// missingUntrackedTree stashes a real capture and deletes its untracked tree,
// so the stash lists and `git ls-tree` must fetch the tree.
func missingUntrackedTree(t *testing.T, repo string) {
	t.Helper()
	commitControl(t, repo)
	scratch := killedMidWrite(t, repo)
	stashAll(t, repo, filepath.Join(repo, scratch))
	tree, err := runEnvGit(t, repo, "rev-parse", "stash@{0}^3^{tree}")
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, tree)
	}
	tree = strings.TrimSpace(tree)
	if err := os.Remove(filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatalf("the untracked tree is not a loose object to remove: %v", err)
	}
}

// TestStashCheckNeverLazyFetches runs every transport, both read paths, and
// with and without an inherited GIT_ALLOW_PROTOCOL that would allow ext, all
// against a modelled pre-2.44 git (no GIT_NO_LAZY_FETCH), so GIT_ALLOW_PROTOCOL
// is the only thing standing between the repository and its canary.
func TestStashCheckNeverLazyFetches(t *testing.T) {
	for _, transport := range []string{"ssh", "ext", "none"} {
		for _, path := range []struct {
			name  string
			setup func(*testing.T, string)
		}{
			{"stash list: refs/stash names a missing commit", missingStashCommit},
			{"ls-tree: a stash's untracked tree is missing", missingUntrackedTree},
		} {
			for _, inherited := range []string{"", "ext:ssh"} {
				name := transport + "/" + path.name
				if inherited != "" {
					name += "/inherited GIT_ALLOW_PROTOCOL=" + inherited
				}
				t.Run(name, func(t *testing.T) {
					captureWarnings(t)
					withoutLazyFetchPin(t)
					repo := envGitRepo(t)
					path.setup(t, repo)
					canary := filepath.Join(t.TempDir(), "canary")
					makePromisor(t, repo, canary, transport)
					if inherited != "" {
						t.Setenv("GIT_ALLOW_PROTOCOL", inherited)
					}

					err := setOn(t, repo, ".env")
					assertCanaryNeverRan(t, canary)
					if err == nil || !strings.Contains(err.Error(), errStashUnreadable.Error()) {
						t.Fatalf("set = %v, want the unreadable-stash refusal", err)
					}
				})
			}
		}
	}
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
