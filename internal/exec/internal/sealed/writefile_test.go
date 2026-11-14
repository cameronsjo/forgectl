// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package sealed

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWriteNewFile_WritesExactlyThePayloadBetweenTheFixedText pins the one
// thing WriteNewFile may write, and the mode it writes it with.
//
// Mutation that turns it red: write prefix+suffix only, or open with 0o644.
func TestWriteNewFile_WritesExactlyThePayloadBetweenTheFixedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := WriteNewFile(path, "pre:", New("'/bin/x' 'a b'"), ":post\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: a path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pre:'/bin/x' 'a b':post\n" {
		t.Fatalf("file = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&^0o600 != 0 {
			t.Fatalf("mode = %o, want at most 0600", perm)
		}
	}
}

// TestWriteNewFile_RefusesWhatItMustNotWrite: an existing file or symlink at
// the path is never written through, and a payload that is empty or would
// break out of its one line is refused, leaving no file.
//
// Mutations that turn it red: drop O_EXCL; drop the line-break check.
func TestWriteNewFile_RefusesWhatItMustNotWrite(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteNewFile(existing, "", New("x"), ""); err == nil {
		t.Error("an existing file was written over")
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" { //nolint:gosec // G304: a path under t.TempDir
		t.Errorf("the existing file now holds %q", got)
	}
	if runtime.GOOS != "windows" {
		target := filepath.Join(dir, "target")
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := WriteNewFile(link, "", New("x"), ""); err == nil {
			t.Error("a symlink at the path was written through")
		}
		if _, err := os.Stat(target); err == nil {
			t.Error("the symlink's target was created")
		}
	}
	for name, v := range map[string]Value{
		"zero":     {},
		"empty":    New(""),
		"newline":  New("a\nrm -rf ~"),
		"carriage": New("a\rb"),
		"nul":      New("a\x00b"),
	} {
		path := filepath.Join(dir, "refused-"+name)
		if err := WriteNewFile(path, "", v, ""); err == nil {
			t.Errorf("%s payload was written", name)
		}
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s payload left a file behind", name)
		}
	}
}
