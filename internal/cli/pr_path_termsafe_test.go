package cli

// Test plan for breadcrumb paths reaching a terminal (forgectl#212)
//
// The sink is older than this branch, but the branch newly ROUTES a whole
// class of records to it: base List used loadSession, so a record whose
// workspace was gone never printed at all. A breadcrumb FILENAME is the one
// field in these rows chosen on disk rather than parsed, so it is the one that
// can carry ANSI or bidi controls.
//
//   [x] `pr list` renders a control-bearing filename with nothing unsafe left
//   [x] `pr list` leaves an ORDINARY path verbatim — field 3 is the documented
//       argument `pr teardown` is fed, and quoting every row would break it
//   [x] `pr dash` renders a control-bearing filename with nothing unsafe left

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hostileBreadcrumbName carries a cursor-clearing CSI sequence, a bare escape,
// a carriage return, and a right-to-left override — the shapes that let a
// filename repaint or reorder a line it was only supposed to appear on.
const hostileBreadcrumbName = "o-r-1-\x1b[2K\rspoofed‮desrever.json"

// seedHostileBreadcrumb writes one stale breadcrumb whose FILENAME is hostile
// while its record contents are perfectly ordinary, and returns the sessions
// dir. Stale, so `pr list` renders it without asking tmux anything.
func seedHostileBreadcrumb(t *testing.T) string {
	t.Helper()
	sessionsDir := t.TempDir()
	ws, err := os.MkdirTemp("", "forgectl-workflow-test-*")
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"workspace": ws,
		"ref":       "o/r#1",
		"agent":     "claude",
		"createdAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("marshal breadcrumb: %v", err)
	}
	path := filepath.Join(sessionsDir, hostileBreadcrumbName)
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Skipf("this filesystem rejects control characters in filenames: %v", err)
	}
	if err := os.RemoveAll(ws); err != nil {
		t.Fatalf("stale the workspace: %v", err)
	}
	return sessionsDir
}

// assertRenderIsInert fails when any rune the terminal would act on survived
// into out, and when the row went missing entirely — an escaped render is only
// meaningful if the record still appears.
func assertRenderIsInert(t *testing.T, out string) {
	t.Helper()
	for _, r := range out {
		if r != '\n' && r != '\t' && termsafe.IsUnsafeTerminalRune(r) {
			t.Fatalf("unsafe rune %U reached the rendered output: %q", r, out)
		}
	}
	if !strings.Contains(out, "spoofed") {
		t.Fatalf("the breadcrumb row disappeared instead of being escaped:\n%q", out)
	}
}

func TestPrList_EscapesAControlBearingBreadcrumbName(t *testing.T) {
	sessionsDir := seedHostileBreadcrumb(t)
	fake := &exec.FakeRunner{}
	client := pr.New(fake, pr.WithSessionsDir(sessionsDir), pr.WithTmuxSession("forgectl"))

	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pr list: %v", err)
	}
	assertRenderIsInert(t, stdout.String())
}

// TestPrList_LeavesAnOrdinaryPathVerbatim pins the other half: the escaping is
// conditional, so the documented `pr teardown` argument survives untouched.
func TestPrList_LeavesAnOrdinaryPathVerbatim(t *testing.T) {
	sessionsDir := t.TempDir()
	seedSummaries(t, sessionsDir, nil, []pr.Ref{{Owner: "o", Repo: "r", Number: 1}})
	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(sessionsDir), pr.WithTmuxSession("forgectl"))
	summaries, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pr list: %v", err)
	}

	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\t")
	if len(fields) != 4 {
		t.Fatalf("want 4 tab-separated fields, got %d: %q", len(fields), stdout.String())
	}
	if fields[2] != summaries[0].Path() {
		t.Errorf("field 3 = %q, want the raw breadcrumb path %q — this is what `pr teardown` is fed",
			fields[2], summaries[0].Path())
	}
}

func TestPrDash_EscapesAControlBearingBreadcrumbName(t *testing.T) {
	sessionsDir := seedHostileBreadcrumb(t)
	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(sessionsDir))
	summaries, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("want the hostile breadcrumb listed, got %d summaries", len(summaries))
	}

	var out bytes.Buffer
	renderSessions(&out, summaries)
	assertRenderIsInert(t, out.String())
	if !strings.Contains(out.String(), fmt.Sprintf("(%s)", workspaceMissingStatus)) {
		t.Errorf("the stale marker must survive escaping:\n%q", out.String())
	}
}
