// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package exec

import (
	"errors"
	"go/types"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNewLaunchScript_Lifecycle pins the script's shape: a 0700 directory, a
// 0600 file holding the self-removal line and then `exec <line>`, Consumed
// false until the file is gone, and Remove taking the directory with it,
// safe to repeat.
//
// Mutations that turn it red: MkdirTemp then Chmod 0o755; drop the prefix's
// rm line; make Remove leave the directory.
func TestNewLaunchScript_Lifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and /bin/sh")
	}
	base := t.TempDir()
	s, err := NewLaunchScript(base, Opaque("'/bin/echo' 'hi'"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s.Path()) != s.Dir() || filepath.Dir(s.Dir()) != base {
		t.Fatalf("script %s is not in a directory of its own under %s", s.Path(), base)
	}
	dirInfo, err := os.Lstat(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 0700", perm)
	}
	fileInfo, err := os.Lstat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm&^0o600 != 0 {
		t.Errorf("file mode = %o, want at most 0600", perm)
	}
	body, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if want := "/bin/rm -f -- \"$0\"\nexec '/bin/echo' 'hi'\n"; string(body) != want {
		t.Errorf("script = %q, want %q", body, want)
	}
	if done, err := s.Consumed(); err != nil || done {
		t.Fatalf("Consumed = %v, %v before anything read it", done, err)
	}
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	if done, err := s.Consumed(); err != nil || !done {
		t.Fatalf("Consumed = %v, %v after the file went", done, err)
	}
	for range 2 {
		if err := s.Remove(); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}
	if _, err := os.Lstat(s.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the private directory is still there: %v", err)
	}
}

// TestNewLaunchScript_RunsItsLineAndRemovesItself runs the script the way a
// manager does, `/bin/sh <path>`: the line runs, and the file is gone by the
// time it does.
//
// Mutation that turns it red: drop the rm line from launchScriptPrefix.
func TestNewLaunchScript_RunsItsLineAndRemovesItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	s, err := NewLaunchScript(t.TempDir(), Opaque("/bin/sh -c 'test -e \"$1\" || echo gone > \"$2\"' sh \"$0\" '"+marker+"'"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Remove() }()
	out, err := osexec.CommandContext(t.Context(), "/bin/sh", s.Path()).CombinedOutput() //nolint:gosec // G204: the script this test wrote
	if err != nil {
		t.Fatalf("sh %s: %v: %s", s.Path(), err, out)
	}
	got, err := os.ReadFile(marker) //nolint:gosec // G304: a path under t.TempDir
	if err != nil || strings.TrimSpace(string(got)) != "gone" {
		t.Fatalf("the line did not run with its script already removed: %q, %v", got, err)
	}
	if done, err := s.Consumed(); err != nil || !done {
		t.Fatalf("Consumed = %v, %v after sh ran it", done, err)
	}
}

// TestNewLaunchScript_Refuses: a plain argument (its value would be
// fixed text, not a sealed payload), an empty one, a line break, and a
// relative base all refuse, and leave nothing under the base.
func TestNewLaunchScript_Refuses(t *testing.T) {
	base := t.TempDir()
	for name, line := range map[string]Arg{
		"fixed":   MustFixed("echo"),
		"empty":   Opaque(""),
		"newline": Opaque("a\nb"),
		"unset":   {},
	} {
		if _, err := NewLaunchScript(base, line); !errors.Is(err, ErrLaunchScript) {
			t.Errorf("%s: err = %v, want ErrLaunchScript", name, err)
		}
	}
	if _, err := NewLaunchScript("relative", Opaque("x")); !errors.Is(err, ErrLaunchScript) {
		t.Errorf("relative base: err = %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused script left %d entries under the base", len(entries))
	}
}

// TestSealedWriteNewFileHasOneCaller pins, on every guard platform, that
// sealed.WriteNewFile, the second reveal, is named only inside
// NewLaunchScript, and there once, as a direct call: a second writer of
// sealed payloads cannot appear without review.
//
// Mutation that turns it red: `var _ = sealed.WriteNewFile` in any
// production file of internal/exec.
func TestSealedWriteNewFileHasOneCaller(t *testing.T) {
	for _, p := range guardConfigs {
		c := checkExec(t, p)
		write, ok := c.sealed.Scope().Lookup("WriteNewFile").(*types.Func)
		if !ok {
			t.Fatalf("[%s] sealed declares no WriteNewFile func; the rule would check nothing", p)
		}
		for _, f := range namedOutside(c, write, "", "NewLaunchScript") {
			t.Errorf("[%s] %s: sealed.WriteNewFile is named outside NewLaunchScript", p, f)
		}
		for _, f := range callShapeFindings(c, write, "", "NewLaunchScript") {
			t.Errorf("[%s] %s", p, f)
		}
	}
}
