package cli

// Test plan for pr_findings.go
//
// newPrFindingsListCmd (Classification: API handler / cobra command)
//   [x] Happy: prints one line per findings dir, each carrying its path
//   [x] Happy: an empty/absent findings dir prints "no findings"
//
// newPrFindingsCleanupCmd (Classification: API handler / cobra command,
// dry-run-by-default over a destructive client op)
//   [x] Happy: dry-run (no --apply) reports the reclaimable dir and deletes
//       nothing
//   [x] Happy: nothing reclaimable short-circuits before any confirmation
//       gate, whether or not --apply is passed (no huh prompt reachable in
//       a non-tty test — mirrors clean_test.go's precedent of not exercising
//       the CLI-level apply+confirm path directly)
//   [x] Unhappy: a negative --older-than errors before any scan runs (a typo
//       like -24h would push the cutoff into the future and make everything
//       look reclaimable)
//   [x] Happy: --older-than 0 passes validation (0 is the explicit "reclaim
//       everything" cutoff, still gated by --apply/confirm like any other)

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/pr"
)

func TestPrFindingsListCmd_PrintsPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "forgectl-findings-aaa"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	client := pr.New(nil, pr.WithFindingsDir(dir))

	cmd := newPrFindingsCmd(client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("forgectl-findings-aaa")) {
		t.Errorf("output missing the findings dir path; got:\n%s", out.String())
	}
}

func TestPrFindingsListCmd_NoFindings(t *testing.T) {
	client := pr.New(nil, pr.WithFindingsDir(filepath.Join(t.TempDir(), "absent")))

	cmd := newPrFindingsCmd(client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := out.String(); got != "no findings\n" {
		t.Errorf("output = %q, want %q", got, "no findings\n")
	}
}

func TestPrFindingsCleanupCmd_DryRun_ReportsAndDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "forgectl-findings-old")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	client := pr.New(nil, pr.WithFindingsDir(dir))

	cmd := newPrFindingsCmd(client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cleanup", "--older-than", "24h"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := out.String()
	for _, want := range []string{oldDir, "re-run with --apply"} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, body)
		}
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("dry-run deleted %q, want it left alone: %v", oldDir, err)
	}
}

func TestPrFindingsCleanupCmd_NegativeOlderThan_ErrorsWithoutScanning(t *testing.T) {
	// oldDir exists solely to prove a negative --older-than never gets the
	// chance to treat it as reclaimable: if validateFindingsOlderThan didn't
	// run before the scan, this dir would show up in a "reclaimable" report
	// instead of the command erroring out. Mirrors clean_test.go's
	// TestCleanCmd_InvalidType_RejectedBeforeScan precedent of asserting
	// only the error, not stdout — cobra's own usage-on-error text lands on
	// OutOrStdout() too, so asserting stdout is empty would be asserting
	// cobra's behavior, not this command's.
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "forgectl-findings-old")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	client := pr.New(nil, pr.WithFindingsDir(dir))

	cmd := newPrFindingsCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"cleanup", "--older-than=-24h"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a negative --older-than, got nil")
	}
	if !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("error = %q, want it to mention the negative-duration rejection", err.Error())
	}
	if strings.Contains(stdout.String(), oldDir) {
		t.Errorf("stdout contains %q — the scan ran despite the negative --older-than; got:\n%s", oldDir, stdout.String())
	}
}

func TestPrFindingsCleanupCmd_ZeroOlderThan_PassesValidation(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "forgectl-findings-old")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	client := pr.New(nil, pr.WithFindingsDir(dir))

	cmd := newPrFindingsCmd(client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cleanup", "--older-than=0"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), oldDir) {
		t.Errorf("output missing %q; --older-than 0 should treat everything as reclaimable; got:\n%s", oldDir, out.String())
	}
}

func TestPrFindingsCleanupCmd_NothingToReclaim_ShortCircuitsBeforeConfirm(t *testing.T) {
	// Empty findings dir: preview is empty for both apply=false and
	// apply=true, so runPrFindingsCleanup must return via the "nothing to
	// reclaim" branch before ever reaching the confirm() gate — the only way
	// this test can pass --apply without a tty/huh stub.
	dir := t.TempDir()
	client := pr.New(nil, pr.WithFindingsDir(dir))

	cmd := newPrFindingsCmd(client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cleanup", "--apply"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := out.String(); got != "nothing to reclaim\n" {
		t.Errorf("output = %q, want %q", got, "nothing to reclaim\n")
	}
}
