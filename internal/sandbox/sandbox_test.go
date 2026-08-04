// Test plan for sandbox.go
//
//   - Sandbox local-repo path issues `git -C <repo> worktree add -- <dir> <ref>`
//     (assert FakeRunner.Calls argv, including the `--` option-terminator).
//   - Sandbox alwaysClone/remote issues `git clone --branch <ref> -- <repo> <dir>`;
//     clone-without-ref omits --branch.
//   - RejectOptionLike rejects a leading-'-' repo and ref before any Runner call.
//   - Teardown is idempotent: an empty workspace and an already-removed dir
//     are both no-ops, and neither issues a Runner call.
//   - Teardown refuses anything whose RESOLVED base name lacks
//     WorkspacePrefix — an unprefixed dir, a prefix-named symlink pointing
//     elsewhere, a dangling symlink, a "..", the prefix in a parent component
//     or merely contained in the base name, a relative path, / and /tmp.
//   - Teardown still removes a genuine forgectl-workflow-* dir (the vacuity
//     guard for the refusal cases above).
//   - Teardown follows a symlinked PARENT component (accepted behaviour, not
//     a desired guarantee — pinned so a change to it is visible).
//   - WithinWorkspace rejects a symlink escaping the workspace.
package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestSandbox_LocalRepo_WorktreeAdd covers the cheap-path default: a local
// repo with alwaysClone=false must issue `git -C <repo> worktree add -- <dir>
// <ref>`, never a clone.
func TestSandbox_LocalRepo_WorktreeAdd(t *testing.T) {
	repoDir := t.TempDir()
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "", nil },
	}

	dir, err := Sandbox(context.Background(), fake, repoDir, "main", false)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 Runner call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	call := fake.Calls[0]
	if call.Name != "git" {
		t.Errorf("call.Name = %q, want git", call.Name)
	}
	want := []string{"-C", repoDir, "worktree", "add", "--", dir, "main"}
	if len(call.Args) != len(want) {
		t.Fatalf("args = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q (full args: %v)", i, call.Args[i], w, call.Args)
		}
	}
}

// TestSandbox_LocalRepo_WorktreeAdd_DefaultsRefToHEAD covers the ref-omitted
// case: a local worktree with no ref defaults to HEAD.
func TestSandbox_LocalRepo_WorktreeAdd_DefaultsRefToHEAD(t *testing.T) {
	repoDir := t.TempDir()
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "", nil },
	}

	dir, err := Sandbox(context.Background(), fake, repoDir, "", false)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	call := fake.Last()
	if call.Args[len(call.Args)-1] != "HEAD" {
		t.Errorf("expected final worktree add arg HEAD, got %q (args: %v)", call.Args[len(call.Args)-1], call.Args)
	}
}

// TestSandbox_AlwaysClone_RemoteRepo covers alwaysClone=true / a remote-
// looking repo: it must `git clone --branch <ref> -- <repo> <dir>`.
func TestSandbox_AlwaysClone_RemoteRepo(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "", nil },
	}

	dir, err := Sandbox(context.Background(), fake, "cameronsjo/forgectl", "main", true)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 Runner call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	call := fake.Calls[0]
	want := []string{"clone", "--branch", "main", "--", "cameronsjo/forgectl", dir}
	if len(call.Args) != len(want) {
		t.Fatalf("args = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q (full args: %v)", i, call.Args[i], w, call.Args)
		}
	}
}

// TestSandbox_Clone_NoRef_OmitsBranchFlag covers the ref-omitted clone case:
// git clone --branch wants a real branch/tag name, so an empty ref must omit
// --branch entirely rather than passing an empty value.
func TestSandbox_Clone_NoRef_OmitsBranchFlag(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "", nil },
	}

	dir, err := Sandbox(context.Background(), fake, "cameronsjo/forgectl", "", true)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	call := fake.Last()
	want := []string{"clone", "--", "cameronsjo/forgectl", dir}
	if len(call.Args) != len(want) {
		t.Fatalf("args = %v, want %v (branch flag should be omitted)", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q (full args: %v)", i, call.Args[i], w, call.Args)
		}
	}
}

