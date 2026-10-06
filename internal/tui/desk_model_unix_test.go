//go:build unix

package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// fakeLaunch is a real desk whose Launch only records the claimed name: a
// claimed item stays in running/ and no supervisor process is started.
type fakeLaunch struct {
	*desk.Desk
	launched []string
}

func (f *fakeLaunch) Launch(c *desk.Claimed) (int, error) {
	f.launched = append(f.launched, c.Name)
	return 0, nil
}

type notice struct{ title, body string }

// deskHarness is a model over a fresh desk, driven by hand: commands run
// synchronously and their messages are fed straight back.
type deskHarness struct {
	t       *testing.T
	d       *desk.Desk
	backend *fakeLaunch
	m       deskModel
	clock   *fakeClock
	bells   int
	notes   []notice
}

func newDeskHarness(t *testing.T) *deskHarness {
	t.Helper()
	d, err := desk.Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatalf("desk.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	h := &deskHarness{t: t, d: d, backend: &fakeLaunch{Desk: d}, clock: &fakeClock{t: deskNow}}
	m := newDeskModel(context.Background(), h.backend, DeskOptions{
		Version: "test", Host: "host-a", Home: "/nonexistent", Theme: theme.Default(),
		Notify: func(_ context.Context, title, body string) error {
			h.notes = append(h.notes, notice{title, body})
			return nil
		},
	})
	m.now = h.clock.now
	m.width, m.height = 100, 40
	h.m = m
	h.scan()
	return h
}

// drive runs cmd and everything it leads to, except tea.Exec (which needs a
// real program) and timers (never started here).
func (h *deskHarness) drive(cmd tea.Cmd) {
	h.t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.RawMsg:
			if msg.Msg == "\a" {
				h.bells++
			}
		default:
			out, next := h.m.Update(msg)
			h.m = out.(deskModel)
			queue = append(queue, next)
		}
	}
}

func (h *deskHarness) scan() { h.t.Helper(); h.m.scanning = false; h.drive(h.m.scanCmd()) }

func (h *deskHarness) press(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "space":
			msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
		default:
			msg = key(k)
		}
		out, cmd := h.m.Update(msg)
		h.m = out.(deskModel)
		h.drive(cmd)
	}
}

// drop hand-drops a pending item, the way Claude queues one.
func (h *deskHarness) drop(file, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.d.Path(), desk.DirPending, file), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *deskHarness) where(name string) string {
	h.t.Helper()
	for _, sub := range []string{desk.DirPending, desk.DirRunning, desk.DirSkipped, desk.DirDone} {
		for _, ext := range []string{".sh", ".manifest"} {
			if _, err := os.Lstat(filepath.Join(h.d.Path(), sub, name+ext)); err == nil {
				return sub
			}
		}
	}
	return "nowhere"
}

func (h *deskHarness) selectItem(name string) {
	h.t.Helper()
	for i, r := range h.m.rows {
		if r.item.Name == name {
			h.m.cursor = i
			return
		}
	}
	h.t.Fatalf("%s is not in the queue", name)
}

func plainScript(what string) string {
	return "#!/bin/bash\n# WHAT: " + what + "\n# WHY: a test\necho " + what + "\n"
}

func ttyScript(what string) string {
	return "#!/bin/bash\n# WHAT: " + what + "\n# WHY: a test\n# TTY: yes\necho " + what + "\n"
}

func TestDesk_YRunsTheSelectedItemDetached(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	h.press("y")
	if !slices.Equal(h.backend.launched, []string{"01-alpha"}) {
		t.Fatalf("launched %v, want [01-alpha]", h.backend.launched)
	}
	if got := h.where("01-alpha"); got != desk.DirRunning {
		t.Errorf("01-alpha is in %s, want running (claimed)", got)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "started 01 alpha") {
		t.Errorf("footer = %q", ansi.Strip(h.m.footer()))
	}
}

// a, which runs several items at once, asks first and lists each with its
// full sha256; at a narrow width each hash wraps but stays whole.
func TestDesk_AConfirmsWithFullHashes(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.drop("02-beta.sh", plainScript("beta"))
	h.scan()
	var shas []string
	for _, r := range h.m.rows {
		if !desk.ValidSHA256(r.item.Meta.SHA256) {
			t.Fatalf("no hash on %s: %q", r.item.Name, r.item.Meta.SHA256)
		}
		shas = append(shas, r.item.Meta.SHA256)
	}
	for _, width := range []int{120, 50} {
		h.m.width = width
		h.press("a")
		prompt := ansi.Strip(h.m.footer())
		flat := strings.NewReplacer(" ", "", "\n", "").Replace(prompt)
		for _, sha := range shas {
			if !strings.Contains(flat, sha) {
				t.Errorf("width %d: the a prompt lacks the full hash %s: %q", width, sha, prompt)
			}
		}
		h.press("n")
	}
	if len(h.backend.launched) != 0 {
		t.Errorf("a declined a ran %v", h.backend.launched)
	}
}

// y runs at once, with one key: the focus panel's short hash is the check.
func TestDesk_YRunsWithOneKey(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	h.press("y")
	if h.m.confirm != confirmNone || !slices.Equal(h.backend.launched, []string{"01-alpha"}) {
		t.Fatalf("y did not run at once: confirm %v launched %v", h.m.confirm, h.backend.launched)
	}
}

func TestDesk_YRefusesAnItemThatChangedSinceItWasShown(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	h.drop("01-alpha.sh", plainScript("alpha edited"))
	h.press("y")
	if len(h.backend.launched) != 0 {
		t.Fatalf("launched %v; a changed item must not run", h.backend.launched)
	}
	if got := h.where("01-alpha"); got != desk.DirSkipped {
		t.Errorf("01-alpha is in %s, want skipped (changed)", got)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "changed") {
		t.Errorf("footer should say it changed: %q", ansi.Strip(h.m.footer()))
	}
}

