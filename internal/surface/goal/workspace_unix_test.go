//go:build unix

package goal

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPrepareWorkspaceUsesExactHeadAndGeneratedBranch(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	before, err := snapshotSource(context.Background(), fixture.repository)
	if err != nil {
		t.Fatalf("snapshot source before workspace: %v", err)
	}
	after, err := prepareWorkspace(context.Background(), fixture.store, fixture.state)
	if err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	if before != after {
		t.Fatalf("source snapshot changed during workspace preparation: before=%#v after=%#v", before, after)
	}
	if got := runFixtureGit(t, fixture.state.Worktree, "rev-parse", "HEAD"); got != fixture.state.Spec.BaseRevision {
		t.Fatalf("workspace HEAD = %q, want %q", got, fixture.state.Spec.BaseRevision)
	}
	if got := runFixtureGit(t, fixture.state.Worktree, "symbolic-ref", "--short", "HEAD"); got != fixture.state.Branch {
		t.Fatalf("workspace branch = %q, want %q", got, fixture.state.Branch)
	}
	gitEntry, err := os.Lstat(filepath.Join(fixture.state.Worktree, ".git"))
	if err != nil {
		t.Fatalf("workspace .git: %v", err)
	}
	if !gitEntry.Mode().IsRegular() {
		t.Fatalf("workspace .git mode = %v, want linked-worktree file", gitEntry.Mode())
	}
	if status := runFixtureGit(t, fixture.state.Worktree, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("new workspace status = %q, want clean", status)
	}
}

func TestPrepareWorkspaceRefusesSourceHeadChangeWithoutCreatingWorktree(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	writeGoalFile(t, filepath.Join(fixture.repository, "after-confirmation.txt"), "changed head\n")
	runFixtureGit(t, fixture.repository, "add", "--", "after-confirmation.txt")
	runFixtureGit(t, fixture.repository, "commit", "-q", "-m", "changed head")
	_, err := prepareWorkspace(context.Background(), fixture.store, fixture.state)
	requireErrorContains(t, err, "source HEAD changed after confirmation; no worktree was created")
	requireFileAbsent(t, fixture.state.Worktree)
	if got := runFixtureGit(t, fixture.repository, "branch", "--list", fixture.state.Branch); got != "" {
		t.Fatalf("source retained generated branch after refusal: %q", got)
	}
}

func TestPrepareWorkspaceExcludesDirtySourceFiles(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	writeGoalFile(t, filepath.Join(fixture.repository, "tracked.txt"), "dirty source\n")
	writeGoalFile(t, filepath.Join(fixture.repository, "untracked.txt"), "untracked source\n")
	before, err := snapshotSource(context.Background(), fixture.repository)
	if err != nil {
		t.Fatalf("snapshot dirty source: %v", err)
	}
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace from dirty source: %v", err)
	}
	after, err := snapshotSource(context.Background(), fixture.repository)
	if err != nil {
		t.Fatalf("snapshot source after workspace: %v", err)
	}
	if before != after {
		t.Fatalf("workspace preparation changed dirty source: before=%#v after=%#v", before, after)
	}
	contents, err := os.ReadFile(filepath.Join(fixture.state.Worktree, "tracked.txt")) //nolint:gosec // G304: fixture path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "committed\n" {
		t.Fatalf("workspace copied dirty tracked content %q, want committed content", contents)
	}
	requireFileAbsent(t, filepath.Join(fixture.state.Worktree, "untracked.txt"))
}

func TestSnapshotSourceDetectsUntrackedContentAndIndexChanges(t *testing.T) {
	fixture := newGoalFixture(t, ".")
	untracked := filepath.Join(fixture.repository, "untracked.txt")
	writeGoalFile(t, untracked, "first\n")
	before, err := snapshotSource(t.Context(), fixture.repository)
	if err != nil {
		t.Fatal(err)
	}
	writeGoalFile(t, untracked, "other\n")
	after, err := snapshotSource(t.Context(), fixture.repository)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != after.Status || before.UntrackedSHA256 == after.UntrackedSHA256 {
		t.Fatal("source snapshot did not distinguish same-status untracked content changes")
	}
	tracked := filepath.Join(fixture.repository, "tracked.txt")
	writeGoalFile(t, tracked, "staged one\n")
	runFixtureGit(t, fixture.repository, "add", "--", "tracked.txt")
	writeGoalFile(t, tracked, "working tree\n")
	before, err = snapshotSource(t.Context(), fixture.repository)
	if err != nil {
		t.Fatal(err)
	}
	writeGoalFile(t, tracked, "staged two\n")
	runFixtureGit(t, fixture.repository, "add", "--", "tracked.txt")
	writeGoalFile(t, tracked, "working tree\n")
	after, err = snapshotSource(t.Context(), fixture.repository)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != after.Status || before.PatchSHA256 != after.PatchSHA256 || before.IndexSHA256 == after.IndexSHA256 {
		t.Fatal("source snapshot did not distinguish same-status, same-worktree index changes")
	}
}

