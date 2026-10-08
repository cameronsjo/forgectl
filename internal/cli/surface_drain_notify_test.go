//go:build unix

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// sentinelPane is a HERDR_PANE_ID no worker has: the drain's own pane, which
// a needs-you signal must never target.
const sentinelPane = "w0:p-drain-own-pane"

// fakePaneState records what the notifier asked of herdr.
type fakePaneState struct {
	session  string
	reported []string // "<pane>|<message>"
	released []string
	err      error
}

func (f *fakePaneState) Session() string { return f.session }

func (f *fakePaneState) ReportBlocked(_ context.Context, ref backend.Ref, message string) error {
	f.reported = append(f.reported, refPane(ref)+"|"+message)
	return f.err
}

func (f *fakePaneState) ReleaseBlocked(_ context.Context, ref backend.Ref) error {
	f.released = append(f.released, refPane(ref))
	return f.err
}

func refPane(ref backend.Ref) string {
	id, err := ref.HerdrIdentity()
	if err != nil {
		return "undecodable"
	}
	return id.Pane()
}

// notifyRig is a notifier over a fake osascript runner (as on darwin) and a
// fake herdr pane state, with a ledger row whose ref names pane w9:p1.
type notifyRig struct {
	n     needsYouNotifier
	run   *exec.FakeRunner
	panes *fakePaneState
	row   worker.QueueRow
	led   worker.Row
}

