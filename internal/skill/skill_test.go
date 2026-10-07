// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package skill

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestText_IsTheSkillEntryNamedForgectl(t *testing.T) {
	text := Text()
	if !strings.HasPrefix(text, "---\nname: forgectl\n") {
		t.Errorf("frontmatter name must be forgectl, got start %q", text[:40])
	}
	if strings.Contains(text, "cadence-forge") {
		t.Error("the skill must not point into a plugin that no longer carries it")
	}
}

func TestInstall_WritesSkillAndReferences(t *testing.T) {
	dir := t.TempDir()

	written, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, want := range []string{"SKILL.md", "references/desk.md", "references/board-tasks.md"} {
		if !slices.Contains(written, want) {
			t.Errorf("Install did not write %s; wrote %v", want, written)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "SKILL.md")) //nolint:gosec // G304: a path the test just built
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != Text() {
		t.Error("installed SKILL.md differs from the embedded text")
	}
}

func TestInstall_RefusesRelativeAndMissingDirs(t *testing.T) {
	if _, err := Install("relative/dir"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative dir: want an 'absolute' error, got %v", err)
	}

	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Install(missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing dir: want a 'does not exist' error, got %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("a refused install must not create the directory")
	}

	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file path: want a 'not a directory' error, got %v", err)
	}
}

func TestInstall_LeavesUnrelatedFilesAlone(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(keep) //nolint:gosec // G304: a path the test just built
	if err != nil || string(got) != "mine" {
		t.Errorf("unrelated file changed: %q, %v", got, err)
	}
}

func TestInstall_SymlinkedDirCannotCarryAWriteOutsideDir(t *testing.T) {
	outside := t.TempDir()
	dir := t.TempDir()
	// references/ is a link out of dir; the install must not write through it.
	if err := os.Symlink(outside, filepath.Join(dir, "references")); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(dir); err == nil {
		t.Error("want an error when references/ links outside the directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a file landed outside the install directory: %v", entries)
	}
}

func TestInstall_SymlinkedFileCannotCarryAWriteOutsideDir(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	// The install replaces the link itself; the file behind it is never opened.
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside) //nolint:gosec // G304: a path the test just built
	if err != nil || string(got) != "untouched" {
		t.Errorf("the file behind the link was written: %q, %v", got, err)
	}
}

func TestInstall_HardlinkIsReplacedNotWrittenThrough(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Link(outside, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside) //nolint:gosec // G304: a path the test just built
	if err != nil || string(got) != "untouched" {
		t.Errorf("the hardlinked file was written through: %q, %v", got, err)
	}
	if installed, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(installed) != Text() { //nolint:gosec // G304: test path
		t.Error("SKILL.md was not installed")
	}
}
