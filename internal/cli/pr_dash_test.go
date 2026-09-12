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
//
// phaseNote / renderSessions phase annotation (#499)
//   [x] needs-repair with a live workspace shows the phase and the reason
//   [x] needs-repair with no workspace shows phase + reason, not workspaceMissingStatus
//   [x] queued and preparing rows carry their phase name, not workspaceUnclassifiedStatus
//   [x] a repairReason carrying an ANSI escape renders with no raw 0x1b byte
//   [x] an active row carries no bracket note
//   [x] the legacy fixture (no phase) still renders no note
//   [x] every pr.Phase constant except active yields a non-empty phaseNote

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
	"github.com/cameronsjo/forgectl/internal/theme"
)

// dashRunner fakes gh search prs (dash's two queries), gh pr view (for a real
// Prepare to build an active-review breadcrumb), and git/tmux as no-ops.
// tmuxDouble answers the tmux commands a review dispatch makes now that the
// review session is resolved and targeted by native id rather than by name.
// list-sessions comes back EMPTY, so EnsureSession finds nothing and creates —
// the same flow the old failing `has-session` produced — and new-session
// returns an identity, because it is now invoked with `-P -F`.
//
// handled=false hands the call back to the caller's own fake, so a test that
// wants to control list-windows (or anything else) still can.
func tmuxDouble(name string, args []string) (out string, handled bool, err error) {
	if name != "tmux" || len(args) == 0 {
		return "", false, nil
	}
	switch args[0] {
	case "list-sessions":
		return "", true, nil
	case "new-session":
		return strings.Join([]string{"123", "456", "$1"}, "\x1f"), true, nil
	}
	return "", false, nil
}

func dashRunner(searchJSON string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if out, handled, err := tmuxDouble(name, args); handled {
			return out, err
		}
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
	summaries, _, err := client.List(context.Background())
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
	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"), theme.Theme{})
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

	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"), theme.Theme{})
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

	cmd := newPrDashCmdForClient(client, reviewedPath, theme.Theme{})
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
	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"), theme.Theme{})
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

// phasedRecord describes one v2 lifecycle breadcrumb for seedPhasedSummaries.
// It only carries the choices validateBreadcrumbRecord actually enforces:
// queued, preparing, and needs-repair may omit a workspace; every other phase
// requires one. Only an active record needs a windowId.
type phasedRecord struct {
	ref           pr.Ref
	phase         pr.Phase
	repairReason  string
	withWorkspace bool
}

// seedPhasedSummaries writes v2 records directly to sessionsDir — version 2,
// revision 1, phase, and an optional repairReason and workspace — and returns
// them through Client.List, the same real-record path seedSummaries uses.
// SessionSummary's fields are private precisely so a test cannot hand-build
// one; a phase-bearing fixture has to go through the loader like any other.
func seedPhasedSummaries(t *testing.T, sessionsDir string, recs []phasedRecord) []pr.SessionSummary {
	t.Helper()
	for i, rec := range recs {
		body := map[string]any{
			"ref":       rec.ref.String(),
			"agent":     "claude",
			"createdAt": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano),
			"version":   2,
			"revision":  1,
			"phase":     string(rec.phase),
		}
		if rec.repairReason != "" {
			body["repairReason"] = rec.repairReason
		}
		if rec.withWorkspace {
			ws, err := os.MkdirTemp("", "forgectl-workflow-test-*")
			if err != nil {
				t.Fatalf("seed workspace: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(ws) })
			body["workspace"] = ws
		}
		if rec.phase == pr.PhaseActive {
			body["windowId"] = fmt.Sprintf("123\x1f456\x1f@%d", i+1)
		}
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal phased breadcrumb: %v", err)
		}
		name := fmt.Sprintf("%s-%s-%d-%d.json", rec.ref.Owner, rec.ref.Repo, rec.ref.Number, time.Now().UnixNano()+int64(i))
		if err := os.WriteFile(filepath.Join(sessionsDir, name), append(data, '\n'), 0o600); err != nil {
			t.Fatalf("seed phased breadcrumb: %v", err)
		}
	}

	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(sessionsDir))
	summaries, unreadable, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if unreadable != 0 {
		t.Fatalf("List found %d unreadable records", unreadable)
	}
	if len(summaries) != len(recs) {
		t.Fatalf("List returned %d summaries, want %d", len(summaries), len(recs))
	}
	return summaries
}

// TestDashRenderSessions_NeedsRepairLiveWorkspace_ShowsPhaseAndReason pins case 1:
// a needs-repair record with a live workspace must render its phase and the
// diagnostic reason, not the healthy-looking unmarked row it got before #499.
func TestDashRenderSessions_NeedsRepairLiveWorkspace_ShowsPhaseAndReason(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 10}
	summaries := seedPhasedSummaries(t, t.TempDir(), []phasedRecord{
		{ref: ref, phase: pr.PhaseNeedsRepair, repairReason: "launch failed: window closed", withWorkspace: true},
	})

	var out bytes.Buffer
	renderSessions(&out, summaries)
	got := out.String()

	if !strings.Contains(got, "needs-repair") {
		t.Errorf("missing phase in output: %q", got)
	}
	if !strings.Contains(got, "launch failed: window closed") {
		t.Errorf("missing repair reason in output: %q", got)
	}
}

