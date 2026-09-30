package sops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
)

// hostileDir carries a bidi override and a C1 CSI, as \u escapes so no
// literal format character sits in source.
const hostileDir = "ev\u202eil\u009b31m"

// hostileTarget resolves a SOPS-named file under a directory named
// hostileDir, inside a throwaway repository. Nothing here runs sops.
func hostileTarget(t *testing.T) env.Target {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	if err := os.Mkdir(filepath.Join(repo, hostileDir), 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	target, err := env.ResolveTarget(filepath.Join(hostileDir, "secrets.sops.yaml"), repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	t.Cleanup(target.Close)
	return target
}

// TestDriverErrorsQuoteTheTargetPath pins forgectl#855: the driver's refusals
// name the target by its repository-relative path, which is whatever the
// directories in the repository are called, so they quote it with every
// control and format rune escaped rather than printing it raw.
//
// Mutation that turns it red: print target.Rel() raw in sopsRefusalError or
// in restoreFailed.
func TestDriverErrorsQuoteTheTargetPath(t *testing.T) {
	target := hostileTarget(t)
	restoreErr := &os.PathError{Op: "rename", Path: target.Abs(), Err: os.ErrPermission}
	for name, err := range map[string]error{
		"refusal":              sopsRefusalError(target),
		"restore, no backup":   restoreFailed(errors.New("cause"), restoreErr, target, ""),
		"restore, backup kept": restoreFailed(errors.New("cause"), restoreErr, target, filepath.Join(filepath.Dir(target.Abs()), "kept")),
	} {
		got := err.Error()
		if strings.ContainsAny(got, "\u202e\u009b") {
			t.Errorf("%s: error carries a raw bidi/control rune: %q", name, got)
		}
		if !strings.Contains(got, `"ev\u202eil\u009b31m`) {
			t.Errorf("%s: error = %q, want the escaped, quoted target path", name, got)
		}
	}
}