func TestDesk_SkipAsksFirstAndUUndoes(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()

	h.press("s")
	if h.m.confirm != confirmSkip || !strings.Contains(ansi.Strip(h.m.footer()), "skip 01 alpha?") {
		t.Fatalf("s should ask first; footer = %q", ansi.Strip(h.m.footer()))
	}
	h.press("n")
	if got := h.where("01-alpha"); got != desk.DirPending {
		t.Fatalf("a declined skip moved the item to %s", got)
	}

	h.press("s", "y")
	if got := h.where("01-alpha"); got != desk.DirSkipped {
		t.Fatalf("after s y the item is in %s, want skipped", got)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "u to undo") {
		t.Errorf("footer = %q", ansi.Strip(h.m.footer()))
	}

	bellsBefore := h.bells
	h.press("u")
	if got := h.where("01-alpha"); got != desk.DirPending {
		t.Fatalf("after u the item is in %s, want pending", got)
	}
	h.press("u")
	if !strings.Contains(ansi.Strip(h.m.footer()), "nothing to undo") {
		t.Errorf("a second u should have nothing to undo: %q", ansi.Strip(h.m.footer()))
	}
	h.press("j")
	if h.m.footer() != "" {
		t.Errorf("the next key should clear the result and bring back the hints: %q", ansi.Strip(h.m.footer()))
	}
	if h.bells != bellsBefore {
		t.Errorf("an undone skip rang %d more bells; it is not an arrival", h.bells-bellsBefore)
	}
}

// TestDesk_ARunsExactlyWhatWasOnScreen: a captures the runnable items and
// their hashes when it is pressed. TTY items are left out; an item that
// changes before the confirm is not run; one that arrives after is not in
// the set.
func TestDesk_ARunsExactlyWhatWasOnScreen(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.drop("02-console.sh", ttyScript("console"))
	h.drop("03-gamma.sh", plainScript("gamma"))
	h.drop("04-delta.manifest", "# WHAT: delta\n# WHY: a test\none -- echo one\n")
	h.scan()

	h.press("a")
	prompt := ansi.Strip(h.m.footer())
	if h.m.confirm != confirmAll || !strings.Contains(prompt, "run these 3?") ||
		!strings.Contains(prompt, "01 alpha") || strings.Contains(prompt, "02 console") || !strings.Contains(prompt, "04 delta") {
		t.Fatalf("a should ask about exactly the three non-TTY items; footer = %q", prompt)
	}

	h.drop("03-gamma.sh", plainScript("gamma edited")) // changed after the screen was drawn
	h.drop("05-late.sh", plainScript("late"))          // arrived after
	h.scan()                                           // the screen moves on; the captured set must not
	h.press("y")

	if !slices.Equal(h.backend.launched, []string{"01-alpha", "04-delta"}) {
		t.Fatalf("launched %v, want [01-alpha 04-delta]", h.backend.launched)
	}
	for name, want := range map[string]string{
		"02-console": desk.DirPending, "03-gamma": desk.DirSkipped, "05-late": desk.DirPending,
	} {
		if got := h.where(name); got != want {
			t.Errorf("%s is in %s, want %s", name, got, want)
		}
	}
	if footer := ansi.Strip(h.m.footer()); !strings.Contains(footer, "started 2 · 1 no longer waiting") {
		t.Errorf("footer = %q", footer)
	}
}

// TestDesk_AReportsAnItemThatChangedBeforeTheConfirm: with no rescan in
// between, the changed item reaches Claim, which refuses it at the hash on
// screen and moves it to skipped/.
func TestDesk_AReportsAnItemThatChangedBeforeTheConfirm(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.drop("02-beta.sh", plainScript("beta"))
	h.scan()
	h.press("a")
	h.drop("02-beta.sh", plainScript("beta edited"))
	h.press("y")
	if !slices.Equal(h.backend.launched, []string{"01-alpha"}) {
		t.Fatalf("launched %v, want [01-alpha]", h.backend.launched)
	}
	if got := h.where("02-beta"); got != desk.DirSkipped {
		t.Errorf("02-beta is in %s, want skipped", got)
	}
	if footer := ansi.Strip(h.m.footer()); !strings.Contains(footer, "started 1 · 1 changed, not run") {
		t.Errorf("footer = %q", footer)
	}
}

func TestDesk_ADeclinedRunsNothing(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	h.press("a", "esc")
	if len(h.backend.launched) != 0 || h.where("01-alpha") != desk.DirPending {
		t.Fatalf("a declined run-all still ran: %v", h.backend.launched)
	}
}

func TestDesk_AWithOnlyTTYItemsSaysSo(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-console.sh", ttyScript("console"))
	h.scan()
	h.press("a")
	if h.m.confirm != confirmNone || !strings.Contains(ansi.Strip(h.m.footer()), "nothing to run") {
		t.Fatalf("footer = %q", ansi.Strip(h.m.footer()))
	}
}

func TestDesk_JKMoveAndClamp(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.drop("02-beta.sh", plainScript("beta"))
	h.scan()
	h.press("k")
	if h.m.cursor != 0 {
		t.Fatalf("k at the top moved to %d", h.m.cursor)
	}
	h.press("j", "j", "j")
	if h.m.cursor != 1 {
		t.Fatalf("j past the end: cursor %d, want 1", h.m.cursor)
	}
	h.press("up")
	if h.m.cursor != 0 {
		t.Fatalf("up: cursor %d", h.m.cursor)
	}
	h.press("down")
	if h.m.cursor != 1 {
		t.Fatalf("down: cursor %d", h.m.cursor)
	}
	// A rescan keeps the cursor on the same item even when one is added above it.
	h.drop("00-first.sh", plainScript("first"))
	h.scan()
	if r, _ := h.m.selected(); r.item.Name != "02-beta" {
		t.Errorf("after a rescan the cursor is on %s, want 02-beta", r.item.Name)
	}
}

