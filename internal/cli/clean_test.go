package cli

// Test plan for clean.go
//
// newCleanCmd / newCleanCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: a dry run (no --apply) reports the reclaimable total and
//       leaves the fixture untouched
//   [x] Happy: `--root` overrides the client's default root
//   [x] Happy: an invalid `--type` value is rejected before any scan runs
//   [x] Happy: the `cln` alias resolves to the clean command
//   [x] Happy: --apply is never exercised here (it drives a huh confirmation
//       prompt requiring a tty) — that path is covered end-to-end at the
//       ops layer in internal/clean/clean_test.go, mirroring branch's split
//       (no CLI-level apply+confirm test either)

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestCleanCmd_DryRun_ReportsReclaimableAndTouchesNothing(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	leaf := filepath.Join(nm, "leaf.js")
	if err := os.MkdirAll(filepath.Dir(leaf), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(leaf, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(leaf); err != nil {
		t.Errorf("fixture must survive a dry run, stat error: %v", err)
	}
	if got := stdout.String(); got == "" {
		t.Error("expected non-empty dry-run report on stdout")
	}
}

func TestCleanCmd_RootFlag_OverridesDefault(t *testing.T) {
	explicitRoot := t.TempDir()
	nm := filepath.Join(explicitRoot, "proj", "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nm, "leaf.js"), make([]byte, 42), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The client's own default root is a DIFFERENT (empty) temp dir — only
	// --root should be scanned.
	defaultRoot := t.TempDir()
	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(defaultRoot))
	cmd := newCleanCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--root", explicitRoot})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stdout.String(); got == "" || got == "no reclaimable directories found\n\nnothing to reclaim\n" {
		t.Errorf("expected the explicit --root's node_modules to be reported, got: %q", got)
	}
}

func TestCleanCmd_InvalidType_RejectedBeforeScan(t *testing.T) {
	root := t.TempDir()
	client := cleanpkg.New(&exec.FakeRunner{}, cleanpkg.WithRoot(root))
	cmd := newCleanCmdForClient(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--type", "rust"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for an unknown --type value")
	}
}

func TestCleanCmd_AliasResolvesToCanonicalVerb(t *testing.T) {
	client := cleanpkg.New(&exec.FakeRunner{})
	cmd := newCleanCmdForClient(client)

	found := false
	for _, a := range cmd.Aliases {
		if a == "cln" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected \"cln\" among clean's aliases, got %v", cmd.Aliases)
	}
}
