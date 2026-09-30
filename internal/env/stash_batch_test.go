package env

// The stash check reads every entry's untracked tree in one git process
// (cameronsjo/forgectl#939). Real git, counted through the stashGit seams.
//
//   [x] The number of git processes the check spawns does not grow with the
//       number of stash entries
//   [x] With ~200 `-u` stashes the check still finds the oldest entry's
//       scratch, in a constant number of spawns, and the batch is faster than
//       one ls-tree per entry
//   [x] The batch reads the same listings as the per-entry ls-tree, at the
//       repository root and in a subdirectory
//   [x] A subdirectory target whose directory is absent from every stash reads
//       as empty, and one whose directory tree is missing refuses

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// countStashGit counts every git process the stash check spawns, through both
// seams, and returns a func reporting the count.
func countStashGit(t *testing.T) func() int {
	t.Helper()
	n := 0
	prev, prevStdin := stashGit, stashGitStdin
	stashGit = func(dir string, args ...string) ([]byte, error) {
		n++
		return prev(dir, args...)
	}
	stashGitStdin = func(dir string, stdin []byte, args ...string) ([]byte, error) {
		n++
		return prevStdin(dir, stdin, args...)
	}
	t.Cleanup(func() { stashGit, stashGitStdin = prev, prevStdin })
	return func() int { return n }
}

// pushPlainStashes stashes count untracked files, one stash entry each.
func pushPlainStashes(t *testing.T, repo string, count int) {
	t.Helper()
	for i := range count {
		name := "u" + strconv.Itoa(i) + ".txt"
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if out, err := runEnvGit(t, repo, append(testIdentityArgs, "stash", "-u", "-q")...); err != nil {
			t.Fatalf("git stash -u: %v\n%s", err, out)
		}
	}
}

// stashRepoWithScratch returns a repository whose oldest stash holds a killed
// write's scratch directory, under count newer plain `-u` stashes.
func stashRepoWithScratch(t *testing.T, count int) (repo, scratch string) {
	t.Helper()
	repo = envGitRepo(t)
	commitControl(t, repo)
	scratch = killedMidWrite(t, repo)
	stashAll(t, repo, filepath.Join(repo, scratch))
	pushPlainStashes(t, repo, count)
	return repo, scratch
}

func TestStashCheckSpawnsDoNotGrowWithStashCount(t *testing.T) {
	var counts []int
	for _, n := range []int{1, 4, 16} {
		captureWarnings(t)
		repo, scratch := stashRepoWithScratch(t, n)
		spawned := countStashGit(t)
		err := setOn(t, repo, ".env")
		if err == nil || !strings.Contains(err.Error(), "stash@{"+strconv.Itoa(n)+"}") || !strings.Contains(err.Error(), scratch) {
			t.Fatalf("with %d newer stashes, set = %v; want the refusal naming stash@{%d} and %s", n, err, n, scratch)
		}
		counts = append(counts, spawned())
	}
	if counts[0] != counts[1] || counts[1] != counts[2] {
		t.Fatalf("git spawns by stash count 1/4/16 = %v; want one constant", counts)
	}
	if counts[0] > 4 {
		t.Fatalf("the stash check spawned %d git processes; want at most 4 (rev-parse, stash list, cat-file --batch-check, cat-file --batch)", counts[0])
	}
}

func TestStashCheckWithManyStashes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds ~200 stashes")
	}
	const n = 200
	captureWarnings(t)
	repo, scratch := stashRepoWithScratch(t, n)
	spawned := countStashGit(t)

	start := time.Now()
	err := setOn(t, repo, ".env")
	batch := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "stash@{"+strconv.Itoa(n)+"}") || !strings.Contains(err.Error(), scratch) {
		t.Fatalf("set = %v; want the refusal naming the oldest stash, stash@{%d}", err, n)
	}
	checkSpawns := spawned()
	if checkSpawns > 4 {
		t.Fatalf("%d stashes cost %d git spawns; want a constant", n, checkSpawns)
	}

	// The same scan, one ls-tree per entry, for scale.
	commits := stashThirdParents(t, repo)
	start = time.Now()
	if _, err := stashListingsPerEntry(repo, "", commits); err != nil {
		t.Fatalf("per-entry listing: %v", err)
	}
	perEntry := time.Since(start)
	t.Logf("%d stashes: whole check (batched) %v, %d spawns; per-entry ls-tree alone %v, %d spawns", n, batch, checkSpawns, perEntry, len(commits))
	if batch > perEntry {
		t.Errorf("the batched check (%v) took longer than just the per-entry ls-trees (%v)", batch, perEntry)
	}
}

