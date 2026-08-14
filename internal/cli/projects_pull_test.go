package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// v2Clean is the porcelain-v2 branch header block git reports for a clean
// tree on an up-to-date tracking branch — the baseline every fixture repo here
// starts from, with dirty records appended per repo.
const v2Clean = "# branch.oid e69de29bb2d1d6434b8b29ae775ad8c2e48c5391\n" +
	"# branch.head main\n" +
	"# branch.upstream origin/main\n" +
	"# branch.ab +0 -0"

// v2ModifiedRecord is a complete porcelain-v2 ordinary record — one modified
// tracked file. Written out in full because the parser validates the fixed
// fields and rejects a truncated record as unreadable rather than dirty.
const v2ModifiedRecord = "1 .M N... 100644 100644 100644 " +
	"e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 file.go"

// pullCmdFixture stands up tmp/name .git dirs and a *projects.Client whose git
// calls branch on the `-C <dir>` arg — statusRecords supplies each repo's
// porcelain-v2 records (absent means clean), pullOut/pullErr drive `git pull
// --rebase`'s outcome, keyed by repo name (not the full path, since callers
// here don't need it).
func pullCmdFixture(t *testing.T, names []string, statusRecords, pullOut map[string]string, pullErr map[string]error) *projects.Client {
	t.Helper()
	tmp := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(tmp, n, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PROJECTS_DIR", tmp)
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "git" || len(args) < 3 || args[0] != "-C" {
			return "", nil
		}
		repoName := filepath.Base(args[1])
		switch args[2] {
		case "status":
			if rec := statusRecords[repoName]; rec != "" {
				return v2Clean + "\n" + rec, nil
			}
			return v2Clean, nil
		case "pull":
			return pullOut[repoName], pullErr[repoName]
		}
		return "", nil
	}}
	return projects.New(fake)
}

func TestPullAllCmd_AllClean_ReturnsNilAndRendersGlyphs(t *testing.T) {
	client := pullCmdFixture(t, []string{"repoa", "repob"}, nil,
		map[string]string{"repoa": "Already up to date.", "repob": "Fast-forward\n f | 1 +"}, nil)
	cmd := newProjectsPullAllCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "✓") {
		t.Errorf("stdout should show the up-to-date glyph, got: %q", out)
	}
	if !strings.Contains(out, "↓") {
		t.Errorf("stdout should show the updated glyph, got: %q", out)
	}
	if stderr.String() != "" {
		t.Errorf("expected no stderr output, got: %q", stderr.String())
	}
}

func TestPullAllCmd_DirtyRepo_ShowsWarningGlyphAndDoesNotFail(t *testing.T) {
	client := pullCmdFixture(t, []string{"dirty"}, map[string]string{"dirty": v2ModifiedRecord}, nil, nil)
	cmd := newProjectsPullAllCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("a dirty repo alone must not fail the command: %v", err)
	}
	if !strings.Contains(stdout.String(), "⚠") {
		t.Errorf("stdout should show the skipped-dirty glyph, got: %q", stdout.String())
	}
}

func TestPullAllCmd_FailedPull_ReturnsAggregateError(t *testing.T) {
	client := pullCmdFixture(t, []string{"ok", "broken"}, nil,
		map[string]string{"ok": "Already up to date."},
		map[string]error{"broken": errors.New("conflict")})
	cmd := newProjectsPullAllCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an aggregate error when a repo fails to pull, got nil")
	}
	if !strings.Contains(err.Error(), "1 of 2 repos failed to pull") {
		t.Errorf("error = %q; want the aggregate failure count", err.Error())
	}
	if !strings.Contains(stdout.String(), "✗") {
		t.Errorf("stdout should show the failed glyph, got: %q", stdout.String())
	}
}

func TestPullAllCmd_DirArg_PassedThrough(t *testing.T) {
	client := pullCmdFixture(t, []string{"repoa"}, nil, map[string]string{"repoa": "Already up to date."}, nil)
	cmd := newProjectsPullAllCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"/nonexistent/subtree"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a nonexistent dir argument, got nil")
	}
}