func TestSnapshotPatchAcceptsTrackedChanges(t *testing.T) {
	fixture := newGoalFixture(t, ".")
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	writeGoalFile(t, filepath.Join(fixture.state.Worktree, "tracked.txt"), "implementation\n")
	patch, err := snapshotPatch(context.Background(), fixture.state)
	if err != nil {
		t.Fatalf("snapshotPatch tracked change: %v", err)
	}
	if !slices.Equal(patch.Files, []string{"tracked.txt"}) {
		t.Fatalf("tracked patch files = %q, want [tracked.txt]", patch.Files)
	}
	if !strings.Contains(string(patch.Bytes), "+implementation") {
		t.Fatalf("tracked patch does not contain the changed line: %q", patch.Bytes)
	}
	if patch.SHA256 == "" || patch.SHA256 != Digest(patch.Bytes) {
		t.Fatalf("tracked patch digest = %q, want digest of patch bytes", patch.SHA256)
	}
}

func TestSnapshotPatchAcceptsScopedNewFiles(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	if err := os.Mkdir(filepath.Join(fixture.state.Worktree, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeGoalFile(t, filepath.Join(fixture.state.Worktree, "src", "new.txt"), "new implementation\n")
	patch, err := snapshotPatch(context.Background(), fixture.state)
	if err != nil {
		t.Fatalf("snapshotPatch new file: %v", err)
	}
	if !slices.Equal(patch.Files, []string{"src/new.txt"}) {
		t.Fatalf("new-file patch files = %q, want [src/new.txt]", patch.Files)
	}
	if !strings.Contains(string(patch.Bytes), "+new implementation") {
		t.Fatalf("new-file patch does not contain the added line: %q", patch.Bytes)
	}
}

func TestSnapshotPatchRejectsOutOfScopeChanges(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	writeGoalFile(t, filepath.Join(fixture.state.Worktree, "outside.txt"), "outside scope\n")
	_, err := snapshotPatch(context.Background(), fixture.state)
	requireErrorContains(t, err, "out-of-scope changed path: outside.txt")
}

func TestSnapshotPatchRejectsSymlinkChanges(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	if err := os.Mkdir(filepath.Join(fixture.state.Worktree, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	writeGoalFile(t, target, "outside target\n")
	link := filepath.Join(fixture.state.Worktree, "src", "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := snapshotPatch(context.Background(), fixture.state)
	requireErrorContains(t, err, "changed paths must not traverse or create symlinks or special files")
}

func TestSnapshotPatchRejectsImplementationHeadMutation(t *testing.T) {
	fixture := newGoalFixture(t, ".")
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	writeGoalFile(t, filepath.Join(fixture.state.Worktree, "mutated.txt"), "mutated head\n")
	runFixtureGit(t, fixture.state.Worktree, "add", "--", "mutated.txt")
	runFixtureGit(t, fixture.state.Worktree, "commit", "-q", "-m", "mutated head")
	_, err := snapshotPatch(context.Background(), fixture.state)
	requireErrorContains(t, err, "implementation changed HEAD or its assigned branch; result is not accepted")
}

func TestSnapshotPatchRejectsBranchMutationWithoutImportingSourceChanges(t *testing.T) {
	fixture := newGoalFixture(t, ".")
	if _, err := prepareWorkspace(context.Background(), fixture.store, fixture.state); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	runFixtureGit(t, fixture.state.Worktree, "checkout", "-q", "-b", "other")
	_, err := snapshotPatch(context.Background(), fixture.state)
	requireErrorContains(t, err, "implementation changed HEAD or its assigned branch; result is not accepted")
	if got := currentFixtureHead(t, fixture.repository); got != fixture.state.Spec.BaseRevision {
		t.Fatalf("source HEAD changed after implementation branch mutation: %q", got)
	}
}
