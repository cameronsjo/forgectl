package quarantine

// Quarantine's own error echoes (#810). A target comes from config and a move
// path from a fetched clone, so every error that names one quotes it with
// controls escaped and caps it:
//
//   [x] a rejected target rule is quoted and capped
//   [x] Restore's stat failure names the move path quoted, not raw, and keeps
//       the PathError on the chain
//   [x] resolveRootIdentity's wrapped root errors are escaped

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Mutation: make quote return s unchanged and the raw control and the full
// tail both reach the error.
func TestNormalizeTargetRule_QuotesAndCapsTheTarget(t *testing.T) {
	// pathEchoMaxRunes is 256, so the tail must outrun it by a margin.
	tail := strings.Repeat("t", 600)
	_, err := normalizeTargetRule("/" + hostile + tail)
	if err == nil {
		t.Fatal("normalizeTargetRule(absolute) = nil, want a refusal")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control: %q", err)
	}
	if strings.Contains(err.Error(), tail[:300]) || !strings.Contains(err.Error(), "…") {
		t.Errorf("the error echoes the target uncapped: %q", err)
	}
	if !strings.Contains(err.Error(), "must not be absolute") {
		t.Errorf("err = %q, want the absolute-target refusal", err)
	}
}

// Mutation: restore the raw %s for m.To in Restore's stat error and the
// control reaches the error text; drop termsafe.Error and the PathError's own
// copy of the path does.
func TestRestore_StatFailureQuotesTheMovePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent is a regular file, so Lstat fails with ENOTDIR, which is
	// not IsNotExist: Restore reports it rather than skipping the move.
	to := filepath.Join(blocker, "evil"+hostile, "CLAUDE.md.quarantined")
	err := New(&exec.FakeRunner{}).Restore(context.Background(), []Move{{From: filepath.Join(dir, "CLAUDE.md"), To: to}})
	if err == nil {
		t.Fatal("Restore under a regular file = nil, want an error")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control from the move path: %q", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("the PathError fell off the chain: %v", err)
	}
}

// resolveRootIdentity's errors wrap the root's own PathError, whose path is
// the workspace. They go through termsafe.Error like the rest of the file.
//
// Mutation: drop termsafe.Error from the "resolve quarantine root" wrap and
// the raw control reaches the error.
func TestResolveRootIdentity_EscapesTheRootPath(t *testing.T) {
	_, err := resolveRootIdentity(filepath.Join(t.TempDir(), "gone"+hostile))
	if err == nil {
		t.Fatal("resolveRootIdentity(missing root) = nil, want an error")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control from the root path: %q", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the not-exist cause fell off the chain: %v", err)
	}
}