// stashThirdParents returns the untracked-files commit of every stash entry.
func stashThirdParents(t *testing.T, repo string) []string {
	t.Helper()
	out, err := runEnvGit(t, repo, "stash", "list", "--format=%H %P")
	if err != nil {
		t.Fatalf("stash list: %v\n%s", err, out)
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) >= 4 {
			commits = append(commits, f[3])
		}
	}
	return commits
}

func TestStashBatchListsWhatLsTreeLists(t *testing.T) {
	captureWarnings(t)
	repo, _ := stashRepoWithScratch(t, 3)
	sub := filepath.Join(repo, "sub", "deep")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, p := range []string{"sub/a.txt", "sub/deep/b.txt", "sub/deep/c.txt", "a b/k.txt", "sub/a b/k.txt", "t\tab/k.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if out, err := runEnvGit(t, repo, append(testIdentityArgs, "stash", "-u", "-q")...); err != nil {
		t.Fatalf("git stash -u: %v\n%s", err, out)
	}
	commits := stashThirdParents(t, repo)
	for _, prefix := range []string{"", "sub/", "sub/deep/", "nosuch/", "sub/nosuch/", "control.txt/", "a b/", "no such/", "sub/a b/", "t\tab/"} {
		want, err := stashListingsPerEntry(repo, prefix, commits)
		if err != nil {
			t.Fatalf("per-entry %q: %v", prefix, err)
		}
		got, err := stashListingsBatch(repo, prefix, commits)
		if err != nil {
			t.Fatalf("batch %q: %v", prefix, err)
		}
		norm := func(l [][]string) [][]string {
			out := make([][]string, len(l))
			for i, paths := range l {
				for _, p := range paths {
					if p != "" {
						out[i] = append(out[i], p)
					}
				}
				slices.Sort(out[i])
			}
			return out
		}
		if !slices.EqualFunc(norm(want), norm(got), slices.Equal[[]string]) {
			t.Errorf("prefix %q: batch = %q, ls-tree = %q", prefix, norm(got), norm(want))
		}
	}
	// The newest entry holds sub/deep/b.txt and c.txt, so the deep prefix must
	// not read as empty: that would make the comparison above vacuous.
	got, err := stashListingsBatch(repo, "sub/deep/", commits)
	if err != nil || len(got[0]) != 2 {
		t.Fatalf("batch sub/deep/ = %q, %v; want two entries in the newest stash", got, err)
	}
}

func TestStashBatchMissingSubdirectoryTree(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := runEnvGit(t, repo, "add", "sub/keep.txt"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commitControl(t, repo)
	sub := filepath.Join(repo, "sub")
	scratch := killedMidWrite(t, sub)
	stashAll(t, repo, filepath.Join(sub, scratch))

	// A directory absent from the stash is an empty listing, not a refusal.
	if err := setOn(t, repo, ".env"); err != nil {
		t.Fatalf("a stash without the target's directory refused the set: %v", err)
	}

	tree, err := runEnvGit(t, repo, "rev-parse", "stash@{0}^3:sub")
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, tree)
	}
	tree = strings.TrimSpace(tree)
	if err := os.Remove(filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatalf("the sub tree is not a loose object to remove: %v", err)
	}
	err = setOn(t, sub, ".env")
	if err == nil || !strings.Contains(err.Error(), errStashUnreadable.Error()) {
		t.Fatalf("set with the sub/ tree missing = %v; want the unreadable-stash refusal", err)
	}
}

// A SHA-256 repository's raw trees carry 32-byte hashes, and the width comes
// from the commit id. (git 2.43's `git stash` cannot run in such a repository,
// so the parse is checked directly.)
func TestStashTreeParsesBothObjectFormats(t *testing.T) {
	for _, commitID := range []string{strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		hashLen := stashHashLen(commitID)
		if hashLen != len(commitID)/2 {
			t.Fatalf("stashHashLen(%d hex) = %d", len(commitID), hashLen)
		}
		raw := "100644 one\x00" + strings.Repeat("\x01", hashLen) + "40000 two\x00" + strings.Repeat("\x02", hashLen)
		got, err := parseStashTree([]byte(raw), hashLen)
		want := []stashTreeEntry{{"100644", "one"}, {"40000", "two"}}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%d-hex ids: parseStashTree = %v, %v; want %v", len(commitID), got, err, want)
		}
	}
}

