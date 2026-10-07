package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const testBriefMarker = "00112233aabb"

// fakePane models a worker's input box: typed text, Enter, a dialog that can
// appear at any step, and herdr's status.
type fakePane struct {
	input     string
	status    string
	dialog    bool
	enters    int
	typed     []string
	reads     int
	readErr   error
	typeErr   error
	enterErr  error
	recordErr error
	recorded  []worker.Brief
	// onType and onEnter run after the step, to stage what the worker does
	// next (a dialog, a paste placeholder, a turn starting).
	onType  func(*fakePane)
	onEnter func(*fakePane)
	// afterReads, when set, runs on the read with that 1-based index.
	afterReads map[int]func(*fakePane)
}

func (p *fakePane) read(context.Context) (ready.Screen, error) {
	p.reads++
	if fn := p.afterReads[p.reads]; fn != nil {
		fn(p)
	}
	if p.readErr != nil {
		return ready.Screen{}, p.readErr
	}
	text := "input:" + p.input
	if p.dialog {
		text = "dialog"
	}
	return ready.Screen{Text: text, Agent: "claude", Status: p.status}, nil
}

// evaluate is a stand-in for the predicates: a dialog blocks, idle at the
// prompt is ready with the box's text as Input.
func (p *fakePane) evaluate(s ready.Screen) ready.Verdict {
	if s.Text == "dialog" {
		return ready.Verdict{State: ready.StateBlocked, Blocking: "permission prompt", Reason: "showing the permission prompt"}
	}
	if s.Status != "idle" && s.Status != "done" {
		// Input rides along on a not-ready verdict too, which the real
		// predicates never do, so the read-back's own ready check is reached.
		return ready.Verdict{State: ready.StateNotReady, Reason: "herdr reports agent status " + s.Status, Input: strings.TrimPrefix(s.Text, "input:")}
	}
	return ready.Verdict{State: ready.StateReady, Input: strings.TrimPrefix(s.Text, "input:")}
}

func (p *fakePane) steps() briefSteps {
	now := time.Unix(0, 0)
	return briefSteps{
		read:     p.read,
		evaluate: p.evaluate,
		typeText: func(_ context.Context, s string) error {
			if p.typeErr != nil {
				return p.typeErr
			}
			p.typed = append(p.typed, s)
			p.input += s
			if p.onType != nil {
				p.onType(p)
			}
			return nil
		},
		enter: func(context.Context) error {
			if p.enterErr != nil {
				return p.enterErr
			}
			p.enters++
			if p.onEnter != nil {
				p.onEnter(p)
			}
			return nil
		},
		record: func(b worker.Brief) (int, error) {
			if p.recordErr != nil {
				return 0, p.recordErr
			}
			p.recorded = append(p.recorded, b)
			return len(p.recorded), nil
		},
		now:      func() time.Time { return now },
		sleep:    func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
		readback: 3 * time.Second,
		start:    5 * time.Second,
		interval: 200 * time.Millisecond,
	}
}

func startsTurn(p *fakePane) { p.input, p.status = "", "working" }