// TestDashRenderSessions_NeedsRepairNoWorkspace_ShowsPhaseAndReason pins case 2: a
// needs-repair record with no workspace (a reservation whose clone failed)
// must show its phase and reason, and must NOT be labeled workspaceMissingStatus
// — a park with no workspace by design is not the same state as one whose
// workspace was deleted out from under it.
func TestDashRenderSessions_NeedsRepairNoWorkspace_ShowsPhaseAndReason(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 11}
	summaries := seedPhasedSummaries(t, t.TempDir(), []phasedRecord{
		{ref: ref, phase: pr.PhaseNeedsRepair, repairReason: "clone failed: no space left on device"},
	})

	var out bytes.Buffer
	renderSessions(&out, summaries)
	got := out.String()

	if !strings.Contains(got, "needs-repair") {
		t.Errorf("missing phase in output: %q", got)
	}
	if !strings.Contains(got, "clone failed: no space left on device") {
		t.Errorf("missing repair reason in output: %q", got)
	}
	if strings.Contains(got, workspaceMissingStatus) {
		t.Errorf("a workspace-none needs-repair row must not be labeled %q: %q", workspaceMissingStatus, got)
	}
}

// TestDashRenderSessions_QueuedAndPreparing_ShowPhaseNotUnclassified pins case 3:
// queued and preparing rows have no workspace by design, and must carry their
// phase name rather than falling into the unclassified-workspace internal
// error the old three-arm switch produced.
func TestDashRenderSessions_QueuedAndPreparing_ShowPhaseNotUnclassified(t *testing.T) {
	queuedRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 12}
	preparingRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 13}
	summaries := seedPhasedSummaries(t, t.TempDir(), []phasedRecord{
		{ref: queuedRef, phase: pr.PhaseQueued},
		{ref: preparingRef, phase: pr.PhasePreparing},
	})

	var out bytes.Buffer
	renderSessions(&out, summaries)
	got := out.String()

	if strings.Contains(got, workspaceUnclassifiedStatus) {
		t.Errorf("queued/preparing rows must not print %q: %q", workspaceUnclassifiedStatus, got)
	}
	if !strings.Contains(got, "queued") {
		t.Errorf("missing queued phase in output: %q", got)
	}
	if !strings.Contains(got, "preparing") {
		t.Errorf("missing preparing phase in output: %q", got)
	}
}

// TestDashRenderSessions_RepairReasonEscapesAnsi pins case 4: a repairReason is
// written at an arbitrary throw site and can carry a captured process's raw
// output, so it is untrusted the same way a breadcrumb path is. The rendered
// line must carry no raw ESC byte.
func TestDashRenderSessions_RepairReasonEscapesAnsi(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 14}
	summaries := seedPhasedSummaries(t, t.TempDir(), []phasedRecord{
		{ref: ref, phase: pr.PhaseNeedsRepair, repairReason: "launch failed: \x1b[31mboom"},
	})

	var out bytes.Buffer
	renderSessions(&out, summaries)
	got := out.String()

	if strings.Contains(got, "\x1b") {
		t.Errorf("repair reason must not carry a raw ESC byte into the terminal: %q", got)
	}
}

// TestDashRenderSessions_ActiveRow_NoPhaseNote pins case 5: active is the
// unmarked baseline. A record actively under review must render exactly as
// it did before phases existed, carrying no bracket note.
func TestDashRenderSessions_ActiveRow_NoPhaseNote(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 15}
	summaries := seedPhasedSummaries(t, t.TempDir(), []phasedRecord{
		{ref: ref, phase: pr.PhaseActive, withWorkspace: true},
	})

	var out bytes.Buffer
	renderSessions(&out, summaries)
	got := out.String()

	if strings.Contains(got, "[") {
		t.Errorf("an active row must carry no bracket phase note: %q", got)
	}
}

// TestDashRenderSessions_LegacyFixture_NoPhaseNote pins case 6: the pre-#299
// legacy fixture (no phase field at all) must keep rendering with no note —
// phaseNote's empty-phase branch is the one guaranteeing that.
func TestDashRenderSessions_LegacyFixture_NoPhaseNote(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 16}
	summaries := seedSummaries(t, t.TempDir(), []pr.Ref{ref}, nil)

	var out bytes.Buffer
	renderSessions(&out, summaries)
	got := out.String()

	if strings.Contains(got, "[") {
		t.Errorf("a legacy (phaseless) row must carry no bracket note: %q", got)
	}
}

// TestDashPhaseNote_EveryPhaseExceptActiveYieldsANote pins case 7: active and the
// legacy empty phase are the only silent cases. Every phase this build knows
// about — including one a future build might add to the switch's default arm
// — must render by name rather than reading as a healthy review.
func TestDashPhaseNote_EveryPhaseExceptActiveYieldsANote(t *testing.T) {
	allPhases := []pr.Phase{
		pr.PhaseQueued, pr.PhasePreparing, pr.PhasePrepared,
		pr.PhaseLaunching, pr.PhaseActive, pr.PhaseNeedsRepair,
	}
	for i, phase := range allPhases {
		rec := phasedRecord{
			ref:   pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 20 + i},
			phase: phase,
		}
		switch phase {
		case pr.PhaseQueued, pr.PhasePreparing:
			// no workspace by design
		case pr.PhaseNeedsRepair:
			rec.repairReason = "some diagnostic"
		default:
			rec.withWorkspace = true
		}
		summaries := seedPhasedSummaries(t, t.TempDir(), []phasedRecord{rec})

		note := phaseNote(summaries[0])
		if phase == pr.PhaseActive {
			if note != "" {
				t.Errorf("phase %q: phaseNote = %q, want empty", phase, note)
			}
			continue
		}
		if note == "" {
			t.Errorf("phase %q: phaseNote returned empty, want a non-empty note", phase)
		}
	}
}