func TestDesk_QQuits(t *testing.T) {
	h := newDeskHarness(t)
	_, cmd := h.m.Update(key("q"))
	if !isQuit(cmd) {
		t.Fatal("q did not quit")
	}
}

func TestDesk_VShowsTheHashedBytesAndQCloses(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	h.press("v")
	if h.m.pager == nil {
		t.Fatal("v opened no pager")
	}
	view := ansi.Strip(h.m.View().Content)
	if !strings.Contains(view, "script · 01 alpha") || !strings.Contains(view, "echo alpha") {
		t.Errorf("pager:\n%s", view)
	}
	h.press("q")
	if h.m.pager != nil {
		t.Fatal("q did not close the pager")
	}
	if _, cmd := h.m.Update(key("q")); !isQuit(cmd) {
		t.Fatal("with the pager closed, q quits")
	}
}

func TestDesk_LShowsTheLatestLogSanitized(t *testing.T) {
	h := newDeskHarness(t)
	h.press("l")
	if !strings.Contains(ansi.Strip(h.m.footer()), "no log yet") {
		t.Fatalf("l with nothing run: footer = %q", ansi.Strip(h.m.footer()))
	}
	done := filepath.Join(h.d.Path(), desk.DirDone)
	if err := os.WriteFile(filepath.Join(done, "07-old.sh"), []byte(plainScript("old")), 0o600); err != nil {
		t.Fatal(err)
	}
	log := "start\n" + termsafetest.Hostile("payload") + "\nprogress 10%\rprogress 100%\n\x1b[32mgreen\x1b[0m\nEXIT=0\n"
	if err := os.WriteFile(filepath.Join(done, "07-old.log"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	h.scan()
	h.press("l")
	if h.m.pager == nil {
		t.Fatalf("l opened no pager; footer = %q", ansi.Strip(h.m.footer()))
	}
	view := h.m.View().Content
	termsafetest.AssertInert(t, "log pager", view)
	plainView := ansi.Strip(view)
	for _, want := range []string{"log · 07 old", "start", "payload", "progress 100%", "green", "EXIT=0"} {
		if !strings.Contains(plainView, want) {
			t.Errorf("pager lacks %q:\n%s", want, plainView)
		}
	}
	if strings.Contains(plainView, "progress 10%") {
		t.Error("a carriage-return rewrite should show only its last version")
	}
}

func TestDesk_PagerScrolls(t *testing.T) {
	h := newDeskHarness(t)
	h.m.height = 10
	var b strings.Builder
	b.WriteString("# WHAT: long\n# WHY: a test\n")
	for i := range 50 {
		b.WriteString("echo line" + strings.Repeat("x", i%3) + "\n")
	}
	h.drop("01-long.sh", b.String())
	h.scan()
	h.press("v", "j", "j")
	if h.m.pager.offset != 2 {
		t.Fatalf("offset %d after j j", h.m.pager.offset)
	}
	h.press("G")
	if want := 52 - 8; h.m.pager.offset != want {
		t.Fatalf("G: offset %d, want %d", h.m.pager.offset, want)
	}
	h.press("g", "k")
	if h.m.pager.offset != 0 {
		t.Fatalf("g k: offset %d", h.m.pager.offset)
	}
}

func TestDesk_BellAndNotificationOnArrivalAndEveryFiveMinutes(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	if h.bells != 1 || len(h.notes) != 1 {
		t.Fatalf("an arrival rang %d bells and sent %d notifications, want 1 and 1", h.bells, len(h.notes))
	}
	if h.notes[0] != (notice{"desk: 1 waiting", "01 alpha: alpha"}) {
		t.Errorf("notification = %+v", h.notes[0])
	}
	h.clock.advance(4 * time.Minute)
	h.scan()
	if h.bells != 1 {
		t.Fatalf("rang again after 4m: %d bells", h.bells)
	}
	h.clock.advance(time.Minute)
	h.scan()
	if h.bells != 2 || h.notes[1].body != "still waiting for you" {
		t.Fatalf("after 5m: %d bells, notes %+v", h.bells, h.notes)
	}
	h.press("s", "y")
	h.clock.advance(10 * time.Minute)
	h.scan()
	if h.bells != 2 {
		t.Fatalf("rang with nothing waiting: %d bells", h.bells)
	}
}

func TestDesk_NoBellForWhatWasWaitingAtStart(t *testing.T) {
	d, err := desk.Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := os.WriteFile(filepath.Join(d.Path(), desk.DirPending, "01-alpha.sh"), []byte(plainScript("alpha")), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &deskHarness{t: t, d: d, backend: &fakeLaunch{Desk: d}, clock: &fakeClock{t: deskNow}}
	h.m = newDeskModel(context.Background(), h.backend, DeskOptions{Theme: theme.Default(), Notify: func(context.Context, string, string) error {
		h.notes = append(h.notes, notice{})
		return nil
	}})
	h.m.now = h.clock.now
	h.scan()
	if h.bells != 0 || len(h.notes) != 0 {
		t.Fatalf("the first scan rang for items already waiting: %d bells", h.bells)
	}
}

func TestDesk_WindowTitleCountsWaiting(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.drop("02-beta.sh", plainScript("beta"))
	h.scan()
	if got := h.m.View().WindowTitle; got != "desk ● 2 waiting" {
		t.Errorf("window title = %q", got)
	}
}

func TestDeskNotification_IsInert(t *testing.T) {
	it := desk.Item{Name: termsafetest.Hostile("01-x"), Headers: desk.Headers{What: termsafetest.Hostile("what")}}
	title, body := deskNotification(3, it)
	termsafetest.AssertInert(t, "notification title", title)
	termsafetest.AssertInert(t, "notification body", body)
	if !strings.Contains(body, "what") {
		t.Fatalf("body lost the WHAT: %q", body)
	}
	if deskNotifyMaxRunes != herdr.NotificationMaxRunes {
		t.Errorf("the desk caps a notification at %d runes and herdr at %d; keep them equal", deskNotifyMaxRunes, herdr.NotificationMaxRunes)
	}
	long := desk.Item{Name: "01-x", Headers: desk.Headers{What: strings.Repeat("w", 1000)}}
	if _, b := deskNotification(1, long); len([]rune(b)) > 260 {
		t.Errorf("body is %d runes; it should be capped", len([]rune(b)))
	}
}

// TestDesk_ModelDrawsNothingUnsafe: hostile WHAT, WHY, script lines and an
// error quoting them, through the live model's view and the v pager.
func TestDesk_ModelDrawsNothingUnsafe(t *testing.T) {
	h := newDeskHarness(t)
	hostile := termsafetest.Hostile
	h.drop("01-x.sh", "# WHAT: "+hostile("what")+"\n# WHY: "+hostile("why")+"\n"+hostile("echo")+"\n")
	h.scan()
	termsafetest.AssertInert(t, "desk view", h.m.View().Content)
	h.press("v")
	termsafetest.AssertInert(t, "script pager", h.m.View().Content)
	h.press("q")
	h.m.message = h.m.resultLine("", errors.New(hostile("failure")))
	termsafetest.AssertInert(t, "error footer", h.m.View().Content)
}

func TestRunDesk_RefusesWithoutATerminal(t *testing.T) {
	d, err := desk.Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	// go test's stdin is not a terminal.
	if err := RunDesk(context.Background(), d, DeskOptions{Theme: theme.Default()}); !errors.Is(err, ErrDeskNeedsTerminal) {
		t.Fatalf("RunDesk = %v, want ErrDeskNeedsTerminal", err)
	}
}

// --- the TTY path ---

// runTTY presses y on a TTY item and runs the resulting command the way
// tea.Exec would, with stdin from /dev/null and the terminal's output
// captured. It returns the rc and the done/ log.
func runTTY(t *testing.T, h *deskHarness, name string) (int, string) {
	t.Helper()
	h.selectItem(name)
	out, cmd := h.m.Update(key("y"))
	h.m = out.(deskModel)
	if cmd == nil {
		t.Fatal("y on a TTY item returned no command")
	}
	ready, ok := cmd().(deskTTYReadyMsg)
	if !ok || ready.err != nil {
		t.Fatalf("y on a TTY item: %+v", ready)
	}
	if got := h.where(name); got != desk.DirRunning {
		t.Fatalf("%s is in %s before the run, want running", name, got)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close() //nolint:errcheck // read-only
	var term bytes.Buffer
	ready.run.SetStdin(devnull)
	ready.run.SetStdout(&term)
	ready.run.SetStderr(&term)
	runErr := ready.run.Run()
	out2, _ := h.m.Update(deskTTYDoneMsg{run: ready.run, err: runErr})
	h.m = out2.(deskModel)
	snap, err := h.d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range snap.Done {
		if it.Name == name {
			if it.ExitCode == nil {
				t.Fatalf("%s finished with no exit code", name)
			}
			data, err := os.ReadFile(h.d.LogPath(name))
			if err != nil {
				t.Fatal(err)
			}
			return *it.ExitCode, string(data)
		}
	}
	t.Fatalf("%s is not in done/ after its run (it is in %s); footer %q", name, h.where(name), ansi.Strip(h.m.footer()))
	return 0, ""
}

// TestTTYRun_RunsTheVerifiedBytesNotTheRecord: the item appends a line to its
// own running/ record and to $0 while it runs. Neither line may run: bash
// reads the verified bytes from fd 3, not the record. A child reading fd 3
// finds it closed (the desk's prelude), so it cannot swallow the rest.
func TestTTYRun_RunsTheVerifiedBytesNotTheRecord(t *testing.T) {
	h := newDeskHarness(t)
	record := filepath.Join(h.d.Path(), desk.DirRunning, "01-console.sh")
	h.drop("01-console.sh", "#!/bin/bash\n# WHAT: console\n# WHY: a test\n# TTY: yes\n"+
		"echo first\n"+
		"printf 'echo INJECTED-RECORD\\n' >> '"+record+"'\n"+
		"echo 'echo INJECTED-SELF' >> \"$0\" 2>/dev/null || echo self-append-refused\n"+
		"head -c 64 <&3 >/dev/null 2>&1 || echo fd3-closed\n"+
		"echo last\n"+
		// No `exit` here: a bash reading the record by name would go on to
		// the appended lines, so the test can see it. The rc is the last
		// command's.
		"bash -c 'exit 7'\n")
	h.scan()
	rc, log := runTTY(t, h, "01-console")
	if rc != 7 {
		t.Errorf("rc = %d, want 7 (from the rc file)\nlog:\n%s", rc, log)
	}
	for _, want := range []string{"first", "fd3-closed", "last", "EXIT=7"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	// bash never echoes a command, so INJECTED reaches the log only if an
	// appended line ran.
	if strings.Contains(log, "INJECTED") {
		t.Errorf("an appended line ran:\n%s", log)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "01 console ended: exit 7") {
		t.Errorf("footer = %q", ansi.Strip(h.m.footer()))
	}
}

// TestTTYRunPassesFD3 is the platform check for ttyArgv: script(1) must
// hand fd 3 through to bash, or a TTY item runs nothing, and must write its
// typescript to fd 4, the log the desk created exclusively through its root.
// It runs the real script(1) on the platform under test.
func TestTTYRunPassesFD3(t *testing.T) {
	if _, err := os.Stat("/usr/bin/script"); err != nil {
		t.Skip("no /usr/bin/script on this machine")
	}
	dir := t.TempDir()
	logPath, rcPath := filepath.Join(dir, "log"), filepath.Join(dir, "rc")
	logF, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600) //nolint:gosec // G304: a path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	a := ttyArgv(rcPath)
	if !slices.Contains(a, ttyLogFD) {
		t.Fatalf("argv %q does not give script(1) the log on fd 4", a)
	}
	cmd := newCmd(a)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close() //nolint:errcheck // read-only
	cmd.Stdin = devnull
	// The item checks fd 4 (the log) is closed, and a child tries to write
	// into it; neither may reach the log except through script(1).
	release, err := desk.AttachScript(cmd, []byte("echo fd3-reached-bash\n"+
		"if [ -e /dev/fd/4 ]; then echo FD4-OPEN-IN-ITEM; else echo fd4-closed-in-item; fi\n"+
		"/bin/sh -c 'echo CHILD-WROTE-FD4 >&4' 2>/dev/null || true\n"+
		"exit 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, logF)
	err = cmd.Start()
	release()
	_ = logF.Close()
	if err != nil {
		t.Fatalf("start %v: %v", a, err)
	}
	_ = cmd.Wait()                     // script(1)'s own status is not the item's; the rc file is
	logData, _ := os.ReadFile(logPath) //nolint:gosec // G304: a path under t.TempDir
	rcData, _ := os.ReadFile(rcPath)   //nolint:gosec // G304: a path under t.TempDir
	if !strings.Contains(string(logData), "fd3-reached-bash") {
		t.Fatalf("the log on fd 4 lacks the script's output: bash did not read fd 3, or script(1) did not write fd 4\nargv %q\nlog %q", a, logData)
	}
	if strings.TrimSpace(string(rcData)) != "3" {
		t.Fatalf("rc file = %q, want 3", rcData)
	}
	if !strings.Contains(string(logData), "fd4-closed-in-item") || strings.Contains(string(logData), "FD4-OPEN-IN-ITEM") || strings.Contains(string(logData), "CHILD-WROTE-FD4") {
		t.Fatalf("fd 4 (the log) reached the item or its child:\n%s", logData)
	}
}

// TestTTYRun_LogIsCreatedExclusively: a log already at done/<name>.log (a
// planted symlink, or another run's) stops the run before script(1) starts,
// rather than script(1) truncating whatever it names.
func TestTTYRun_LogIsCreatedExclusively(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-console.sh", ttyScript("console"))
	h.scan()
	h.selectItem("01-console")
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// BeginRun's doneTaken check passes; the symlink lands between it and the
	// log's creation, the window the review described.
	h.m.ttyArgv = func(string) []string { t.Fatal("script(1) was started"); return nil }
	backend := &symlinkOnBegin{fakeLaunch: h.backend, link: filepath.Join(h.d.Path(), desk.DirDone, "01-console.log"), target: target}
	h.m.d = backend
	h.press("y")
	if data, _ := os.ReadFile(target); string(data) != "keep me\n" { //nolint:gosec // G304: a path under t.TempDir
		t.Fatalf("the symlink's target was written: %q", data)
	}
	if footer := ansi.Strip(h.m.footer()); !strings.Contains(footer, "create log for 01-console") {
		t.Errorf("footer should report the refused log: %q", footer)
	}
	// The run ends in skipped/ as name-reused; done/ keeps what is there.
	snap, err := h.d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Meta.SkipReason != desk.SkipReused {
		t.Fatalf("skipped = %+v, want 01-console as name-reused", snap.Skipped)
	}
	if fi, err := os.Lstat(backend.link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the planted log entry was touched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.d.Path(), desk.DirDone, "01-console.sh")); err == nil {
		t.Fatal("the run's record was moved into done/")
	}
}

// symlinkOnBegin plants a symlink at the log path right after BeginRun.
type symlinkOnBegin struct {
	*fakeLaunch
	link, target string
}

func (s *symlinkOnBegin) BeginRun(name string, pid int, fields ...string) (*desk.Run, error) {
	run, err := s.fakeLaunch.BeginRun(name, pid, fields...)
	if err == nil {
		if lerr := os.Symlink(s.target, s.link); lerr != nil {
			panic(lerr)
		}
	}
	return run, err
}

func newCmd(a []string) *osexec.Cmd {
	return osexec.CommandContext(context.Background(), a[0], a[1:]...) //nolint:gosec // G204: ttyArgv's fixed argv
}

// TestTTYRun_BeginRunRefusalReleasesTheClaim: a reused number (done/ already
// holds the name) makes BeginRun refuse after Claim succeeded. The claim
// must end in skipped/ (launch-failed), not sit ownerless in running/.
func TestTTYRun_BeginRunRefusalReleasesTheClaim(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-console.sh", ttyScript("console"))
	if err := os.WriteFile(filepath.Join(h.d.Path(), desk.DirDone, "01-console.log"), []byte("EXIT=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.scan()
	h.selectItem("01-console")
	h.press("y")
	if got := h.where("01-console"); got != desk.DirSkipped {
		t.Fatalf("01-console is in %s, want skipped", got)
	}
	snap, err := h.d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Meta.SkipReason != desk.SkipLaunchFailed {
		t.Fatalf("skipped = %+v, want one launch-failed item", snap.Skipped)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "done/ already holds") {
		t.Errorf("footer should say why: %q", ansi.Strip(h.m.footer()))
	}
}

// TestDesk_SkippingALostRunIsFinal: s on a lost row records the run as lost,
// and u cannot bring it back to pending/.
func TestDesk_SkippingALostRunIsFinal(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-alpha.sh", plainScript("alpha"))
	h.scan()
	h.press("y") // claimed; the fake Launch starts no owner
	// Age the claim past the grace: the claim's owner never came.
	metaPath := filepath.Join(h.d.Path(), desk.DirRunning, "01-alpha.meta.json")
	data, err := os.ReadFile(metaPath) //nolint:gosec // G304: a path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var meta desk.Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	meta.ClaimedAt = &old
	if data, err = json.Marshal(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	h.scan()
	h.selectItem("01-alpha")
	if r, _ := h.m.selected(); r.kind != rowLost {
		t.Fatalf("row kind %d, want lost", r.kind)
	}
	h.press("s", "y")
	if got := h.where("01-alpha"); got != desk.DirSkipped {
		t.Fatalf("01-alpha is in %s, want skipped", got)
	}
	h.press("u")
	if got := h.where("01-alpha"); got != desk.DirSkipped {
		t.Fatalf("u re-armed a lost run: it is in %s", got)
	}
	snap, err := h.d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Skipped[0].Meta.SkipReason != desk.SkipLost {
		t.Errorf("skip reason %q, want %q", snap.Skipped[0].Meta.SkipReason, desk.SkipLost)
	}
}

// TestDesk_PagerShowsALongLineWhole: v soft-wraps a line wider than the
// window; every character of it is on screen.
func TestDesk_PagerShowsALongLineWhole(t *testing.T) {
	h := newDeskHarness(t)
	h.m.width, h.m.height = 80, 30
	var b strings.Builder
	for i := 0; b.Len() < 290; i++ {
		b.WriteString("seg" + strconv.Itoa(i) + "-")
	}
	long := "echo " + b.String() + "END-OF-LINE"
	if len(long) < 300 {
		t.Fatalf("fixture line is %d columns", len(long))
	}
	h.drop("01-long.sh", "# WHAT: long\n# WHY: a test\n"+long+"\n")
	h.scan()
	h.press("v")
	view := ansi.Strip(h.m.View().Content)
	var joined strings.Builder
	for _, row := range strings.Split(view, "\n") {
		if ansi.StringWidth(row) > 80 {
			t.Errorf("a row is wider than the window: %q", row)
		}
		joined.WriteString(strings.TrimPrefix(row, pagerWrapMark))
	}
	if !strings.Contains(joined.String(), long) {
		t.Fatalf("the long line is not fully on screen:\n%s", view)
	}
}

// hangingScan never answers a scan, like a read blocked on a FIFO.
type hangingScan struct{ *fakeLaunch }

func (hangingScan) Scan() (*desk.Snapshot, error) { select {} }

// TestDesk_HungScanReportsAndRetries: a scan that never answers is reported
// as an error, and the scan flag clears so the next tick scans again.
func TestDesk_HungScanReportsAndRetries(t *testing.T) {
	h := newDeskHarness(t)
	h.m.d = hangingScan{h.backend}
	h.m.scanTimeout = 50 * time.Millisecond
	h.m.scanning = true
	h.drive(h.m.scanCmd())
	if h.m.scanning {
		t.Fatal("a timed-out scan left the scanning flag set; no scan would run again")
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "did not finish") {
		t.Fatalf("footer = %q", ansi.Strip(h.m.footer()))
	}
}

// TestTTYRunStartsInHomeWithACleanEnvironment: a TTY item starts where a
// detached one does, in $HOME, with bash's startup hooks removed.
func TestTTYRunStartsInHomeWithACleanEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASH_ENV", filepath.Join(home, "hook.sh"))
	h := newDeskHarness(t)
	h.drop("01-console.sh", ttyScript("console"))
	h.scan()
	snap, err := h.d.Scan()
	if err != nil || len(snap.Pending) != 1 {
		t.Fatalf("scan: %v, %d pending", err, len(snap.Pending))
	}
	c, err := h.backend.Claim("01-console", snap.Pending[0].Meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	rcPath := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rcPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := newTTYRun(h.backend, c, rcPath, func(string) []string { return []string{"/bin/true"} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.finish(nil) })
	if r.cmd.Dir != home {
		t.Errorf("dir = %q, want %q", r.cmd.Dir, home)
	}
	if r.cmd.Env == nil || slices.ContainsFunc(r.cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "BASH_ENV=") }) {
		t.Errorf("env = %q; want an explicit environment without BASH_ENV", r.cmd.Env)
	}
}

// TestAManifestWithATTYLineRunsDetached: "# TTY: yes" in a manifest does
// not make it a TTY item. y runs it detached, as a batch; script(1) never
// starts, so bash never reads the manifest as a script.
func TestAManifestWithATTYLineRunsDetached(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-m.manifest", "# WHAT: x\n# WHY: y\n# TTY: yes\na -- true\n")
	h.scan()
	h.selectItem("01-m")
	h.m.ttyArgv = func(string) []string { t.Fatal("script(1) was started for a manifest"); return nil }
	h.press("y")
	if !slices.Contains(h.backend.launched, "01-m") {
		t.Fatalf("launched = %q, want 01-m started detached", h.backend.launched)
	}
}

// TestTTYRunRefusesABatch: the terminal path runs only a TTY script, and
// ends any other claim in skipped/.
func TestTTYRunRefusesABatch(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-m.manifest", "# WHAT: x\n# WHY: y\na -- true\n")
	h.scan()
	snap, err := h.d.Scan()
	if err != nil || len(snap.Pending) != 1 {
		t.Fatalf("scan: %v, %d pending", err, len(snap.Pending))
	}
	c, err := h.backend.Claim("01-m", snap.Pending[0].Meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	c.TTY = true // as an older reader that trusted the manifest's header would have it
	rcPath := filepath.Join(t.TempDir(), "rc")
	if _, err := newTTYRun(h.backend, c, rcPath, func(string) []string { t.Fatal("argv built for a batch"); return nil }); err == nil {
		t.Fatal("newTTYRun accepted a batch")
	}
	if got := h.where("01-m"); got != desk.DirSkipped {
		t.Fatalf("01-m is in %s, want skipped", got)
	}
}

// TestDesk_YRefusesWhenTheFocusPanelIsNotOnScreen is #1098: in a window too
// small to show the selected item's sha256, what and why, y runs nothing and
// says why. The same item runs once the window can show them.
func TestDesk_YRefusesWhenTheFocusPanelIsNotOnScreen(t *testing.T) {
	for _, size := range [][2]int{{100, 7}, {80, 9}, {30, 8}} {
		h := newDeskHarness(t)
		h.drop("01-s1.sh", plainScript("s1"))
		h.drop("02-s2.sh", plainScript("s2"))
		h.scan()
		h.m.width, h.m.height = size[0], size[1]
		h.press("y")
		if len(h.backend.launched) != 0 || h.where("01-s1") != desk.DirPending {
			t.Fatalf("%dx%d: y ran %v with the focus panel off screen", size[0], size[1], h.backend.launched)
		}
		if footer := ansi.Strip(h.m.footer()); !strings.Contains(footer, "not run: enlarge the window to see 01 s1's sha256, what and why") {
			t.Errorf("%dx%d: footer = %q", size[0], size[1], footer)
		}
		h.m.width, h.m.height = 80, 10
		h.press("y")
		if !slices.Equal(h.backend.launched, []string{"01-s1"}) {
			t.Errorf("%dx%d then 80x10: launched %v, want [01-s1]", size[0], size[1], h.backend.launched)
		}
	}
}

// The y gate reads the frame View draws: whenever y runs, that frame showed
// the selected item's short sha256.
func TestDesk_YRunsOnlyWhatTheViewShows(t *testing.T) {
	for h := 6; h <= 16; h++ {
		for _, w := range []int{30, 40, 80} {
			hn := newDeskHarness(t)
			hn.drop("01-s1.sh", plainScript("s1"))
			hn.scan()
			hn.m.width, hn.m.height = w, h
			sha := hn.m.rows[0].item.Meta.SHA256
			view := ansi.Strip(hn.m.View().Content)
			hn.press("y")
			ran := len(hn.backend.launched) == 1
			var visible []string
			for _, l := range strings.Split(view, "\n") {
				visible = append(visible, ansi.Truncate(l, w, ""))
			}
			if ran && !strings.Contains(strings.Join(visible, "\n"), "sha256 "+sha[:deskShortHash]) {
				t.Errorf("%dx%d: y ran an item whose hash the view did not show:\n%s", w, h, view)
			}
		}
	}
}

func dropMany(h *deskHarness, n int) []string {
	h.t.Helper()
	var names []string
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%02d-item%02d", i, i)
		h.drop(name+".sh", plainScript(name))
		names = append(names, name)
	}
	h.scan()
	return names
}

// TestDesk_APagesAndRunsOnlyAfterEveryHashWasShown is #1098's second gap:
// with 12 items at 100x24 the a prompt cannot fit every hash at once. It
// pages, y refuses until every page has been on screen, and every full hash
// appears on some page.
func TestDesk_APagesAndRunsOnlyAfterEveryHashWasShown(t *testing.T) {
	h := newDeskHarness(t)
	names := dropMany(h, 12)
	h.m.width, h.m.height = 100, 24
	want := map[string]bool{}
	for _, r := range h.m.rows {
		want[r.item.Meta.SHA256] = false
	}

	h.press("a")
	if h.m.confirm != confirmAll {
		t.Fatalf("a did not ask; footer = %q", ansi.Strip(h.m.footer()))
	}
	seen := func() {
		for _, l := range strings.Split(ansi.Strip(h.m.View().Content), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "sha256" {
				want[f[1]] = true
			}
		}
	}
	seen()
	if !strings.Contains(ansi.Strip(h.m.footer()), "page 1/") {
		t.Fatalf("12 items at 100x24 should page; footer = %q", ansi.Strip(h.m.footer()))
	}
	h.press("y")
	if len(h.backend.launched) != 0 || h.m.confirm != confirmAll {
		t.Fatalf("y on page 1 ran %v; it must wait for every page", h.backend.launched)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "not run: see every page first") {
		t.Errorf("y on page 1 should say why it did not run: %q", ansi.Strip(h.m.footer()))
	}
	for range 10 {
		h.press("space")
		seen()
	}
	for sha, ok := range want {
		if !ok {
			t.Errorf("hash %s never appeared on any page", sha)
		}
	}
	h.press("y")
	if !slices.Equal(h.backend.launched, names) {
		t.Fatalf("after every page, launched %v, want %v", h.backend.launched, names)
	}
}

// A window too short for even one item's full hash refuses a outright.
func TestDesk_ARefusesWhenNoHashFits(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 2)
	h.m.width, h.m.height = 100, 4
	h.press("a")
	if h.m.confirm != confirmNone || !strings.Contains(ansi.Strip(h.m.footer()), "too small to show a full sha256") {
		t.Fatalf("a in a 4-line window: confirm %v footer %q", h.m.confirm, ansi.Strip(h.m.footer()))
	}
	h.m.width, h.m.height = 6, 40 // too narrow for an 8-character chunk
	h.press("a")
	if h.m.confirm != confirmNone {
		t.Fatalf("a in a 6-column window asked anyway: %q", ansi.Strip(h.m.footer()))
	}
}

// Shrinking the window during the a prompt re-pages it; hashes shown before
// the resize stay shown, and y still waits for the rest.
func TestDesk_AResizeRepagesWithoutLosingTrack(t *testing.T) {
	h := newDeskHarness(t)
	names := dropMany(h, 4)
	h.m.width, h.m.height = 100, 40
	h.press("a") // one page: all four shown
	out, _ := h.m.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
	h.m = out.(deskModel)
	h.press("y")
	if !slices.Equal(h.backend.launched, names) {
		t.Fatalf("every hash was on screen before the resize; launched %v", h.backend.launched)
	}
}

// Shrinking the window below one hash while the a prompt is open cancels on
// y, as the prompt then says, even though every hash was shown before.
func TestDesk_AShrunkBelowOneHashCancelsOnY(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 3)
	h.m.width, h.m.height = 100, 40
	h.press("a")
	if !h.m.allShown() {
		t.Fatal("a at 100x40 should show every hash on one page")
	}
	out, _ := h.m.Update(tea.WindowSizeMsg{Width: 100, Height: 4})
	h.m = out.(deskModel)
	if !strings.Contains(ansi.Strip(h.m.footer()), "any key cancels") {
		t.Fatalf("footer = %q", ansi.Strip(h.m.footer()))
	}
	h.press("y")
	if len(h.backend.launched) != 0 || h.m.confirm != confirmNone {
		t.Fatalf("y ran %v while the prompt said any key cancels", h.backend.launched)
	}
}

// On a one-page a prompt, j is "any other key" and cancels.
func TestDesk_ASinglePageAnyOtherKeyCancels(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 2)
	h.press("a", "j")
	if h.m.confirm != confirmNone {
		t.Fatalf("j kept a one-page prompt open: %q", ansi.Strip(h.m.footer()))
	}
}

// A size message with no height falls back to 80x24 everywhere, so a
// still asks.
func TestDesk_AWithAnUnknownHeightStillAsks(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 2)
	out, _ := h.m.Update(tea.WindowSizeMsg{Width: 100, Height: 0})
	h.m = out.(deskModel)
	h.press("a")
	if h.m.confirm != confirmAll {
		t.Fatalf("a with height 0 refused: %q", ansi.Strip(h.m.footer()))
	}
}

// An item with no valid hash is refused for that reason, not for the
// window's size.
func TestDesk_YNamesAMissingHash(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 1)
	h.m.rows[0].item.Meta.SHA256 = ""
	h.press("y")
	footer := ansi.Strip(h.m.footer())
	if len(h.backend.launched) != 0 || !strings.Contains(footer, "no valid sha256") || strings.Contains(footer, "too small") {
		t.Fatalf("launched %v footer %q", h.backend.launched, footer)
	}
}

// TestDesk_YRefusesOnceWhenARescanMovesTheSelection: another desk skips the
// selected item, and a rescan drops it, so the cursor lands on the next item. That item's hash was never matched, so y refuses once and
// names what happened; a second y (now on screen) runs it.
func TestDesk_YRefusesOnceWhenARescanMovesTheSelection(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-s1.sh", plainScript("s1"))
	h.drop("02-s2.sh", plainScript("s2"))
	h.scan()
	if err := h.d.Skip("01-s1", desk.SkipOperator); err != nil { // another desk skipped it
		h.t.Fatal(err)
	}
	h.scan()
	if r, _ := h.m.selected(); r.item.Name != "02-s2" {
		t.Fatalf("selected %s after 01-s1 left, want 02-s2", r.item.Name)
	}
	h.press("y")
	if len(h.backend.launched) != 0 {
		t.Fatalf("y ran %v right after the selection moved", h.backend.launched)
	}
	if footer := ansi.Strip(h.m.footer()); !strings.Contains(footer, "01 s1 left the queue") {
		t.Errorf("footer = %q", footer)
	}
	h.press("y")
	if !slices.Equal(h.backend.launched, []string{"02-s2"}) {
		t.Fatalf("the second y launched %v, want [02-s2]", h.backend.launched)
	}
}

// The operator's own skip moves the selection too, but they did that: y on
// the next item runs at once.
func TestDesk_OwnSkipDoesNotCostAnExtraY(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-s1.sh", plainScript("s1"))
	h.drop("02-s2.sh", plainScript("s2"))
	h.scan()
	h.press("s", "y") // skip 01-s1
	h.press("y")
	if !slices.Equal(h.backend.launched, []string{"02-s2"}) {
		t.Fatalf("launched %v, want [02-s2]", h.backend.launched)
	}
}

// While the a prompt crowds the dashboard out, the frame says the dashboard
// is hidden rather than that items cannot run.
func TestDesk_APromptDoesNotSayTooSmallToRun(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 12)
	h.m.width, h.m.height = 80, 10
	h.press("a")
	view := ansi.Strip(h.m.View().Content)
	if strings.Contains(view, "to run items") || !strings.Contains(view, "dashboard hidden while you confirm") {
		t.Errorf("view during the a prompt:\n%s", view)
	}
}

// A resize during the a prompt keeps the first item of the page on screen.
func TestDesk_AResizeKeepsThePlace(t *testing.T) {
	h := newDeskHarness(t)
	dropMany(h, 12)
	h.m.width, h.m.height = 100, 24
	h.press("a", "space")
	first := h.m.anchor
	out, _ := h.m.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
	h.m = out.(deskModel)
	if !strings.Contains(ansi.Strip(h.m.footer()), itemLabel(h.m.targets[first].name)) {
		t.Errorf("after the resize the page lost %s:\n%s", h.m.targets[first].name, ansi.Strip(h.m.footer()))
	}
}
