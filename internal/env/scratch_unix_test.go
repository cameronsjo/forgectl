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
//   [x] An rmdir failing with anything but ENOENT (EIO here) also gets the
//       .gitignore put back, by both teardowns, and the error says so (#768)
//   [x] A restore that fails is reported as failed, never as a .gitignore
//       left in place (#768)

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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
	prev, prevAt := scratchIgnoreGone, scratchIgnoreGoneAt
	scratchIgnoreGone = func(dir string) {
		if err := WriteFileExclusive(filepath.Join(dir, "late"), nil); err != nil {
			t.Errorf("plant the late entry: %v", err)
		}
	}
	scratchIgnoreGoneAt = func(sub *dirPin) {
		if err := createAt(sub.fd, "late"); err != nil {
			t.Errorf("plant the late entry: %v", err)
		}
	}
	t.Cleanup(func() { scratchIgnoreGone, scratchIgnoreGoneAt = prev, prevAt })
}

// createAt creates an empty file name in the directory dirfd names: the
// descriptor teardown's way to plant a late entry.
func createAt(dirfd int, name string) error {
	fd, err := openatCreate(dirfd, name, unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// failRmdir makes both teardowns' rmdir fail with EIO, leaving the directory
// in place, after running before (which may be nil).
func failRmdir(t *testing.T, before func(dir string)) {
	t.Helper()
	prev, prevAt := rmdirScratch, rmdirScratchAt
	rmdirScratch = func(dir string) error {
		if before != nil {
			before(dir)
		}
		return &os.PathError{Op: "remove", Path: dir, Err: unix.EIO}
	}
	rmdirScratchAt = func(_ *dirPin, name string) error {
		if before != nil {
			before(name)
		}
		return unix.EIO
	}
	t.Cleanup(func() { rmdirScratch, rmdirScratchAt = prev, prevAt })
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

// assertEIOReported checks the teardown returned the rmdir's own error, and
// told the truth about the .gitignore: that it was put back.
func assertEIOReported(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("err = %v, want the rmdir's EIO on the chain", err)
	}
	if errors.Is(err, errScratchNotEmpty) || !strings.Contains(err.Error(), "was put back") {
		t.Errorf("err = %q, want it to say the .gitignore was put back and not blame a non-empty directory", err)
	}
}

func assertIgnoreBack(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Clean(dir), ScratchIgnoreName))
	if err != nil || string(got) != ScratchIgnore {
		t.Errorf("the .gitignore was not put back after a failed rmdir: %q, %v", got, err)
	}
}

func TestRemoveScratchDirRestoresTheIgnoreOnAnyRmdirError(t *testing.T) {
	dir, err := MakeScratchDir(t.TempDir(), "scratch-")
	if err != nil {
		t.Fatalf("MakeScratchDir: %v", err)
	}
	failRmdir(t, nil)
	assertEIOReported(t, RemoveScratchDir(dir))
	assertIgnoreBack(t, dir)
}

func TestRemoveScratchDirAtRestoresTheIgnoreOnAnyRmdirError(t *testing.T) {
	dir := t.TempDir()
	pin := pinnedTarget(t, dir, ".env").dir
	sub, name, err := pin.mkScratchDir(".forgectl-env-x-")
	if err != nil {
		t.Fatalf("mkScratchDir: %v", err)
	}
	failRmdir(t, nil)
	assertEIOReported(t, pin.removeScratchDir(sub, name))
	assertIgnoreBack(t, filepath.Join(dir, name))
}

// A .gitignore someone else put there makes the O_EXCL restore fail. The
// teardown must say the restore failed rather than claim the file it wrote
// is in place, and must leave the planted file alone.
func TestRemoveScratchDirReportsAFailedRestore(t *testing.T) {
	dir, err := MakeScratchDir(t.TempDir(), "scratch-")
	if err != nil {
		t.Fatalf("MakeScratchDir: %v", err)
	}
	failRmdir(t, func(string) {
		if err := os.WriteFile(filepath.Join(dir, ScratchIgnoreName), []byte(plantedIgnore), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	err = RemoveScratchDir(dir)
	if !errors.Is(err, unix.EIO) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want both the rmdir's EIO and the restore's EEXIST on the chain", err)
	}
	if !strings.Contains(err.Error(), "putting its .gitignore back also failed") {
		t.Errorf("err = %q, want it to say the restore failed", err)
	}
	assertPlantedIgnoreKept(t, filepath.Join(dir, ScratchIgnoreName))
}

// The not-empty case with a failed restore must not return errScratchNotEmpty,
// whose text says the .gitignore was left in place.
func TestRemoveScratchDirAtReportsAFailedRestoreAfterALateEntry(t *testing.T) {
	dir := t.TempDir()
	pin := pinnedTarget(t, dir, ".env").dir
	sub, name, err := pin.mkScratchDir(".forgectl-env-x-")
	if err != nil {
		t.Fatalf("mkScratchDir: %v", err)
	}
	prevAt := scratchIgnoreGoneAt
	scratchIgnoreGoneAt = func(sub *dirPin) {
		if err := createAt(sub.fd, "late"); err != nil {
			t.Errorf("plant the late entry: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, ScratchIgnoreName), []byte(plantedIgnore), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { scratchIgnoreGoneAt = prevAt })
	err = pin.removeScratchDir(sub, name)
	if err == nil || errors.Is(err, errScratchNotEmpty) {
		t.Fatalf("err = %v, want a failed-restore error, not errScratchNotEmpty", err)
	}
	if !strings.Contains(err.Error(), "putting its .gitignore back failed") {
		t.Errorf("err = %q, want it to say the restore failed", err)
	}
	assertPlantedIgnoreKept(t, filepath.Join(dir, name, ScratchIgnoreName))
}
