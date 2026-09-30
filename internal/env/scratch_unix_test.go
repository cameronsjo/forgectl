//go:build unix

package env

// The scratch directory's creation and teardown guards, driven through the
// test seams in scratch.go at the points only a race could reach (the #750
// review):
//
//   [x] A directory that is not empty once opened is abandoned, and what was
//       planted in it is left alone (mkScratchDir)
//   [x] A .gitignore that is already there when the create runs (EEXIST) is
//       never unlinked, by mkScratchDir or by MakeScratchDir
//   [x] A file arriving between the teardown's listing and its rmdir gets
//       the .gitignore put back, by both teardowns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const plantedIgnore = "a .gitignore this process did not write\n"

func setSeam(t *testing.T, seam *func(string), fn func(string)) {
	t.Helper()
	prev := *seam
	*seam = fn
	t.Cleanup(func() { *seam = prev })
}

func TestMkScratchDirAbandonsADirectoryThatIsNotEmpty(t *testing.T) {
	dir := t.TempDir()
	setSeam(t, &scratchDirMade, func(name string) { plant(t, filepath.Join(dir, name, "planted")) })
	pin := pinnedTarget(t, dir, ".env").dir
	if _, _, err := pin.mkScratchDir(".forgectl-env-x-"); err == nil {
		t.Fatal("mkScratchDir accepted a directory that already held an entry")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".forgectl-env-x-*"))
	if len(matches) != 1 {
		t.Fatalf("want the abandoned directory left in place, found %v", matches)
	}
	assertStillThere(t, filepath.Join(matches[0], "planted"))
	if _, err := os.Lstat(filepath.Join(matches[0], ScratchIgnoreName)); !os.IsNotExist(err) {
		t.Errorf("mkScratchDir wrote a .gitignore into a directory it abandoned: %v", err)
	}
}

func assertPlantedIgnoreKept(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Clean(path))
	if err != nil || string(got) != plantedIgnore {
		t.Errorf("the .gitignore this process did not create was removed or changed: %q, %v", got, err)
	}
}

func TestMkScratchDirNeverUnlinksAnIgnoreItDidNotCreate(t *testing.T) {
	dir := t.TempDir()
	setSeam(t, &scratchIgnoreCreating, func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name, ScratchIgnoreName), []byte(plantedIgnore), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	pin := pinnedTarget(t, dir, ".env").dir
	if _, _, err := pin.mkScratchDir(".forgectl-env-x-"); err == nil {
		t.Fatal("mkScratchDir succeeded over an existing .gitignore")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".forgectl-env-x-*"))
	if len(matches) != 1 {
		t.Fatalf("want the directory left in place, found %v", matches)
	}
	assertPlantedIgnoreKept(t, filepath.Join(matches[0], ScratchIgnoreName))
}

func TestMakeScratchDirNeverUnlinksAnIgnoreItDidNotCreate(t *testing.T) {
	parent := t.TempDir()
	setSeam(t, &scratchIgnoreCreating, func(name string) {
		if err := os.WriteFile(filepath.Join(parent, name, ScratchIgnoreName), []byte(plantedIgnore), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := MakeScratchDir(parent, "scratch-"); err == nil {
		t.Fatal("MakeScratchDir succeeded over an existing .gitignore")
	}
	matches, _ := filepath.Glob(filepath.Join(parent, "scratch-*"))
	if len(matches) != 1 {
		t.Fatalf("want the directory left in place, found %v", matches)
	}
	assertPlantedIgnoreKept(t, filepath.Join(matches[0], ScratchIgnoreName))
}

func setGoneSeam(t *testing.T) {
	t.Helper()
	prev := scratchIgnoreGone
	scratchIgnoreGone = func(plant func(string) error) {
		if err := plant("late"); err != nil {
			t.Errorf("plant the late entry: %v", err)
		}
	}
	t.Cleanup(func() { scratchIgnoreGone = prev })
}

func assertIgnoreRestored(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Clean(dir), ScratchIgnoreName))
	if err != nil || string(got) != ScratchIgnore {
		t.Errorf("the .gitignore was not put back beside the late entry: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "late")); err != nil {
		t.Errorf("the late entry is gone: %v", err)
	}
}

func TestRemoveScratchDirRestoresTheIgnoreAfterALateEntry(t *testing.T) {
	dir, err := MakeScratchDir(t.TempDir(), "scratch-")
	if err != nil {
		t.Fatalf("MakeScratchDir: %v", err)
	}
	setGoneSeam(t)
	if err := RemoveScratchDir(dir); err == nil {
		t.Error("RemoveScratchDir reported success although an entry arrived")
	}
	assertIgnoreRestored(t, dir)
}

func TestRemoveScratchDirAtRestoresTheIgnoreAfterALateEntry(t *testing.T) {
	dir := t.TempDir()
	pin := pinnedTarget(t, dir, ".env").dir
	sub, name, err := pin.mkScratchDir(".forgectl-env-x-")
	if err != nil {
		t.Fatalf("mkScratchDir: %v", err)
	}
	setGoneSeam(t)
	if err := pin.removeScratchDir(sub, name); err == nil {
		t.Error("removeScratchDir reported success although an entry arrived")
	}
	if !strings.HasPrefix(name, ".forgectl-env-x-") {
		t.Fatalf("unexpected name %q", name)
	}
	assertIgnoreRestored(t, filepath.Join(dir, name))
}
