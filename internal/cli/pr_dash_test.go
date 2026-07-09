package cli

// Test plan for pr_dash.go
//
// renderSessions (Classification: pure rendering helper)
//   [x] Boundary: no sessions → "(none)"
//   [x] Happy: a session renders its ref, an "ago" age, and its breadcrumb path
//
// newPrDashCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: all three section headers render, in order (active reviews,
//       awaiting your review, your open PRs)
//   [x] Happy: an active-review breadcrumb (from a real Prepare) surfaces under
//       "active reviews"
//   [x] Happy: a reviewed PR in awaiting/open is dimmed (ANSI wrap), matching
//       the `prs` command's dim contract
//   [x] Happy: per-query degradation notes land on stderr, not stdout

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// dashRunner fakes gh search prs (dash's two queries), gh pr view (for a real
// Prepare to build an active-review breadcrumb), and git/tmux as no-ops.
func dashRunner(searchJSON string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "search" && args[1] == "prs" {
			return searchJSON, nil
		}
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		return "", nil // git clone / tmux succeed as no-ops
	}}
}

func TestRenderSessions_NoSessions_ShowsNone(t *testing.T) {
	var out bytes.Buffer
	renderSessions(&out, nil)
	if got := strings.TrimSpace(out.String()); got != "(none)" {
		t.Errorf("renderSessions(nil) = %q, want %q", got, "(none)")
	}
}

func TestRenderSessions_ListsRefAgeAndPath(t *testing.T) {
	sess := pr.Session{
		Ref:       pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42},
		Path:      "/tmp/forgectl/pr-sessions/cameronsjo-forgectl-42.json",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	var out bytes.Buffer
	renderSessions(&out, []pr.Session{sess})

	got := out.String()
	if !strings.Contains(got, "cameronsjo/forgectl#42") {
		t.Errorf("missing ref in output: %q", got)
	}
	if !strings.Contains(got, "ago)") {
		t.Errorf("missing age suffix in output: %q", got)
	}
	if !strings.Contains(got, sess.Path) {
		t.Errorf("missing breadcrumb path in output: %q", got)
	}
}

func TestDashCmd_ThreeSectionsRenderInOrder(t *testing.T) {
	client := pr.New(dashRunner("[]"), pr.WithSessionsDir(t.TempDir()))
	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("dash: %v", err)
	}

	got := stdout.String()
	activeIdx := strings.Index(got, "active reviews")
	awaitingIdx := strings.Index(got, "awaiting your review")
	openIdx := strings.Index(got, "your open PRs")
	if activeIdx < 0 || awaitingIdx < 0 || openIdx < 0 {
		t.Fatalf("missing a section header; stdout:\n%s", got)
	}
	if !(activeIdx < awaitingIdx && awaitingIdx < openIdx) {
		t.Errorf("sections out of order: active=%d awaiting=%d open=%d", activeIdx, awaitingIdx, openIdx)
	}
	if !strings.Contains(got, "(none)") {
		t.Errorf("empty active reviews should render (none); stdout:\n%s", got)
	}
}

func TestDashCmd_ActiveReviewSurfacesFromRealBreadcrumb(t *testing.T) {
	fakeClaudeBin(t)
	sessionsDir := t.TempDir()
	fake := dashRunner("[]")
	client := pr.New(fake, pr.WithSessionsDir(sessionsDir))

	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	sess, err := client.Prepare(context.Background(), ref, pr.PrepareOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("dash: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, ref.String()) {
		t.Errorf("active review ref missing from dash output: %q", got)
	}
	if !strings.Contains(got, sess.Path) {
		t.Errorf("active review breadcrumb path missing from dash output: %q", got)
	}
}

func TestDashCmd_DimsReviewedRow(t *testing.T) {
	forceColor(t)
	searchJSON := "[" + prSearchRow("cameronsjo/forgectl", 42) + "," + prSearchRow("cameronsjo/homeclaw", 7) + "]"
	client := pr.New(dashRunner(searchJSON), pr.WithSessionsDir(t.TempDir()))

	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	seedReviewed(t, reviewedPath, pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42},
		time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC))

	cmd := newPrDashCmdForClient(client, reviewedPath)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("dash: %v", err)
	}

	var forgeLine, homeLine string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "cameronsjo/forgectl") && strings.Contains(line, "42") {
			forgeLine = line
		}
		if strings.Contains(line, "cameronsjo/homeclaw") {
			homeLine = line
		}
	}
	if forgeLine == "" || homeLine == "" {
		t.Fatalf("missing expected rows; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(forgeLine, "\x1b[") {
		t.Errorf("reviewed row (#42) should be dimmed (ANSI), got %q", forgeLine)
	}
	if strings.Contains(homeLine, "\x1b[") {
		t.Errorf("unreviewed row (#7) should be plain, got %q", homeLine)
	}
}

func TestDashCmd_DegradationNotesOnStderr(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "search" && args[1] == "prs" {
			for _, a := range args {
				if a == "--author" {
					return "", errors.New("gh: not authenticated")
				}
			}
			return "[" + prSearchRow("cameronsjo/forgectl", 1) + "]", nil
		}
		return "", nil
	}}
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()))
	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("dash (degraded): %v", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Errorf("notes must not leak to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note:") {
		t.Errorf("degradation note missing from stderr: %q", stderr.String())
	}
}
