package cli

// Test plan for newPrListCmd / windowStatus (forgectl#242)
//
// windowStatus (Classification: pure rendering helper, fail-soft)
//   [x] Happy: a ref present in the live map renders "live"
//   [x] Happy: a ref absent from a READABLE live map renders "window gone"
//   [x] Unhappy: an unreadable tmux (ok=false) renders "?" for every ref,
//       including one that happens to be in the map
//
// newPrListCmd (Classification: cobra command, tmux cross-check)
//   [x] Happy: a session whose review window is live renders "live"
//   [x] Happy: a session whose window vanished renders "window gone" — the
//       forgectl#242 case, where tmux new-window exited 0, the breadcrumb
//       landed, and the agent then died taking the window with it
//   [x] Unhappy: list-windows erroring renders "?" and the command still
//       EXITS ZERO — a tmux read failure must not fail `pr list`

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// listWinRow builds one `tmux list-windows -a` fixture line in the format
// internal/tmux parses: session, index, name, active, panes on \x1f.
func listWinRow(session, name string) string {
	return strings.Join([]string{session, "1", name, "0", "1"}, "\x1f")
}

// prListRunner layers list-windows control over dashRunner, which already
// fakes the gh pr view + git calls a real Prepare makes. listErr, when
// non-nil, makes list-windows fail — the unreadable-tmux case.
//
// TRAP: listErr must NOT read "no server running"; internal/tmux/sessions.go
// converts that to an empty-but-successful window list, which is a readable
// zero rather than the unreadable failure this fake is meant to produce.
func prListRunner(listErr error, rows ...string) *exec.FakeRunner {
	out := strings.Join(rows, "\n")
	prepare := dashRunner("[]").RunFunc
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			if listErr != nil {
				return "", listErr
			}
			return out, nil
		}
		return prepare(name, args)
	}}
}

// runPrList prepares one real review session against fake, then runs
// `pr list` over it and returns stdout plus the command's error.
func runPrList(t *testing.T, fake *exec.FakeRunner, ref pr.Ref) (string, error) {
	t.Helper()
	fakeClaudeBin(t)
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))
	if _, err := client.Prepare(context.Background(), ref, pr.PrepareOpts{Agent: "claude"}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

func TestWindowStatus_Live(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{ref: true}, ref, true); got != "live" {
		t.Errorf("windowStatus(live) = %q, want %q", got, "live")
	}
}

func TestWindowStatus_Gone(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{}, ref, true); got != "window gone" {
		t.Errorf("windowStatus(absent) = %q, want %q", got, "window gone")
	}
}

// TestWindowStatus_UnreadableTmux is the fail-soft pin: when tmux could not be
// read, even a ref the map claims is live must render "?" — the map is
// meaningless in that branch, and rendering anything definite would cry wolf on
// every healthy launch.
func TestWindowStatus_UnreadableTmux(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{ref: true}, ref, false); got != "?" {
		t.Errorf("windowStatus(tmuxOK=false) = %q, want %q", got, "?")
	}
	if got := windowStatus(nil, ref, false); got != "?" {
		t.Errorf("windowStatus(nil map, tmuxOK=false) = %q, want %q", got, "?")
	}
}

func TestPrList_LiveWindow(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(nil,
		listWinRow("forgectl", "pr-cameronsjo-forgectl-9"),
		listWinRow("forgectl", "shell"),
	)
	got, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list: %v", err)
	}
	if !strings.Contains(got, "\tlive\n") {
		t.Errorf("pr list output missing a trailing \"live\" status column:\n%s", got)
	}
	if !strings.Contains(got, ref.String()) {
		t.Errorf("pr list output missing the ref:\n%s", got)
	}

	// The COLUMN CONTRACT, pinned by position and not merely by suffix.
	// README documents field 3 as the breadcrumb `pr teardown` is fed, so a
	// suffix check on the status alone is not enough: a future change could
	// insert a column at position 2, keep status last, pass every other
	// assertion here, and still hand `cut -f3` a timestamp.
	line := strings.TrimSuffix(got, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) != 4 {
		t.Fatalf("pr list row has %d tab-separated fields, want exactly 4 (ref, created, breadcrumb, status):\n%s", len(fields), got)
	}
	if fields[0] != ref.String() {
		t.Errorf("field 1 = %q, want the ref %q", fields[0], ref.String())
	}
	if !strings.HasSuffix(fields[2], ".json") {
		t.Errorf("field 3 = %q, want the breadcrumb path — this is the field `pr teardown` is fed", fields[2])
	}
	if fields[3] != "live" {
		t.Errorf("field 4 = %q, want the status %q", fields[3], "live")
	}
}

// TestPrList_WindowGone is forgectl#242 end to end: the breadcrumb is on disk
// (Launch returned nil, because tmux new-window exits 0 before the agent runs)
// but no window exists, so the row must read "window gone" instead of listing
// the session as active forever.
func TestPrList_WindowGone(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(nil, listWinRow("forgectl", "shell"))
	got, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list: %v", err)
	}
	if !strings.Contains(got, "window gone") {
		t.Errorf("pr list output missing \"window gone\" for a vanished review window:\n%s", got)
	}
}

// TestPrList_UnreadableTmux_DegradesAndSucceeds is the single biggest risk in
// this change, pinned: an unreadable tmux must degrade to "?" and MUST NOT
// fail the command or render "window gone".
func TestPrList_UnreadableTmux_DegradesAndSucceeds(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(errors.New("boom: tmux exploded"))
	got, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list must succeed when tmux is unreadable, got: %v", err)
	}
	if !strings.Contains(got, "\t?\n") {
		t.Errorf("pr list output missing a trailing \"?\" status for an unreadable tmux:\n%s", got)
	}
	if strings.Contains(got, "window gone") {
		t.Errorf("an unreadable tmux must NOT render \"window gone\" — that would flag every "+
			"healthy review as dead:\n%s", got)
	}
}