func TestSendBrief(t *testing.T) {
	ctx := context.Background()

	t.Run("types, reads back, records, presses Enter once, sees the turn", func(t *testing.T) {
		p := &fakePane{status: "idle", onEnter: startsTurn}
		r := sendBrief(ctx, p.steps(), "run the tests", testBriefMarker)
		if r.Outcome != briefSent || p.enters != 1 || len(p.recorded) != 1 {
			t.Fatalf("result %+v, enters %d, recorded %d", r, p.enters, len(p.recorded))
		}
		if want := worker.Compose("run the tests", testBriefMarker, worker.ViaTyped); p.typed[0] != want {
			t.Fatalf("typed %q, want %q", p.typed[0], want)
		}
		if p.recorded[0].Marker != testBriefMarker || p.recorded[0].Via != worker.ViaTyped {
			t.Fatalf("recorded %+v", p.recorded[0])
		}
	})

	// The plan's negative controls: a dialog after ready passed, and one
	// between typing and Enter, are both caught with Enter never sent.
	refusals := map[string]struct {
		pane func() *fakePane
		step string
		// reads, when set, is how many reads the refusal may take: a dialog
		// must stop the read-back at once, not by running out its timeout.
		reads int
	}{
		"not at its prompt": {
			pane: func() *fakePane { return &fakePane{status: "working"} },
			step: "check",
		},
		"a dialog before anything is typed": {
			pane: func() *fakePane { return &fakePane{status: "idle", dialog: true} },
			step: "check",
		},
		"text already in the box": {
			pane: func() *fakePane { return &fakePane{status: "idle", input: "half a thought"} },
			step: "check",
		},
		"a dialog appears after ready passed, as the text is typed": {
			pane: func() *fakePane {
				return &fakePane{status: "idle", onType: func(p *fakePane) { p.dialog = true }}
			},
			step:  "readback",
			reads: 2,
		},
		"a dialog appears between the read-back and Enter's turn": {
			pane: func() *fakePane {
				// The read-back's first read sees the text; the dialog shows
				// on the same read, so the read-back must not pass it.
				return &fakePane{status: "idle", afterReads: map[int]func(*fakePane){2: func(p *fakePane) { p.dialog = true }}}
			},
			step:  "readback",
			reads: 2,
		},
		"herdr stops reporting idle while the text shows": {
			pane: func() *fakePane {
				return &fakePane{status: "idle", onType: func(p *fakePane) { p.status = "working" }}
			},
			step: "readback",
		},
		"the harness collapsed the text into a placeholder": {
			pane: func() *fakePane {
				return &fakePane{status: "idle", onType: func(p *fakePane) { p.input = "[Pasted text #1]" }}
			},
			step: "readback",
		},
		"the read-back never succeeds": {
			pane: func() *fakePane {
				return &fakePane{status: "idle", afterReads: map[int]func(*fakePane){2: func(p *fakePane) { p.readErr = errors.New("herdr down") }}}
			},
			step: "readback",
		},
		"typing fails": {
			pane: func() *fakePane { return &fakePane{status: "idle", typeErr: herdradapter.ErrSendFailed} },
			step: "type",
		},
		"the screen changes between the read-back and Enter": {
			pane: func() *fakePane {
				// Read 1 is the check, read 2 the matching read-back; the
				// dialog draws before read 3, the last look before Enter.
				return &fakePane{status: "idle", afterReads: map[int]func(*fakePane){3: func(p *fakePane) { p.dialog = true }}}
			},
			step:  "enter-check",
			reads: 3,
		},
		"the ledger write fails": {
			pane: func() *fakePane { return &fakePane{status: "idle", recordErr: errors.New("disk full")} },
			step: "record",
		},
	}
	for name, c := range refusals {
		t.Run("refuses: "+name, func(t *testing.T) {
			p := c.pane()
			r := sendBrief(ctx, p.steps(), "run the tests", testBriefMarker)
			if r.Outcome != briefRefused || r.Step != c.step {
				t.Fatalf("result %+v, want refused at %s", r, c.step)
			}
			if p.enters != 0 {
				t.Fatalf("Enter was sent %d times", p.enters)
			}
			if c.reads > 0 && p.reads != c.reads {
				t.Fatalf("refused after %d reads, want %d", p.reads, c.reads)
			}
		})
	}

	t.Run("a dialog instead of a turn after Enter is unconfirmed, naming it", func(t *testing.T) {
		p := &fakePane{status: "idle", onEnter: func(p *fakePane) { p.dialog = true }}
		r := sendBrief(ctx, p.steps(), "run the tests", testBriefMarker)
		if r.Outcome != briefUnconfirmed || r.Blocking == "" || p.enters != 1 {
			t.Fatalf("result %+v, enters %d", r, p.enters)
		}
	})

	t.Run("no turn after Enter is unconfirmed and keeps the marker", func(t *testing.T) {
		p := &fakePane{status: "idle"}
		r := sendBrief(ctx, p.steps(), "run the tests", testBriefMarker)
		if r.Outcome != briefUnconfirmed || r.Marker != testBriefMarker || p.enters != 1 {
			t.Fatalf("result %+v, enters %d", r, p.enters)
		}
	})

	t.Run("a failed Enter is unconfirmed, not refused", func(t *testing.T) {
		p := &fakePane{status: "idle", enterErr: herdradapter.ErrSendFailed}
		r := sendBrief(ctx, p.steps(), "run the tests", testBriefMarker)
		if r.Outcome != briefUnconfirmed || r.Step != "enter" || len(p.recorded) != 1 {
			t.Fatalf("result %+v, recorded %d", r, len(p.recorded))
		}
	})
}

func TestReadBriefArg(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(file, []byte("Fix it.\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readBriefArg("@" + file); err != nil || got != "Fix it." {
		t.Fatalf("readBriefArg(@file) = %q, %v", got, err)
	}
	if got, err := readBriefArg("plain text"); err != nil || got != "plain text" {
		t.Fatalf("readBriefArg(text) = %q, %v", got, err)
	}
	if _, err := readBriefArg("@" + dir); err == nil {
		t.Fatal("a directory was read as a brief")
	}
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", worker.MaxLaunchBrief+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBriefArg("@" + big); err == nil {
		t.Fatal("an oversize brief file was read")
	}
}

func TestLaunchBrief(t *testing.T) {
	now := func() time.Time { return time.Unix(100, 0) }
	if p, b, err := launchBrief("", now); p != "" || b != nil || err != nil {
		t.Fatalf("an empty --brief gave %q, %+v, %v", p, b, err)
	}
	p, b, err := launchBrief("Fix the login bug.\nThen open a PR.", now)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Count != 1 || b.Via != worker.ViaLaunch || !strings.Contains(p, b.Marker) {
		t.Fatalf("prompt %q, brief %+v", p, b)
	}
	if !strings.HasPrefix(p, "Fix the login bug.\nThen open a PR.\n\n") {
		t.Fatalf("prompt %q lost the brief's own lines", p)
	}
	if _, _, err := launchBrief("a\x1b[2Jb", now); !errors.Is(err, worker.ErrInvalidBrief) {
		t.Fatalf("an escape sequence passed: %v", err)
	}
}

// TestComposeLaunchBrief: the in-process brief is text only. A leading @ is
// not read as a path, and empty text is refused rather than meaning no brief.
func TestComposeLaunchBrief(t *testing.T) {
	now := func() time.Time { return time.Unix(100, 0) }
	if _, _, err := composeLaunchBrief("", now); !errors.Is(err, worker.ErrInvalidBrief) {
		t.Fatalf("empty text passed: %v", err)
	}
	p, b, err := composeLaunchBrief("@/etc/hosts", now)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || !strings.HasPrefix(p, "@/etc/hosts\n") {
		t.Fatalf("prompt %q: the @ text was not kept as text", p)
	}
}