// A target directory whose name holds whitespace: cat-file echoes the object
// name on a missing line, so the parser must match it whole, not by fields.
func TestStashCheckInADirectoryWithASpaceInItsName(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	spaced := filepath.Join(repo, "a b")
	if err := os.Mkdir(spaced, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spaced, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := runEnvGit(t, repo, "add", "a b/keep.txt"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commitControl(t, repo)
	pushPlainStashes(t, repo, 1) // an unrelated -u stash that lacks "a b/"

	if err := setOn(t, spaced, ".env"); err != nil {
		t.Fatalf("an unrelated stash refused a set in a directory with a space in its name: %v", err)
	}
	if err := os.Remove(filepath.Join(spaced, ".env")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// And a real capture in that directory still refuses.
	scratch := killedMidWrite(t, spaced)
	stashAll(t, repo, filepath.Join(spaced, scratch))
	err := setOn(t, spaced, ".env")
	if err == nil || !strings.Contains(err.Error(), "stash@{0}") || !strings.Contains(err.Error(), scratch) {
		t.Fatalf("set = %v; want the refusal naming stash@{0} and %s", err, scratch)
	}
}

func TestStashBatchOutputParsersRefuseExtraOrMalformedOutput(t *testing.T) {
	oid := strings.Repeat("a", 40)
	specs := []string{oid + ":", oid + ":a b"}
	good := oid + " tree 0\n" + oid + ":a b missing\n"
	if kinds, err := stashBatchCheck([]byte(good), specs); err != nil || !slices.Equal(kinds, []string{"tree", "missing"}) {
		t.Fatalf("good batch-check output = %v, %v", kinds, err)
	}
	for name, out := range map[string]string{
		"an extra record":       good + oid + " tree 0\n",
		"a truncated output":    oid + " tree 0\n",
		"a missing for another": oid + " tree 0\n" + oid + ":x missing\n",
		"a non-numeric size":    oid + " tree x\n" + oid + ":a b missing\n",
	} {
		if _, err := stashBatchCheck([]byte(out), specs); err == nil {
			t.Errorf("stashBatchCheck accepted %s", name)
		}
	}
	rec := oid + " tree 0\n\n"
	if c, err := stashBatchTrees([]byte(rec), 1); err != nil || len(c) != 1 {
		t.Fatalf("good batch output = %v, %v", c, err)
	}
	for name, out := range map[string]string{
		"an extra record": rec + rec,
		"a short content": oid + " tree 5\nab\n",
		"a blob":          oid + " blob 0\n\n",
		"no terminator":   oid + " tree 0\nX",
	} {
		if _, err := stashBatchTrees([]byte(out), 1); err == nil {
			t.Errorf("stashBatchTrees accepted %s", name)
		}
	}
}

// A prefix level that is a file in a stash is named to batch-check only: its
// content is never asked of `cat-file --batch`, so it cannot be read into
// memory.
func TestStashBatchNeverReadsANonTreeLevel(t *testing.T) {
	captureWarnings(t)
	repo := envGitRepo(t)
	commitControl(t, repo)
	big := filepath.Join(repo, "zz")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 1<<20)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, err := runEnvGit(t, repo, append(testIdentityArgs, "stash", "-u", "-q")...); err != nil {
		t.Fatalf("git stash -u: %v\n%s", err, out)
	}
	if err := os.Mkdir(big, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	var batchInputs []string
	prev := stashGitStdin
	stashGitStdin = func(dir string, stdin []byte, args ...string) ([]byte, error) {
		if slices.Contains(args, "--batch") {
			batchInputs = append(batchInputs, string(stdin))
		}
		return prev(dir, stdin, args...)
	}
	t.Cleanup(func() { stashGitStdin = prev })

	if err := setOn(t, big, ".env"); err != nil {
		t.Fatalf("a stash holding a file named zz refused a set in zz/: %v", err)
	}
	if len(batchInputs) != 1 || strings.Contains(batchInputs[0], "zz") {
		t.Fatalf("cat-file --batch was asked for %q; want only the root tree, never the file zz", batchInputs)
	}
}
