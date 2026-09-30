package env

// The leftover scan's reach into the stashes (cameronsjo/forgectl#751). Real
// git throughout: the claim is about what `git stash --all` does with a
// gitignored scratch directory.
//
//   [x] A killed write's scratch directory, stashed by `git stash --all`, is
//       gone from the working tree (the premise) and the next set refuses,
//       naming stash@{0} and the directory, without the value
//   [x] A capture pushed down to stash@{1} by a later stash still refuses,
//       naming stash@{1}
//   [x] The check reads the target's own directory in the stash: a capture of
//       the same name at the repository root does not refuse a set on sub/.env,
//       and one in sub/ does
//   [x] A sibling target's stashed scratch does not refuse
//   [x] A confirmed repository whose stashes cannot be read refuses

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stashGitArgs are the identity flags a stash needs, kept out of any config.
var stashGitArgs = []string{"-c", "user.name=forgectl-test", "-c", "user.email=test@example.invalid"}

// commitControl commits the control file, which a stash needs a HEAD for.
func commitControl(t *testing.T, repo string) {
	t.Helper()
	if out, err := runEnvGit(t, repo, "add", "control.txt"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := runEnvGit(t, repo, append(stashGitArgs, "commit", "-q", "-m", "control")...); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

// stashAll runs `git stash --all` in repo and fails the test if dir survives
// it in the working tree, which would make every assertion after it vacuous.
func stashAll(t *testing.T, repo, dir string) {
	t.Helper()
	if out, err := runEnvGit(t, repo, append(stashGitArgs, "stash", "--all", "-q")...); err != nil {
		t.Fatalf("git stash --all: %v\n%s", err, out)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git stash --all left %s in the working tree (%v); the premise does not hold", dir, err)
	}
}

func TestScanRefusesScratchCapturedByStashAll(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	commitControl(t, repo)
	scratch := killedMidWrite(t, repo)
	stashAll(t, repo, filepath.Join(repo, scratch))

	err := setOn(t, repo, ".env")
	if err == nil {
		t.Fatal("set went ahead while stash@{0} held a killed write's scratch directory")
	}
	msg := err.Error()
	for _, want := range []string{"stash@{0}", scratch, "git stash --all"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, msg)
		}
	}
	assertNoSecretInOutput(t, leftoverSecret, "", msg)
	// Nothing was un-stashed or dropped.
	if list, err := runEnvGit(t, repo, "stash", "list"); err != nil || strings.Count(list, "\n") != 1 {
		t.Errorf("the stash list changed: %v\n%s", err, list)
	}
}

func TestScanRefusesAnOlderStashEntry(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	commitControl(t, repo)
	scratch := killedMidWrite(t, repo)
	stashAll(t, repo, filepath.Join(repo, scratch))

	// A later, ordinary stash pushes the capture down to stash@{1}.
	if err := os.WriteFile(filepath.Join(repo, "control.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := runEnvGit(t, repo, append(stashGitArgs, "stash", "-q")...); err != nil {
		t.Fatalf("git stash: %v\n%s", err, out)
	}

	err := setOn(t, repo, ".env")
	if err == nil {
		t.Fatal("set went ahead while stash@{1} held a killed write's scratch directory")
	}
	if msg := err.Error(); !strings.Contains(msg, "stash@{1}") || !strings.Contains(msg, scratch) {
		t.Errorf("the refusal does not name stash@{1} and %s:\n%s", scratch, msg)
	}
}

func TestScanReadsTheTargetsOwnDirectoryInTheStash(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// A tracked file keeps sub/ in the working tree once its scratch is
	// stashed away.
	if err := os.WriteFile(filepath.Join(sub, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := runEnvGit(t, repo, "add", "sub/keep.txt"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commitControl(t, repo)

	// A root .env's scratch shares sub/.env's name, but not its directory.
	rootScratch := killedMidWrite(t, repo)
	stashAll(t, repo, filepath.Join(repo, rootScratch))
	if err := setOn(t, sub, ".env"); err != nil {
		t.Fatalf("a stashed scratch directory at the repository root refused a set on sub/.env: %v", err)
	}
	// That set wrote sub/.env; take it away so the next capture is the only
	// change a stash holds.
	if err := os.Remove(filepath.Join(sub, ".env")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	subScratch := killedMidWrite(t, sub)
	stashAll(t, repo, filepath.Join(sub, subScratch))
	err := setOn(t, sub, ".env")
	if err == nil {
		t.Fatal("set on sub/.env went ahead while stash@{0} held its scratch directory")
	}
	if msg := err.Error(); !strings.Contains(msg, "stash@{0}") || !strings.Contains(msg, subScratch) {
		t.Errorf("the refusal does not name stash@{0} and %s:\n%s", subScratch, msg)
	}
}

func TestScanIgnoresASiblingTargetsStashedScratch(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	commitControl(t, repo)
	scratch := killedMidWrite(t, repo) // .env's
	stashAll(t, repo, filepath.Join(repo, scratch))

	if err := setOn(t, repo, ".env.local"); err != nil {
		t.Fatalf(".env's stashed scratch refused a set on .env.local: %v", err)
	}
}

func TestScanRefusesWhenTheStashesCannotBeRead(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	prev := stashGit
	stashGit = func(dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "stash" {
			return nil, errors.New("fatal: bad object refs/stash")
		}
		return prev(dir, args...)
	}
	t.Cleanup(func() { stashGit = prev })

	err := setOn(t, repo, ".env")
	if err == nil {
		t.Fatal("set went ahead in a repository whose stashes could not be read")
	}
	if !strings.Contains(err.Error(), errStashUnreadable.Error()) {
		t.Errorf("the refusal is not the unreadable-stash one: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(repo, ".env")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused set wrote .env anyway (%v)", statErr)
	}
}
