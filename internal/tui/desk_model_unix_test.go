//go:build unix

package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
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

// fakeLaunch is a real desk whose Launch only records the name: a claimed
// item stays in running/ and no supervisor process is started.
type fakeLaunch struct {
	*desk.Desk
	launched []string
}

func (f *fakeLaunch) Launch(name string) (int, error) {
	f.launched = append(f.launched, name)
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

	h.press("u")
	if got := h.where("01-alpha"); got != desk.DirPending {
		t.Fatalf("after u the item is in %s, want pending", got)
	}
	h.press("u")
	if !strings.Contains(ansi.Strip(h.m.footer()), "nothing to undo") {
		t.Errorf("a second u should have nothing to undo: %q", ansi.Strip(h.m.footer()))
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
	if h.m.confirm != confirmAll || !strings.Contains(prompt, "run 3: 01 alpha, 03 gamma, 04 delta?") {
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

// TestTTYRunPassesFD3 is the platform check for ttyArgv: script(1) must hand
// fd 3 through to bash, or a TTY item runs nothing. It runs the real
// script(1) on the platform under test.
func TestTTYRunPassesFD3(t *testing.T) {
	if _, err := os.Stat("/usr/bin/script"); err != nil {
		t.Skip("no /usr/bin/script on this machine")
	}
	dir := t.TempDir()
	logPath, rcPath := filepath.Join(dir, "log"), filepath.Join(dir, "rc")
	a := ttyArgv(logPath, rcPath)
	cmd := newCmd(a)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close() //nolint:errcheck // read-only
	cmd.Stdin = devnull
	release, err := desk.AttachScript(cmd, []byte("echo fd3-reached-bash\nexit 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Start()
	release()
	if err != nil {
		t.Fatalf("start %v: %v", a, err)
	}
	_ = cmd.Wait()                     // script(1)'s own status is not the item's; the rc file is
	logData, _ := os.ReadFile(logPath) //nolint:gosec // G304: a path under t.TempDir
	rcData, _ := os.ReadFile(rcPath)   //nolint:gosec // G304: a path under t.TempDir
	if !strings.Contains(string(logData), "fd3-reached-bash") {
		t.Fatalf("bash never read fd 3 through script(1)\nargv %q\nlog %q", a, logData)
	}
	if strings.TrimSpace(string(rcData)) != "3" {
		t.Fatalf("rc file = %q, want 3", rcData)
	}
}

func newCmd(a []string) *osexec.Cmd {
	return osexec.CommandContext(context.Background(), a[0], a[1:]...) //nolint:gosec // G204: ttyArgv's fixed argv
}
