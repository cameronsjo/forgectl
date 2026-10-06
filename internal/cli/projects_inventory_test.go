package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/theme"
)

const eraseLine = "\r" + ansi.EraseEntireLine

// TestWithStatus_ShowsAtOnceThenErases pins #1104 problem 1: the screen was
// blank for the whole inventory query. The status must already be on screen
// when work starts, and must be gone when it returns.
func TestWithStatus_ShowsAtOnceThenErases(t *testing.T) {
	var buf bytes.Buffer
	var seenByWork string
	err := withStatus(&buf, true, time.Hour, func() error {
		seenByWork = buf.String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := eraseLine + inventoryStatus; seenByWork != want {
		t.Errorf("when work started the terminal held %q, want %q", seenByWork, want)
	}
	if !strings.HasSuffix(buf.String(), eraseLine) {
		t.Errorf("the status line must be erased when work returns, got %q", buf.String())
	}
	if strings.Contains(buf.String(), inventoryStatusSlow) {
		t.Error("a fast query must not claim to be slow")
	}
}

// TestWithStatus_SlowWaitSaysSo: a wait that does not end becomes an
// explanation (I7), not a line that never changes.
func TestWithStatus_SlowWaitSaysSo(t *testing.T) {
	var buf bytes.Buffer
	_ = withStatus(&buf, true, 10*time.Millisecond, func() error {
		time.Sleep(120 * time.Millisecond)
		return nil
	})
	if !strings.Contains(buf.String(), inventoryStatusSlow) {
		t.Errorf("a slow query must say it is still waiting, got %q", buf.String())
	}
	if !strings.HasSuffix(buf.String(), eraseLine) {
		t.Errorf("the slow line must be erased too, got %q", buf.String())
	}
}

func TestWithStatus_NoTerminalWritesNothingAndKeepsTheError(t *testing.T) {
	var buf bytes.Buffer
	boom := errors.New("boom")
	if err := withStatus(&buf, false, time.Millisecond, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the work's error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("no terminal must mean no status bytes, got %q", buf.String())
	}
}

func TestHeldNotes_TakeAndFlush(t *testing.T) {
	var errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errb)
	h := &heldNotes{cmd: cmd, notes: []string{"github: host query failed", "gitea: host query failed"}}

	if got := h.take(); len(got) != 2 {
		t.Fatalf("take() = %v", got)
	}
	h.flush()
	if errb.Len() != 0 {
		t.Errorf("flush after take must print nothing, got %q", errb.String())
	}

	h.notes = []string{"github: host query failed"}
	h.flush()
	if got := errb.String(); got != "note: github: host query failed\n" {
		t.Errorf("flush = %q", got)
	}
	h.flush()
	if strings.Count(errb.String(), "note:") != 1 {
		t.Errorf("flush must print once, got %q", errb.String())
	}

	var nilHeld *heldNotes
	nilHeld.flush()
	if nilHeld.take() != nil {
		t.Error("a nil heldNotes holds nothing")
	}
}

// TestChooseRepo_InteractiveHandsHeldNotesToThePicker pins #1104 problem 2:
// notes used to print on the terminal around the form. On the picker path they
// go into the picker, and nothing is written to stderr.
func TestChooseRepo_InteractiveHandsHeldNotesToThePicker(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickRepoFn
	isInteractiveTTY = func() bool { return true }
	var got []string
	pickRepoFn = func(_ []projects.Repo, _ theme.Theme, notes []string) (projects.Repo, error) {
		got = notes
		return projects.Repo{Name: "x"}, nil
	}
	t.Cleanup(func() { isInteractiveTTY, pickRepoFn = prevTTY, prevPicker })

	var errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errb)
	held := &heldNotes{cmd: cmd, notes: []string{"github: host query failed"}}
	if _, err := chooseRepo(cmd, []projects.Repo{{Name: "a"}, {Name: "b"}}, projectSelectionPick, theme.Theme{}, held); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "github: host query failed" {
		t.Errorf("picker notes = %v", got)
	}
	held.flush()
	if errb.Len() != 0 {
		t.Errorf("notes the picker took must not print again, got %q", errb.String())
	}
}

// TestDegradationNotesBlock_IsEscaped: a note built from a host's own text
// reaches the picker's description through the same boundary as stderr.
func TestDegradationNotesBlock_IsEscaped(t *testing.T) {
	got := degradationNotesBlock([]string{"local: bad \x1b[2J path", "github: host query failed"})
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("an escape reached the description: %q", got)
	}
	if lines := strings.Split(got, "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "note: ") {
		t.Errorf("block = %q", got)
	}
}
