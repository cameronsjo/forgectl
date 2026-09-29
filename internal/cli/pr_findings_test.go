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
//
// Terminal safety (forgectl#551)
//   [x] A findings dir named with ESC and a newline reaches no output raw —
//       not the list rows, not the cleanup preview, not the "reclaimed" line
//       after --apply (driven through the confirmFn seam) — and each is shown
//       in its QuotePath-escaped form

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// mkStaleFindingsDir creates a findings dir whose owner marker names a
// session record that does not exist, the way a finished review leaves it.
// `pr findings cleanup` refuses a dir with no marker at all (forgectl#558).
func mkStaleFindingsDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, ".forgectl-owner"), []byte("local-gone000-1-1.json\n"), 0o600); err != nil {
		t.Fatalf("write owner marker: %v", err)
	}
}

func TestPrFindingsListCmd_PrintsPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "forgectl-findings-aaa"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	client := pr.New(nil, pr.WithFindingsDir(dir))

	cmd := newPrFindingsCmd(client, theme.Theme{})
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

	cmd := newPrFindingsCmd(client, theme.Theme{})
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
	mkStaleFindingsDir(t, oldDir)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	client := pr.New(nil, pr.WithFindingsDir(dir), pr.WithSessionsDir(t.TempDir()))

	cmd := newPrFindingsCmd(client, theme.Theme{})
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
	mkStaleFindingsDir(t, oldDir)
	client := pr.New(nil, pr.WithFindingsDir(dir), pr.WithSessionsDir(t.TempDir()))

	cmd := newPrFindingsCmd(client, theme.Theme{})
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
	mkStaleFindingsDir(t, oldDir)
	client := pr.New(nil, pr.WithFindingsDir(dir), pr.WithSessionsDir(t.TempDir()))

	cmd := newPrFindingsCmd(client, theme.Theme{})
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
	client := pr.New(nil, pr.WithFindingsDir(dir), pr.WithSessionsDir(t.TempDir()))

	cmd := newPrFindingsCmd(client, theme.Theme{})
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

// TestPrFindingsCmd_ControlCharacterDirNameNeverReachesOutputRaw pins
// forgectl#551: every findings path printed by `pr findings list` and
// `pr findings cleanup` (preview and --apply) is a directory name read off
// disk, so one planted with ESC and a newline must come out escaped. The
// name keeps the findings prefix so --apply really removes it and the
// "reclaimed" line is exercised, not skipped.
func TestPrFindingsCmd_ControlCharacterDirNameNeverReachesOutputRaw(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot hold a newline")
	}
	dir := t.TempDir()
	evil := filepath.Join(dir, "forgectl-findings-\x1b[2J\nforged")
	mkStaleFindingsDir(t, evil)
	client := pr.New(nil, pr.WithFindingsDir(dir), pr.WithSessionsDir(t.TempDir()))
	withConfirmFn(t, func(string) (bool, error) { return true, nil })

	for _, args := range [][]string{
		{"list"},
		{"cleanup", "--older-than=0"},
		{"cleanup", "--older-than=0", "--apply"},
	} {
		cmd := newPrFindingsCmd(client, theme.Theme{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%v: unexpected error: %v", args, err)
		}
		body := out.String()
		if strings.Contains(body, "\x1b") {
			t.Errorf("%v: output carries a raw ESC; got %q", args, body)
		}
		if strings.Contains(body, "\nforged") {
			t.Errorf("%v: output carries the name's raw newline; got %q", args, body)
		}
		if !strings.Contains(body, termsafe.QuotePath(evil)) {
			t.Errorf("%v: output missing the escaped path %s; got %q", args, termsafe.QuotePath(evil), body)
		}
	}
	if _, err := os.Stat(evil); !os.IsNotExist(err) {
		t.Errorf("--apply left %q in place (err=%v), want it reclaimed so the reclaimed line was exercised", evil, err)
	}
}
