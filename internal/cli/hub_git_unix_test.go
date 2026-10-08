//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitBranchWithin runs gitProjectBranch with a deadline, so a read that
// blocks (a FIFO opened without O_NONBLOCK) fails the test instead of hanging.
func gitBranchWithin(t *testing.T, dir string) (string, string, bool) {
	t.Helper()
	type res struct {
		p, b string
		ok   bool
	}
	done := make(chan res, 1)
	go func() {
		p, b, ok := gitProjectBranch(dir)
		done <- res{p, b, ok}
	}()
	select {
	case r := <-done:
		return r.p, r.b, r.ok
	case <-time.After(5 * time.Second):
		t.Fatalf("gitProjectBranch(%s) blocked", dir)
		return "", "", false
	}
}

// TestGitProjectBranch_HostileMarkers pins the review's N1–N4: a FIFO .git, a
// FIFO or symlinked HEAD, a symlinked .git dir, an oversized HEAD, and a HEAD
// naming an implausible ref all keep the project and drop the branch —
// without blocking and without following the link.
func TestGitProjectBranch_HostileMarkers(t *testing.T) {
	root := t.TempDir()
	mkfifo := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}
	mkfifo(filepath.Join(root, "fifomarker", ".git"))
	mkfifo(filepath.Join(root, "fifohead", ".git", "HEAD"))
	writeFile(t, filepath.Join(root, "real", ".git", "HEAD"), "ref: refs/heads/main\n")
	if err := os.MkdirAll(filepath.Join(root, "symhead", ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(root, "symhead", ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "symgit"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", ".git"), filepath.Join(root, "symgit", ".git")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "gitdirs", "fifo", "placeholder"), "")
	mkfifo(filepath.Join(root, "gitdirs", "fifo", "HEAD"))
	writeFile(t, filepath.Join(root, "linkedfifo", ".git"), "gitdir: "+filepath.Join(root, "gitdirs", "fifo")+"\n")
	// Over the 4 KiB cap, but a valid branch once trimmed: only the cap drops it.
	writeFile(t, filepath.Join(root, "huge", ".git", "HEAD"), "ref: refs/heads/main"+strings.Repeat(" ", gitMetaMaxBytes)+"\n")
	for name, head := range map[string]string{
		"badref-ctl":  "ref: refs/heads/a\x1b[31mb\n",
		"badref-dots": "ref: refs/heads/a..b\n",
		"badref-dash": "ref: refs/heads/-x\n",
		"badref-lock": "ref: refs/heads/x.lock\n",
		"badref-tag":  "ref: refs/tags/v1\n",
	} {
		writeFile(t, filepath.Join(root, name, ".git", "HEAD"), head)
	}

	for _, dir := range []string{"fifomarker", "fifohead", "symhead", "symgit", "linkedfifo", "huge",
		"badref-ctl", "badref-dots", "badref-dash", "badref-lock", "badref-tag"} {
		project, branch, ok := gitBranchWithin(t, filepath.Join(root, dir))
		if !ok || project != dir || branch != "" {
			t.Errorf("%s: gitProjectBranch = (%q, %q, %v), want (%q, \"\", true)", dir, project, branch, ok, dir)
		}
	}
	if _, branch, _ := gitBranchWithin(t, filepath.Join(root, "real")); branch != "main" {
		t.Errorf("control: real checkout branch = %q, want main", branch)
	}
}

// TestGitProjectBranch_ForeignOwnerDropsBranch pins git's safe.directory
// default: a .git another user owns keeps the project name, never its HEAD.
func TestGitProjectBranch_ForeignOwnerDropsBranch(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown a fixture to another user")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "theirs", ".git", "HEAD"), "ref: refs/heads/main\n")
	if err := os.Chown(filepath.Join(root, "theirs", ".git"), 4242, 4242); err != nil {
		t.Fatal(err)
	}
	if project, branch, ok := gitBranchWithin(t, filepath.Join(root, "theirs")); !ok || project != "theirs" || branch != "" {
		t.Errorf("gitProjectBranch = (%q, %q, %v), want (theirs, \"\", true)", project, branch, ok)
	}
}

// TestOpenGitMeta_NeitherBlocksNorFollows pins the open's own flags, apart
// from readSmallFile's Lstat: they are what hold when a path is swapped to a
// FIFO or a symlink between that Lstat and this open.
func TestOpenGitMeta_NeitherBlocksNorFollows(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "HEAD")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openGitMeta(fifo)
		if err == nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("openGitMeta blocked on a FIFO with no writer")
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openGitMeta(link); err == nil {
		_ = f.Close()
		t.Error("openGitMeta followed a symlink")
	}
}
