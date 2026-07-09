// Test plan for sandbox.go
//
//   - Sandbox local-repo path issues `git -C <repo> worktree add -- <dir> <ref>`
//     (assert FakeRunner.Calls argv, including the `--` option-terminator).
//   - Sandbox alwaysClone/remote issues `git clone --branch <ref> -- <repo> <dir>`;
//     clone-without-ref omits --branch.
//   - RejectOptionLike rejects a leading-'-' repo and ref before any Runner call.
//   - Teardown is idempotent: an empty workspace is a no-op and issues no
//     Runner call.
//   - WithinWorkspace rejects a symlink escaping the workspace.
package sandbox

import (
	"context"
	"os"
	"path/filepath"
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

// TestTeardown_Idempotent verifies Teardown is safe to call on an empty
// workspace (no-op) and issues no Runner call.
func TestTeardown_Idempotent(t *testing.T) {
	fake := &exec.FakeRunner{}
	if err := Teardown(context.Background(), fake, ""); err != nil {
		t.Fatalf("Teardown on empty workspace: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("Teardown must not shell out, got calls: %+v", fake.Calls)
	}

	workspace := t.TempDir()
	if err := Teardown(context.Background(), fake, workspace); err != nil {
		t.Fatalf("first Teardown: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace should be gone after Teardown, stat err = %v", err)
	}
	// Second call on the already-removed dir must not error.
	if err := Teardown(context.Background(), fake, workspace); err != nil {
		t.Fatalf("second (idempotent) Teardown must not error, got: %v", err)
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
