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
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// seedSummaries writes real breadcrumbs into a fresh session-state dir and
// returns them through Client.List — the ONLY supported way to obtain a
// SessionSummary. Its fields are private precisely so a test cannot hand-build
// one and assert a liveness nobody verified, so presentation fixtures seed
// real records instead of faking the type.
//
// A ref in stale has its workspace deleted after the breadcrumb is written,
// reproducing the #212 state.
func seedSummaries(t *testing.T, sessionsDir string, live, stale []pr.Ref) []pr.SessionSummary {
	t.Helper()
	write := func(ref pr.Ref, keepWorkspace bool) {
		ws, err := os.MkdirTemp("", "forgectl-workflow-test-*")
		if err != nil {
			t.Fatalf("seed workspace: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(ws) })
		body, err := json.Marshal(map[string]any{
			"workspace": ws,
			"ref":       ref.String(),
			"agent":     "claude",
			"createdAt": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatalf("marshal breadcrumb: %v", err)
		}
		name := fmt.Sprintf("%s-%s-%d-%d.json", ref.Owner, ref.Repo, ref.Number, time.Now().UnixNano())
		if err := os.WriteFile(filepath.Join(sessionsDir, name), append(body, '\n'), 0o600); err != nil {
			t.Fatalf("seed breadcrumb: %v", err)
		}
		if !keepWorkspace {
			if err := os.RemoveAll(ws); err != nil {
				t.Fatalf("stale the workspace: %v", err)
			}
		}
	}
	for _, ref := range live {
		write(ref, true)
	}
	for _, ref := range stale {
		write(ref, false)
	}

	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(sessionsDir))
	summaries, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := len(live) + len(stale); len(summaries) != want {
		t.Fatalf("List returned %d summaries, want %d", len(summaries), want)
	}
	return summaries
}

func TestRenderSessions_ListsRefAgeAndPath(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	summaries := seedSummaries(t, t.TempDir(), []pr.Ref{ref}, nil)

	var out bytes.Buffer
	renderSessions(&out, summaries)

	got := out.String()
	if !strings.Contains(got, "cameronsjo/forgectl#42") {
		t.Errorf("missing ref in output: %q", got)
	}
	if !strings.Contains(got, "ago)") {
		t.Errorf("missing age suffix in output: %q", got)
	}
	if !strings.Contains(got, summaries[0].Path()) {
		t.Errorf("missing breadcrumb path in output: %q", got)
	}
	if strings.Contains(got, workspaceMissingStatus) {
		t.Errorf("a live session must not be marked %q: %q", workspaceMissingStatus, got)
	}
}

// TestRenderSessions_MarksMissingWorkspace pins the dashboard half of #212: a
// record whose workspace is gone is SHOWN, with its breadcrumb path, and
// marked — hiding it is what let the leftovers accumulate unnoticed.
func TestRenderSessions_MarksMissingWorkspace(t *testing.T) {
	liveRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	staleRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 2}
	summaries := seedSummaries(t, t.TempDir(), []pr.Ref{liveRef}, []pr.Ref{staleRef})

	var out bytes.Buffer
	renderSessions(&out, summaries)

	var liveLine, staleLine string
	for _, line := range strings.Split(out.String(), "\n") {
		switch {
		case strings.Contains(line, liveRef.String()):
			liveLine = line
		case strings.Contains(line, staleRef.String()):
			staleLine = line
		}
	}
	if liveLine == "" || staleLine == "" {
		t.Fatalf("both rows must render; got:\n%s", out.String())
	}
	if !strings.Contains(staleLine, workspaceMissingStatus) {
		t.Errorf("stale row missing the %q marker: %q", workspaceMissingStatus, staleLine)
	}
	if !strings.Contains(staleLine, ".json") {
		t.Errorf("stale row must still carry the breadcrumb path teardown takes: %q", staleLine)
	}
	if strings.Contains(liveLine, workspaceMissingStatus) {
		t.Errorf("live row must not be marked: %q", liveLine)
	}
}

// TestRenderSessions_MarksUnclassifiedWorkspace pins the third arm of the
// fail-closed enum, which the dashboard used to render as nothing at all.
//
// A summary for which NEITHER predicate holds — the zero value, the only such
// shape constructible outside internal/pr, and the shape that type's contract
// warns consumers about — must not print as an ordinary unmarked row. An
// unmarked row is the LIVE rendering, so silence there asserts liveness that
// nothing verified. `pr list` already says so in its status column; the dash
// now says the same thing in its suffix.
func TestRenderSessions_MarksUnclassifiedWorkspace(t *testing.T) {
	var zero pr.SessionSummary
	if zero.IsWorkspaceLive() || zero.IsWorkspaceMissing() {
		t.Fatal("the zero summary must hold neither predicate; this test targets that state")
	}

	var out bytes.Buffer
	renderSessions(&out, []pr.SessionSummary{zero})

	got := out.String()
	if !strings.Contains(got, workspaceUnclassifiedStatus) {
		t.Errorf("an unclassified row must be marked %q, not rendered as a live row: %q",
			workspaceUnclassifiedStatus, got)
	}
	if strings.Contains(got, workspaceMissingStatus) {
		t.Errorf("an unclassified row must not borrow the missing label: %q", got)
	}
}

// TestSessionStatus_UnclassifiedMatchesTheDash pins that the two human sinks
// agree on the unclassified state, so a future edit cannot leave one of them
// silently rendering it as live.
func TestSessionStatus_UnclassifiedMatchesTheDash(t *testing.T) {
	var zero pr.SessionSummary
	if got := sessionStatus(nil, zero, true); got != workspaceUnclassifiedStatus {
		t.Errorf("sessionStatus(unclassified) = %q, want %q", got, workspaceUnclassifiedStatus)
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