// TestSandbox_RejectsOptionLikeRepoRef locks the git-argument-injection
// defense: a repo or ref beginning with '-' is refused before any Runner call.
func TestSandbox_RejectsOptionLikeRepoRef(t *testing.T) {
	repoDir := t.TempDir()
	cases := []struct {
		name string
		repo string
		ref  string
	}{
		{"option-like repo", "--upload-pack=touch /tmp/pwned", ""},
		{"option-like ref", repoDir, "--upload-pack=x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			if _, err := Sandbox(context.Background(), fake, tc.repo, tc.ref, false); err == nil {
				t.Fatal("expected rejection of an option-like value, got nil")
			}
			if len(fake.Calls) != 0 {
				t.Errorf("git must not run for a rejected value, got calls: %+v", fake.Calls)
			}
		})
	}
}

// TestRejectOptionLike covers the guard directly.
func TestRejectOptionLike(t *testing.T) {
	if err := RejectOptionLike("repo", "-x"); err == nil {
		t.Error("expected rejection of a leading '-' value")
	}
	if err := RejectOptionLike("repo", "cameronsjo/forgectl"); err != nil {
		t.Errorf("expected no error for a normal value, got %v", err)
	}
}

// mkWorkspace makes a real, prefix-carrying sandbox dir under root.
func mkWorkspace(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, WorkspacePrefix+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	return dir
}