func newNotifyRig(t *testing.T) *notifyRig {
	t.Helper()
	t.Setenv("HERDR_PANE_ID", sentinelPane)
	raw, err := testHerdrRef(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	r := &notifyRig{run: &exec.FakeRunner{}, panes: &fakePaneState{session: drainTestSession}}
	r.n = needsYouNotifier{
		session: drainTestSession,
		mac:     notify.New(r.run, notify.WithGOOS("darwin")),
		panes:   func() (workerPaneState, error) { return r.panes, nil },
	}
	r.row = worker.QueueRow{Name: "fix-flake", Repo: "/repo/a", State: worker.QueueNeedsYou, Session: drainTestSession}
	r.led = worker.Row{Name: "fix-flake", Ref: raw}
	return r
}

// TestNeedsYouNotificationArgv pins the macOS half: one osascript call, with
// the title and body as their own argv elements after "--", never a shell.
func TestNeedsYouNotificationArgv(t *testing.T) {
	r := newNotifyRig(t)
	reason := `blocked: "; do shell script "rm -rf ~" & $(id)`
	if err := r.n.NeedsYou(t.Context(), r.row, r.led, reason); err != nil {
		t.Fatalf("NeedsYou: %v", err)
	}
	if len(r.run.Calls) != 1 {
		t.Fatalf("runner calls %+v, want one osascript call", r.run.Calls)
	}
	c := r.run.Calls[0]
	if c.Name != "osascript" {
		t.Fatalf("ran %q, want osascript", c.Name)
	}
	if len(c.Args) != 5 || c.Args[0] != "-e" || c.Args[2] != "--" {
		t.Fatalf("argv %q, want -e SCRIPT -- TITLE BODY", c.Args)
	}
	if strings.Contains(c.Args[1], "fix-flake") || strings.Contains(c.Args[1], "rm -rf") {
		t.Errorf("the script carries caller text: %q", c.Args[1])
	}
	if c.Args[3] != "forgectl: fix-flake needs you" {
		t.Errorf("title %q", c.Args[3])
	}
	if c.Args[4] != reason {
		t.Errorf("body %q, want the reason as one argument", c.Args[4])
	}
}

// TestNeedsYouMarksTheLedgerPane pins the pane-target rule: the worker's pane
// from its ledger ref, never the drain's own HERDR_PANE_ID.
func TestNeedsYouMarksTheLedgerPane(t *testing.T) {
	r := newNotifyRig(t)
	if err := r.n.NeedsYou(t.Context(), r.row, r.led, "idle without report"); err != nil {
		t.Fatalf("NeedsYou: %v", err)
	}
	want := "w9:p1|forgectl: fix-flake needs you — idle without report"
	if len(r.panes.reported) != 1 || r.panes.reported[0] != want {
		t.Fatalf("reported %q, want [%q]", r.panes.reported, want)
	}
	if strings.Contains(r.panes.reported[0], sentinelPane) {
		t.Fatal("the drain's own HERDR_PANE_ID was targeted")
	}
	if err := r.n.Cleared(t.Context(), r.row, r.led); err != nil {
		t.Fatalf("Cleared: %v", err)
	}
	if len(r.panes.released) != 1 || r.panes.released[0] != "w9:p1" {
		t.Fatalf("released %q, want [w9:p1]", r.panes.released)
	}
}

// TestNeedsYouUnverifiedPaneStillNotifies: a pane the adapter refuses (not
// forgectl's, not the root pane, not in the workspace) is not marked, the
// macOS notification still goes out, and the failure is returned once.
func TestNeedsYouUnverifiedPaneStillNotifies(t *testing.T) {
	t.Run("adapter refuses the pane", func(t *testing.T) {
		r := newNotifyRig(t)
		r.panes.err = herdradapter.ErrScreenUnreadable
		err := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt")
		if !errors.Is(err, herdradapter.ErrScreenUnreadable) {
			t.Fatalf("err = %v, want the adapter's refusal", err)
		}
		if len(r.run.Calls) != 1 {
			t.Fatalf("osascript calls %d, want 1", len(r.run.Calls))
		}
	})
	t.Run("worker in another session", func(t *testing.T) {
		r := newNotifyRig(t)
		r.row.Session = "other"
		if err := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt"); err == nil || !strings.Contains(err.Error(), "other") {
			t.Fatalf("err = %v, want a session mismatch", err)
		}
		if len(r.panes.reported) != 0 || len(r.run.Calls) != 1 {
			t.Fatalf("reported %q, osascript %d; want none and 1", r.panes.reported, len(r.run.Calls))
		}
	})
	t.Run("adapter pinned elsewhere", func(t *testing.T) {
		r := newNotifyRig(t)
		r.panes.session = "default"
		if err := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt"); err == nil {
			t.Fatal("an adapter on another session was used")
		}
		if len(r.panes.reported) != 0 {
			t.Fatalf("reported %q on an unpinned adapter", r.panes.reported)
		}
	})
	t.Run("no ledger ref", func(t *testing.T) {
		r := newNotifyRig(t)
		r.led.Ref = nil
		if err := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt"); err == nil {
			t.Fatal("no error for a row with no ref")
		}
		if len(r.panes.reported) != 0 || len(r.run.Calls) != 1 {
			t.Fatalf("reported %q, osascript %d", r.panes.reported, len(r.run.Calls))
		}
		if err := r.n.Cleared(t.Context(), r.row, r.led); err != nil {
			t.Fatalf("Cleared with no ref: %v", err)
		}
	})
	t.Run("osascript fails, the pane is still marked", func(t *testing.T) {
		r := newNotifyRig(t)
		r.run.RunFunc = func(string, []string) (string, error) { return "", errors.New("exit status 1") }
		if err := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt"); err == nil || !strings.Contains(err.Error(), "macOS") {
			t.Fatalf("err = %v, want the macOS failure", err)
		}
		if len(r.panes.reported) != 1 {
			t.Fatalf("reported %q, want the pane marked", r.panes.reported)
		}
	})
	t.Run("a gone workspace has nothing to clear", func(t *testing.T) {
		r := newNotifyRig(t)
		r.panes.err = herdradapter.ErrWorkerGone
		if err := r.n.Cleared(t.Context(), r.row, r.led); err != nil {
			t.Fatalf("Cleared: %v", err)
		}
	})
}

// TestNeedsYouOffDarwinSendsNoOsascript: off darwin the macOS half sends
// nothing, and the pane is still marked.
func TestNeedsYouOffDarwinSendsNoOsascript(t *testing.T) {
	r := newNotifyRig(t)
	r.n.mac = notify.New(r.run, notify.WithGOOS("linux"))
	if err := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt"); err != nil {
		t.Fatalf("NeedsYou: %v", err)
	}
	if len(r.run.Calls) != 0 || len(r.panes.reported) != 1 {
		t.Fatalf("osascript %d, reported %q", len(r.run.Calls), r.panes.reported)
	}
}
