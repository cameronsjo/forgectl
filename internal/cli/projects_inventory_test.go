package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"
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
	err := withStatus(context.Background(), &buf, true, time.Hour, func() error {
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
	_ = withStatus(context.Background(), &buf, true, 10*time.Millisecond, func() error {
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
	if err := withStatus(context.Background(), &buf, false, time.Millisecond, func() error { return boom }); !errors.Is(err, boom) {
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

// TestWithStatus_CancelReturnsAtOnceAndErases: Ctrl+C during the wait must not
// wait for a host that is still sleeping, and must leave no status behind.
func TestWithStatus_CancelReturnsAtOnceAndErases(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	start := time.Now()
	err := withStatus(ctx, &buf, true, time.Hour, func() error { <-release; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("withStatus waited %v for work after the cancel", took)
	}
	if !strings.HasSuffix(buf.String(), eraseLine) {
		t.Errorf("a cancelled wait must erase its line, got %q", buf.String())
	}
}

// TestWithStatus_LinesFitFortyColumns: the erase clears one row, so a status
// line wider than a narrow terminal would leave its first row behind.
func TestWithStatus_LinesFitFortyColumns(t *testing.T) {
	for _, line := range []string{inventoryStatus, inventoryStatusSlow} {
		if w := ansi.StringWidth(line); w > 39 {
			t.Errorf("%q is %d cells; it must fit a 40-column terminal in one row", line, w)
		}
	}
}

// TestHeldNotes_BeforeFlushesThenActs pins the ordering the exec paths need: a
// verb that opens, clones, or adds a worktree may replace the process, so the
// notes must already be on stderr when the action starts.
func TestHeldNotes_BeforeFlushesThenActs(t *testing.T) {
	var errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errb)
	h := &heldNotes{cmd: cmd, notes: []string{"github: host query failed"}}

	var seen string
	boom := errors.New("act failed")
	err := h.before(func() error { seen = errb.String(); return boom })
	if seen != "note: github: host query failed\n" {
		t.Errorf("the action started with stderr = %q, want the note already printed", seen)
	}
	if !errors.Is(err, boom) {
		t.Errorf("before must return the action's error, got %v", err)
	}
	var nilHeld *heldNotes
	if err := nilHeld.before(func() error { return nil }); err != nil {
		t.Errorf("a nil heldNotes still runs the action, got %v", err)
	}
}

// TestLoadInventory_RedirectedStderrPrintsNotesAtOnce: with stderr not a
// terminal (2>log, | tee) there is no status line and no held notes; they go to
// the redirect as they always did.
func TestLoadInventory_RedirectedStderrPrintsNotesAtOnce(t *testing.T) {
	prevTTY, prevInv := isInteractiveTTY, inventoryFn
	isInteractiveTTY = func() bool { return true }
	inventoryFn = func(context.Context, *projects.Client) ([]projects.Repo, []string, error) {
		return []projects.Repo{{Name: "a"}}, []string{"github: host query failed"}, nil
	}
	t.Cleanup(func() { isInteractiveTTY, inventoryFn = prevTTY, prevInv })

	var errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errb)
	cmd.SetContext(context.Background())
	all, held, err := loadInventory(cmd, nil)
	if err != nil || len(all) != 1 {
		t.Fatalf("loadInventory = %v, %v", all, err)
	}
	if got := errb.String(); got != "note: github: host query failed\n" {
		t.Errorf("stderr = %q, want exactly the note, no status bytes", got)
	}
	if held.take() != nil {
		t.Error("notes printed at once must not also be held")
	}
}

// TestChooseRepo_PickerFailureGivesNotesBack: a picker that fails for a reason
// other than a cancel never showed the notes, so the caller's flush must.
func TestChooseRepo_PickerFailureGivesNotesBack(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickRepoFn
	isInteractiveTTY = func() bool { return true }
	pickRepoFn = func([]projects.Repo, theme.Theme, []string) (projects.Repo, error) {
		return projects.Repo{}, errors.New("could not open a new TTY")
	}
	t.Cleanup(func() { isInteractiveTTY, pickRepoFn = prevTTY, prevPicker })

	cmd := &cobra.Command{}
	held := &heldNotes{cmd: cmd, notes: []string{"n"}}
	if _, err := chooseRepo(cmd, nil, projectSelectionPick, theme.Theme{}, held); err == nil {
		t.Fatal("want the picker's error")
	}
	if len(held.notes) != 1 {
		t.Errorf("held = %v, want the note back", held.notes)
	}

	pickRepoFn = func([]projects.Repo, theme.Theme, []string) (projects.Repo, error) {
		return projects.Repo{}, huh.ErrUserAborted
	}
	held.notes = []string{"n"}
	_, _ = chooseRepo(cmd, nil, projectSelectionPick, theme.Theme{}, held)
	if len(held.notes) != 0 {
		t.Errorf("after a cancel the user saw the notes; held = %v", held.notes)
	}
}

// TestLoadInventoryTo_InteractiveHoldsNotesAndShowsStatus drives the branch the
// real terminal takes: the notes are held for the picker, not printed, and the
// status line comes and goes.
func TestLoadInventoryTo_InteractiveHoldsNotesAndShowsStatus(t *testing.T) {
	prev := inventoryFn
	inventoryFn = func(context.Context, *projects.Client) ([]projects.Repo, []string, error) {
		return []projects.Repo{{Name: "a"}}, []string{"github: host query failed"}, nil
	}
	t.Cleanup(func() { inventoryFn = prev })

	var errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	all, held, err := loadInventoryTo(cmd, nil, &errb, true)
	if err != nil || len(all) != 1 {
		t.Fatalf("loadInventoryTo = %v, %v", all, err)
	}
	if !strings.Contains(errb.String(), inventoryStatus) || !strings.HasSuffix(errb.String(), eraseLine) {
		t.Errorf("stderr = %q, want the status line, then an erase", errb.String())
	}
	if strings.Contains(errb.String(), "note:") {
		t.Errorf("held notes must not print around the picker, got %q", errb.String())
	}
	if got := held.take(); len(got) != 1 {
		t.Errorf("held = %v, want the note", got)
	}
}
