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

// pullCmdFixture stands up tmp/name .git dirs and a *projects.Client whose git
// calls branch on the `-C <dir>` arg — porcelain empty means clean, pullOut/
// pullErr drive `git pull --rebase`'s outcome, keyed by repo name (not the
// full path, since callers here don't need it).
func pullCmdFixture(t *testing.T, names []string, porcelain, pullOut map[string]string, pullErr map[string]error) *projects.Client {
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
			return porcelain[repoName], nil
		case "rev-list":
			return "0", nil
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
	client := pullCmdFixture(t, []string{"dirty"}, map[string]string{"dirty": " M file.go"}, nil, nil)
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
