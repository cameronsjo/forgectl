//go:build unix

package sops

// newWorkDir's nonce failure (#768): the one error path between the work
// directory existing and newWorkDir returning it.
//
//   [x] A failed nonce read returns the fixed error and removes the work
//       directory it had just made, .gitignore included
//   [x] A teardown that cannot remove the work directory says so in the log,
//       on the nonce path and in cleanup, rather than dropping the error

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
)

func TestNewWorkDirRemovesTheDirectoryWhenTheNonceFails(t *testing.T) {
	dir, _ := gitRepo(t)
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.ResolveTarget("secrets.sops.yaml", dir)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()

	prev := readNonce
	readNonce = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { readNonce = prev })

	w, err := newWorkDir(target)
	if err == nil || w != nil {
		t.Fatalf("newWorkDir = %v, %v; want the nonce failure", w, err)
	}
	if err.Error() != "could not generate a nonce" {
		t.Errorf("err = %q, want the fixed nonce error", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		names := make([]string, 0, len(after))
		for _, e := range after {
			names = append(names, e.Name())
		}
		t.Errorf("newWorkDir left an entry behind after the nonce failed: %v", names)
	}
}

func captureWarnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

const teardownWarning = "Failed to remove the sops work directory."

// On the nonce path, something that arrived in the new work directory makes
// its removal fail; the failure is logged, not dropped.
func TestNewWorkDirLogsAFailedTeardown(t *testing.T) {
	dir, _ := gitRepo(t)
	target, err := env.ResolveTarget("secrets.sops.yaml", dir)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()
	logBuf := captureWarnLog(t)

	prev := readNonce
	readNonce = func([]byte) (int, error) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", env.ScratchIgnoreName))
		for _, m := range matches {
			if err := os.WriteFile(filepath.Join(filepath.Dir(m), "late"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return 0, errors.New("entropy unavailable")
	}
	t.Cleanup(func() { readNonce = prev })

	if _, err := newWorkDir(target); err == nil {
		t.Fatal("want the nonce failure")
	}
	if !strings.Contains(logBuf.String(), teardownWarning) {
		t.Errorf("the failed teardown was not logged: %q", logBuf.String())
	}
}

// In cleanup, an entry whose removal fails keeps the directory; the
// teardown's refusal is logged, not dropped.
func TestWorkDirCleanupLogsAFailedTeardown(t *testing.T) {
	dir, _ := gitRepo(t)
	target, err := env.ResolveTarget("secrets.sops.yaml", dir)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()
	w, err := newWorkDir(target)
	if err != nil {
		t.Fatalf("newWorkDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "value"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	logBuf := captureWarnLog(t)
	prev := removeWorkDirEntry
	removeWorkDirEntry = func(string) error { return errors.New("EIO") }
	t.Cleanup(func() { removeWorkDirEntry = prev })

	w.cleanup()
	if !strings.Contains(logBuf.String(), teardownWarning) {
		t.Errorf("the failed teardown was not logged: %q", logBuf.String())
	}
}