// TestTeardown_EmptyWorkspaceIsNoOp covers the documented idempotency floor:
// an empty workspace is a no-op and issues no Runner call.
func TestTeardown_EmptyWorkspaceIsNoOp(t *testing.T) {
	fake := &exec.FakeRunner{}
	if err := Teardown(context.Background(), fake, ""); err != nil {
		t.Fatalf("Teardown on empty workspace: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("Teardown must not shell out, got calls: %+v", fake.Calls)
	}
}

// TestTeardown_AlreadyRemovedIsNoOp proves the Lstat-ENOENT leg keeps
// idempotency intact: tearing down a real workspace twice must not error, and
// the second call must NOT be reported as a refusal.
func TestTeardown_AlreadyRemovedIsNoOp(t *testing.T) {
	fake := &exec.FakeRunner{}
	workspace := mkWorkspace(t, t.TempDir(), "gone")

	if err := Teardown(context.Background(), fake, workspace); err != nil {
		t.Fatalf("first Teardown: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace should be gone after Teardown, stat err = %v", err)
	}
	if err := Teardown(context.Background(), fake, workspace); err != nil {
		if errors.Is(err, ErrUnsafeTeardown) {
			t.Fatalf("an already-removed workspace must be a no-op, not a refusal: %v", err)
		}
		t.Fatalf("second (idempotent) Teardown must not error, got: %v", err)
	}
}

// TestTeardown_AcceptsRealSandbox is the VACUITY GUARD for every refusal test
// below: a genuine forgectl-workflow-* dir with content is still removed. If
// this fails, the refusals prove nothing because everything is refused.
func TestTeardown_AcceptsRealSandbox(t *testing.T) {
	workspace := mkWorkspace(t, t.TempDir(), "real")
	if err := os.WriteFile(filepath.Join(workspace, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := Teardown(context.Background(), &exec.FakeRunner{}, workspace); err != nil {
		t.Fatalf("Teardown of a genuine sandbox must succeed, got: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("genuine sandbox should be gone, stat err = %v", err)
	}
}

// TestTeardown_RefusesSymlinkNamedWithPrefix is the order-of-resolution case:
// a link NAMED forgectl-workflow-* pointing at an unprefixed victim must be
// refused. Testing the literal base name before resolving would let the link's
// name alone authorize removing the target.
func TestTeardown_RefusesSymlinkNamedWithPrefix(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(filepath.Join(victim, "sub"), 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	link := filepath.Join(root, WorkspacePrefix+"x")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	err := Teardown(context.Background(), &exec.FakeRunner{}, link)
	if !errors.Is(err, ErrUnsafeTeardown) {
		t.Fatalf("expected ErrUnsafeTeardown, got %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim must survive: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link itself must survive: %v", err)
	}
}

// TestTeardown_RefusesDanglingSymlink pins the fail-closed EvalSymlinks trade:
// a prefixed link whose target is gone cannot be resolved, so it is refused
// rather than falling back to the literal name.
func TestTeardown_RefusesDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, WorkspacePrefix+"dangling")
	if err := os.Symlink(filepath.Join(root, "nonexistent"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := Teardown(context.Background(), &exec.FakeRunner{}, link); !errors.Is(err, ErrUnsafeTeardown) {
		t.Fatalf("expected ErrUnsafeTeardown for a dangling symlink, got %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the dangling link must survive: %v", err)
	}
}

// TestTeardown_RefusesUnprefixedBaseName covers the plain case that motivated
// the guard: an arbitrary existing directory is not a sandbox.
func TestTeardown_RefusesUnprefixedBaseName(t *testing.T) {
	dir := t.TempDir()
	if err := Teardown(context.Background(), &exec.FakeRunner{}, dir); !errors.Is(err, ErrUnsafeTeardown) {
		t.Fatalf("expected ErrUnsafeTeardown, got %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir must survive: %v", err)
	}
}

// TestTeardown_RefusesDotDotTraversal covers a path whose literal text carries
// the prefix in a component but which climbs back out to an unprefixed target.
func TestTeardown_RefusesDotDotTraversal(t *testing.T) {
	root := t.TempDir()
	mkWorkspace(t, root, "x")
	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}

	target := filepath.Join(root, WorkspacePrefix+"x", "..", "victim")
	if err := Teardown(context.Background(), &exec.FakeRunner{}, target); !errors.Is(err, ErrUnsafeTeardown) {
		t.Fatalf("expected ErrUnsafeTeardown, got %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim must survive: %v", err)
	}
}

// TestTeardown_RefusesPrefixNotAtBaseName covers the two near-misses: the
// prefix sitting in a PARENT component, and a base name that merely CONTAINS
// the prefix rather than starting with it.
func TestTeardown_RefusesPrefixNotAtBaseName(t *testing.T) {
	root := t.TempDir()

	parent := mkWorkspace(t, root, "parent")
	inner := filepath.Join(parent, "victim")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatalf("mkdir inner: %v", err)
	}
	notPrefixed := filepath.Join(root, "not"+WorkspacePrefix+"x")
	if err := os.Mkdir(notPrefixed, 0o700); err != nil {
		t.Fatalf("mkdir notPrefixed: %v", err)
	}

	for _, target := range []string{inner, notPrefixed} {
		if err := Teardown(context.Background(), &exec.FakeRunner{}, target); !errors.Is(err, ErrUnsafeTeardown) {
			t.Errorf("%s: expected ErrUnsafeTeardown, got %v", target, err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("%s must survive: %v", target, err)
		}
	}
}

// TestTeardown_UnlinksSymlinkWithoutFollowingToTarget is the ONLY test that
// pins the ACT-UNRESOLVED half of the invariant. The header comment on
// Teardown, the one on pr.validateWorkspace, and breadcrumb_test.go:293 all
// describe it, but until this test existed, mutating os.RemoveAll(workspace)
// to os.RemoveAll(resolved) passed internal/sandbox, internal/pr, and
// internal/workflow clean — the exact regression all three comments forbid.
//
// The shape: a NON-prefixed symlink pointing at a PREFIXED directory. The
// guard resolves it, sees a prefixed base, and accepts. Because the delete
// then uses the UNRESOLVED string, os.RemoveAll unlinks the link and never
// follows it. Resolving first would delete the real directory instead.
func TestTeardown_UnlinksSymlinkWithoutFollowingToTarget(t *testing.T) {
	root := t.TempDir()
	target := mkWorkspace(t, root, "target")
	keep := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(keep, []byte("must survive"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "plainlink")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Accepted: the RESOLVED base name carries the prefix.
	if err := Teardown(context.Background(), &exec.FakeRunner{}, link); err != nil {
		t.Fatalf("Teardown of a link resolving to a sandbox must succeed, got: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link itself should be unlinked, stat err = %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("ACT-UNRESOLVED violated: the link's target was deleted: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("ACT-UNRESOLVED violated: the target's contents were deleted: %v", err)
	}
}

// TestTeardown_RefusesRoot covers the worst-case sinks directly.
func TestTeardown_RefusesRoot(t *testing.T) {
	for _, target := range []string{"/", "/tmp"} {
		if err := Teardown(context.Background(), &exec.FakeRunner{}, target); !errors.Is(err, ErrUnsafeTeardown) {
			t.Fatalf("%s: expected ErrUnsafeTeardown, got %v", target, err)
		}
	}
}

// TestTeardown_RefusesRelativePath covers the absolute-path requirement: a
// relative name can carry the prefix while resolving anywhere off the cwd.
func TestTeardown_RefusesRelativePath(t *testing.T) {
	if err := Teardown(context.Background(), &exec.FakeRunner{}, WorkspacePrefix+"x"); !errors.Is(err, ErrUnsafeTeardown) {
		t.Fatalf("expected ErrUnsafeTeardown for a relative path, got %v", err)
	}
}

// FuzzTeardown_NeverDeletesUnprefixed asserts the property over arbitrary path
// bytes rooted in a canary tree: Teardown either errors, or the path it acted
// on resolved to a base name carrying WorkspacePrefix.
//
// CONTAINMENT IS ESTABLISHED BEFORE THE CALL, NOT ASSERTED AFTER. filepath.Join
// cleans "../", so a rel of "../../forgectl-workflow-anything" resolves out of
// the fuzz tree and into the process temp root — exactly where
// os.MkdirTemp("", WorkspacePrefix+"*") puts REAL sandboxes. Teardown would
// then be CORRECT to delete it (the base name carries the prefix), and an
// after-the-fact canary check inside the tree could not see it. Under
// `go test -fuzz` that destroys a concurrently running forgectl's workspace, or
// a CI agent's. So: any input whose joined path leaves root is dropped before
// Teardown is ever called.
//
// The tree is rooted two levels deep so escape-SHAPED inputs stay explorable —
// "../" and "../../" still climb, they just land inside root — and only deeper
// climbs are dropped. outsideCanary is a PREFIXED directory outside root: it is
// one Teardown would happily delete, so a regression of this exact kind fails
// the test instead of passing silently.
func FuzzTeardown_NeverDeletesUnprefixed(f *testing.F) {
	f.Add("victim")
	f.Add(WorkspacePrefix + "x")
	f.Add(WorkspacePrefix + "x/../victim")
	f.Add("../victim")
	f.Add("../../victim")
	f.Add("../../../" + WorkspacePrefix + "escape")
	f.Add("plink/" + WorkspacePrefix + "x")
	f.Add("")
	f.Add("/")

	f.Fuzz(func(t *testing.T, rel string) {
		root := t.TempDir()
		base := filepath.Join(root, "tree", "sub")
		if err := os.MkdirAll(base, 0o700); err != nil {
			t.Fatalf("mkdir base: %v", err)
		}
		canaries := []string{filepath.Join(base, "victim"), filepath.Join(root, "canary")}
		for _, c := range canaries {
			if err := os.MkdirAll(c, 0o700); err != nil {
				t.Fatalf("mkdir canary: %v", err)
			}
		}
		mkWorkspace(t, base, "x")

		// A prefixed — therefore deletable — canary OUTSIDE the containment
		// boundary. If containment ever regresses, this is what dies.
		outsideCanary := mkWorkspace(t, t.TempDir(), "outside-canary")

		target := filepath.Join(root, "tree", "sub", rel)
		// Containment, before the call. filepath.Join has already cleaned the
		// path, so this comparison is the real reach of the delete. The tree
		// contains no symlinks, so lexical containment is the whole story.
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return
		}

		_, lstatErr := os.Lstat(target)
		resolvedBefore, resolveErr := filepath.EvalSymlinks(target)

		err := Teardown(context.Background(), &exec.FakeRunner{}, target)
		// A nil return for a path that never existed is the documented
		// already-removed no-op, not an acceptance.
		if err == nil && lstatErr == nil {
			if resolveErr != nil {
				t.Fatalf("Teardown accepted %q whose target did not resolve: %v", target, resolveErr)
			}
			if !strings.HasPrefix(filepath.Base(resolvedBefore), WorkspacePrefix) {
				t.Fatalf("Teardown accepted %q resolving to %q, which lacks %q", target, resolvedBefore, WorkspacePrefix)
			}
		}
		for _, c := range canaries {
			if _, statErr := os.Stat(c); statErr != nil {
				t.Fatalf("canary %s destroyed by Teardown(%q): %v", c, target, statErr)
			}
		}
		if _, statErr := os.Stat(outsideCanary); statErr != nil {
			t.Fatalf("CONTAINMENT BREACH: Teardown(%q) reached %s outside the fuzz tree: %v", target, outsideCanary, statErr)
		}
	})
}

// TestTeardown_FollowsSymlinkedParent documents ACCEPTED BEHAVIOUR, not
// desired behaviour: os.RemoveAll declines to follow only the FINAL path
// component, so a workspace reached through a symlinked PARENT is deleted at
// its real location.
//
// It matters because issue #184 retired the temp-root bound in
// pr.validateWorkspace, which rejected exactly this shape (WithinWorkspace
// resolves symlinks on both sides). Nothing rejects it now, so the reach is
// real and worth pinning — if a future change makes Teardown resolve or
// refuse a symlinked parent, this test fails and the decision is deliberate
// rather than accidental. Everything below stays inside one t.TempDir().
func TestTeardown_FollowsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	fake := &exec.FakeRunner{}

	realParent := filepath.Join(root, "realparent")
	victim := filepath.Join(realParent, "forgectl-workflow-x")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	canary := filepath.Join(realParent, "canary")
	if err := os.Mkdir(canary, 0o700); err != nil {
		t.Fatalf("mkdir canary: %v", err)
	}
	link := filepath.Join(root, "plink")
	if err := os.Symlink(realParent, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := Teardown(context.Background(), fake, filepath.Join(link, "forgectl-workflow-x")); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("accepted behaviour changed: Teardown no longer follows a symlinked parent, stat err = %v", err)
	}
	// Only the named component goes: the link and its siblings survive.
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the parent symlink itself must survive: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Errorf("sibling of the target must survive: %v", err)
	}
}

// TestWithinWorkspace_RejectsSymlinkEscape covers the glob-via-symlink vector:
// a target reached through a symlink pointing outside workspace must be
// refused even though the literal path has no "..".
func TestWithinWorkspace_RejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()

	victim := filepath.Join(external, "victim.md")
	if err := os.WriteFile(victim, []byte("must survive"), 0o644); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}

	link := filepath.Join(workspace, "sub")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	target := filepath.Join(link, "victim.md")
	if WithinWorkspace(workspace, target) {
		t.Error("expected WithinWorkspace to reject a target reached via a symlink escaping the workspace")
	}

	kept := filepath.Join(workspace, "kept.md")
	if err := os.WriteFile(kept, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("WriteFile kept: %v", err)
	}
	if !WithinWorkspace(workspace, kept) {
		t.Error("expected WithinWorkspace to accept a target actually inside the workspace")
	}
}
