package env

// WriteTargetTracked's ScratchTracker contract (cameronsjo/forgectl#751).
//
//   [x] The scratch directory does not exist until the tracker calls mkdir,
//       and mkdir returns its absolute path beside the target; the write then
//       completes and removes it
//   [x] A tracker error abandons the write and removes a directory mkdir made
//   [x] A tracker that returns no error without calling mkdir fails the write
//   [x] A second mkdir call fails and makes no second directory; the write
//       goes on with the first and removes it

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envScratchNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), envScratchPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestWriteTargetTrackedCreatesTheScratchInsideTheTracker(t *testing.T) {
	dir := t.TempDir()
	tg := pinnedTarget(t, dir, ".env")

	var before, after []string
	var made string
	err := WriteTargetTracked(tg, []byte("K=v\n"), func(mkdir func() (string, error)) error {
		before = envScratchNames(t, dir)
		var err error
		made, err = mkdir()
		after = envScratchNames(t, dir)
		return err
	})
	if err != nil {
		t.Fatalf("WriteTargetTracked: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("the scratch directory existed before the tracker called mkdir: %v", before)
	}
	if len(after) != 1 || made != filepath.Join(dir, after[0]) {
		t.Errorf("mkdir returned %q with %v on disk; want the one scratch directory's absolute path", made, after)
	}
	if left := envScratchNames(t, dir); len(left) != 0 {
		t.Errorf("a completed write left %v", left)
	}
}

func TestWriteTargetTrackedAbandonsOnTrackerError(t *testing.T) {
	dir := t.TempDir()
	tg := pinnedTarget(t, dir, ".env")
	stop := errors.New("stop")

	err := WriteTargetTracked(tg, []byte("K=v\n"), func(mkdir func() (string, error)) error {
		if _, err := mkdir(); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("WriteTargetTracked = %v, want the tracker's error", err)
	}
	if left := envScratchNames(t, dir); len(left) != 0 {
		t.Errorf("an abandoned write left %v", left)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, ".env")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("an abandoned write wrote the target (%v)", statErr)
	}

	err = WriteTargetTracked(tg, []byte("K=v\n"), func(func() (string, error)) error { return nil })
	if !errors.Is(err, errScratchNotMade) {
		t.Fatalf("a tracker that never called mkdir: WriteTargetTracked = %v, want errScratchNotMade", err)
	}
}

func TestWriteTargetTrackedRefusesASecondMkdir(t *testing.T) {
	dir := t.TempDir()
	tg := pinnedTarget(t, dir, ".env")

	var second error
	var during []string
	err := WriteTargetTracked(tg, []byte("K=v\n"), func(mkdir func() (string, error)) error {
		if _, err := mkdir(); err != nil {
			return err
		}
		_, second = mkdir()
		during = envScratchNames(t, dir)
		return nil
	})
	if err != nil {
		t.Fatalf("WriteTargetTracked: %v", err)
	}
	if !errors.Is(second, errScratchMadeTwice) {
		t.Errorf("the second mkdir = %v, want errScratchMadeTwice", second)
	}
	if len(during) != 1 {
		t.Errorf("after two mkdir calls the directory holds %v; want exactly one scratch directory", during)
	}
	if left := envScratchNames(t, dir); len(left) != 0 {
		t.Errorf("the write left %v", left)
	}
}
