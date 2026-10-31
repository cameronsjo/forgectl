// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// deskAskStub is an in-memory asks store. Respond applies the store's own
// checks (ask.CheckResponse) so a refused response is refused here too.
type deskAskStub struct {
	now       time.Time
	records   []ask.Record
	responses []ask.Response
	notifyErr error
}

func (s *deskAskStub) ReadFrom(off int64) ([]ask.Record, int64, int, error) {
	if int(off) >= len(s.records) {
		return nil, off, 0, nil
	}
	return append([]ask.Record(nil), s.records[off:]...), int64(len(s.records)), 0, nil
}

func (s *deskAskStub) Respond(_ context.Context, r ask.Response) (ask.Item, ask.Response, error, error) {
	it, ok := ask.Find(ask.Fold(s.records, s.now), r.Ask)
	if !ok {
		return ask.Item{}, r, nil, ask.ErrNotFound
	}
	r, err := ask.CheckResponse(it, r, s.now)
	if err != nil {
		return ask.Item{}, r, nil, err
	}
	r.ID = fmt.Sprintf("resp-%08d", len(s.responses))
	r.At = s.now
	s.responses = append(s.responses, r)
	s.records = append(s.records, ask.Record{Response: &r})
	it, _ = ask.Find(ask.Fold(s.records, s.now), r.Ask)
	return it, r, s.notifyErr, nil
}

var deskAskT0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func deskAskFixture(asks ...ask.Ask) *deskAskStub {
	s := &deskAskStub{now: deskAskT0}
	for i, a := range asks {
		if a.ID == "" {
			a.ID = fmt.Sprintf("ask-%08d", i)
		}
		if a.At.IsZero() {
			a.At = deskAskT0.Add(-time.Duration(i+1) * time.Hour)
		}
		if len(a.Allow) == 0 {
			a.Allow = ask.Kinds
		}
		if a.Bucket == "" {
			a.Bucket = ask.BucketNow
		}
		s.records = append(s.records, ask.Record{Ask: &a})
	}
	return s
}

func newDeskAskTestModel(t *testing.T, s *deskAskStub, scripts []DeskAskScript) deskAsksModel {
	t.Helper()
	opts := DeskAsksOptions{Store: s, Theme: theme.Default(), Now: func() time.Time { return s.now }}
	if scripts != nil {
		opts.Scripts = func() ([]DeskAskScript, error) { return scripts, nil }
	}
	m := newDeskAsksModel(context.Background(), opts)
	if err := m.readLog(); err != nil {
		t.Fatal(err)
	}
	m.loadScripts()
	m.width, m.height = 100, 30
	m.clampCursor()
	return m
}

// deskAskPress sends k and runs a respond command to completion, feeding its
// message back, so one call is one full keypress.
func deskAskPress(t *testing.T, m deskAsksModel, k tea.KeyPressMsg) deskAsksModel {
	t.Helper()
	out, cmd := m.Update(k)
	m = out.(deskAsksModel)
	if cmd != nil {
		// A respond command returns at once; the textinput's cursor blink
		// is a timer, so wait briefly and drop anything slower.
		ch := make(chan tea.Msg, 1)
		go func() { ch <- cmd() }()
		var msg tea.Msg
		select {
		case msg = <-ch:
		case <-time.After(200 * time.Millisecond):
		}
		if msg, ok := msg.(deskAsksRespondedMsg); ok {
			out, _ = m.Update(msg)
			m = out.(deskAsksModel)
		}
	}
	return m
}

func deskAskType(t *testing.T, m deskAsksModel, text string) deskAsksModel {
	t.Helper()
	for _, r := range text {
		out, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = out.(deskAsksModel)
	}
	return m
}

var deskAskEnter = tea.KeyPressMsg{Code: tea.KeyEnter}

func TestDeskAskMoveSkipsHeadings(t *testing.T) {
	s := deskAskFixture(
		ask.Ask{Title: "first now"},
		ask.Ask{Title: "a soon one", Bucket: ask.BucketSoon},
	)
	m := newDeskAskTestModel(t, s, []DeskAskScript{{Name: "01-merge", State: "waiting", What: "merge it", SHA256: strings.Repeat("a", 64)}})
	if got := m.selectedKey(); got != "ask:ask-00000000" {
		t.Fatalf("start on the first ask, got %q", got)
	}
	m = deskAskPress(t, m, key("j"))
	if got := m.selectedKey(); got != "ask:ask-00000001" {
		t.Fatalf("j crosses the heading to the soon ask, got %q", got)
	}
	m = deskAskPress(t, m, key("j"))
	if got := m.selectedKey(); got != "script:01-merge" {
		t.Fatalf("j reaches the script, got %q", got)
	}
	m = deskAskPress(t, m, key("d"))
	if m.mode != deskAsksList || len(s.responses) != 0 {
		t.Fatalf("d on a script must not respond: mode %v responses %v", m.mode, s.responses)
	}
	m = deskAskPress(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m = deskAskPress(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.selectedKey(); got != "ask:ask-00000000" {
		t.Fatalf("up returns to the first ask, got %q", got)
	}
}

func TestDeskAskDoneNeedsConfirm(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "rotate the key", Notify: "t1k"})
	m := newDeskAskTestModel(t, s, nil)

	m = deskAskPress(t, m, key("d"))
	m = deskAskPress(t, m, key("x"))
	if len(s.responses) != 0 {
		t.Fatalf("a key other than d or y must cancel, got %v", s.responses)
	}

	m = deskAskPress(t, m, key("d"))
	if m.mode != deskAsksConfirmDone {
		t.Fatalf("d asks for confirmation, mode %v", m.mode)
	}
	m = deskAskPress(t, m, key("d"))
	if len(s.responses) != 1 {
		t.Fatalf("d d records one response, got %v", s.responses)
	}
	r := s.responses[0]
	if r.Kind != ask.KindDone || r.Via != "ui" || r.Ask != "ask-00000000" {
		t.Fatalf("response = %+v, want done via ui for ask-00000000", r)
	}
	if !strings.Contains(m.status, "notified t1k") || m.statusWarn {
		t.Fatalf("status = %q (warn %v), want a notified line", m.status, m.statusWarn)
	}
}

func TestDeskAskReply(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "which region?"})
	m := newDeskAskTestModel(t, s, nil)
	m = deskAskPress(t, m, key("r"))
	if m.mode != deskAsksReply {
		t.Fatalf("r opens the reply prompt, mode %v", m.mode)
	}
	m = deskAskType(t, m, "use the west one")
	m = deskAskPress(t, m, deskAskEnter)
	if len(s.responses) != 1 || s.responses[0].Kind != ask.KindReply || s.responses[0].Text != "use the west one" {
		t.Fatalf("responses = %+v, want one reply", s.responses)
	}
	if !strings.Contains(m.status, "no terminal to notify") {
		t.Fatalf("status = %q", m.status)
	}
}

func TestDeskAskLinkRefusesUnsafeURL(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "paste the doc link"})
	m := newDeskAskTestModel(t, s, nil)
	m = deskAskPress(t, m, key("l"))
	m = deskAskType(t, m, "javascript:alert(1)")
	m = deskAskPress(t, m, deskAskEnter)
	if len(s.responses) != 0 {
		t.Fatalf("a javascript: link must not be sent, got %+v", s.responses)
	}
	if m.mode != deskAsksLink || m.inputErr == "" {
		t.Fatalf("the prompt stays open with an error: mode %v err %q", m.mode, m.inputErr)
	}
	m = deskAskPress(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = deskAskPress(t, m, key("l"))
	m = deskAskType(t, m, "https://example.com/doc")
	m = deskAskPress(t, m, deskAskEnter)
	if len(s.responses) != 1 || s.responses[0].URL != "https://example.com/doc" {
		t.Fatalf("responses = %+v, want the https link", s.responses)
	}
}

func TestDeskAskSnoozeFourHours(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "check the phone"})
	m := newDeskAskTestModel(t, s, nil)
	m = deskAskPress(t, m, key("s"))
	if m.mode != deskAsksSnooze {
		t.Fatalf("s opens the snooze choice, mode %v", m.mode)
	}
	m = deskAskPress(t, m, key("2"))
	if len(s.responses) != 1 || s.responses[0].Kind != ask.KindSnooze {
		t.Fatalf("responses = %+v, want one snooze", s.responses)
	}
	if want := deskAskT0.Add(4 * time.Hour); !s.responses[0].Until.Equal(want) {
		t.Fatalf("until = %v, want %v", s.responses[0].Until, want)
	}
	if got := m.items[0].State; got != ask.StateSnoozed {
		t.Fatalf("the ask is %s after the re-read, want snoozed", got)
	}
}

func TestDeskAskDisallowedKindDoesNothing(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "just tick it", Allow: []ask.Kind{ask.KindDone}})
	m := newDeskAskTestModel(t, s, nil)
	for _, k := range []string{"r", "l", "s"} {
		m = deskAskPress(t, m, key(k))
		if m.mode != deskAsksList {
			t.Fatalf("%s opened a prompt on an ask that allows only done (mode %v)", k, m.mode)
		}
	}
	if len(s.responses) != 0 {
		t.Fatalf("responses = %+v, want none", s.responses)
	}
	if !strings.Contains(m.status, "does not take") {
		t.Fatalf("status = %q, want why nothing happened", m.status)
	}
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "respond: d done ") || strings.Contains(v, "respond: d done ·") {
		t.Fatalf("the details offer a reply the ask does not allow:\n%s", v)
	}
}

func TestDeskAskStoreRefusalShows(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "x"})
	m := newDeskAskTestModel(t, s, nil)
	out, _ := m.Update(deskAsksRespondedMsg{err: errors.New("ask: already answered: ask-00000000")})
	m = out.(deskAsksModel)
	if !m.statusWarn || !strings.Contains(m.status, "not recorded") {
		t.Fatalf("status = %q warn %v", m.status, m.statusWarn)
	}
}

func TestDeskAskNotifyFailureShows(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "x", Notify: "t9"})
	s.notifyErr = errors.New("hc: terminal gone")
	m := newDeskAskTestModel(t, s, nil)
	m = deskAskPress(t, m, key("d"))
	m = deskAskPress(t, m, key("y"))
	if len(s.responses) != 1 || !m.statusWarn || !strings.Contains(m.status, "notify failed") {
		t.Fatalf("responses %d status %q warn %v", len(s.responses), m.status, m.statusWarn)
	}
}

func TestDeskAskLiveReread(t *testing.T) {
	s := deskAskFixture(ask.Ask{Title: "first"})
	m := newDeskAskTestModel(t, s, nil)
	a := ask.Ask{ID: "ask-0000000a", At: deskAskT0, Title: "arrived later", Bucket: ask.BucketSoon, Allow: ask.Kinds}
	s.records = append(s.records, ask.Record{Ask: &a})
	out, _ := m.Update(deskAsksTickMsg{})
	m = out.(deskAsksModel)
	if len(m.items) != 2 || !strings.Contains(ansi.Strip(m.View().Content), "arrived later") {
		t.Fatalf("a tick re-reads the log; items %d", len(m.items))
	}
	if got := m.selectedKey(); got != "ask:ask-00000000" {
		t.Fatalf("the cursor stays on its ask, got %q", got)
	}
}

func TestDeskAskHostileTextIsInert(t *testing.T) {
	hostile := "evil\x1b]52;c;cGF3bmVk\x07 \x1b[2J\u202egnp.exe title"
	s := deskAskFixture(ask.Ask{
		Title: hostile,
		Steps: "step one \x1b[31mred\x1b[0m\n\u202eflipped",
		From:  "agent\x1b[1m",
		Links: []ask.Link{{Label: "doc\x1b[5m", URL: "https://example.com/\x1b"}},
	})
	m := newDeskAskTestModel(t, s, []DeskAskScript{{Name: "01-x\x1b[2J", What: "w\x1b]8;;http://x\x07", State: "waiting"}})
	for _, w := range []int{100, 140} {
		m.width = w
		v := m.View().Content
		// The theme's own styling is the only escape allowed: strip it and
		// nothing ESC, BEL or bidi may be left.
		plain := ansi.Strip(v)
		for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
			if strings.Contains(plain, bad) {
				t.Fatalf("width %d: raw %q reached the view:\n%q", w, bad, plain)
			}
		}
		for _, bad := range []string{"]52;", "[2J"} {
			if strings.Contains(v, "\x1b"+bad) {
				t.Fatalf("width %d: escape sequence %q reached the view", w, bad)
			}
		}
		if !strings.Contains(plain, "evil") {
			t.Fatalf("width %d: the title text is missing:\n%s", w, plain)
		}
	}
}

func TestDeskAskFrame(t *testing.T) {
	s := deskAskFixture(
		ask.Ask{Title: "Rotate the deploy key in the password manager", Steps: "1. Open the vault\n2. Rename the old key", From: "worker-a", Notify: "t1k"},
		ask.Ask{Title: "Read the failed deployment reason in the cloud portal", Bucket: ask.BucketSoon, Links: []ask.Link{{Label: "portal", URL: "https://portal.example.com"}}},
		ask.Ask{Title: "Paste the doc link if you want it updated", Bucket: ask.BucketOptional, Allow: []ask.Kind{ask.KindLink, ask.KindSnooze}},
		ask.Ask{Title: "Go-live script for the docs site", Bucket: ask.BucketComing},
	)
	m := newDeskAskTestModel(t, s, []DeskAskScript{{Name: "03-merge-1201", State: "waiting", What: "Merge PR 1201 once checks are green", SHA256: strings.Repeat("ab", 32)}})
	frame, err := RenderDeskAsksFrame(context.Background(), m.opts, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(frame)
	lines := strings.Split(plain, "\n")
	if len(lines) != 30 {
		t.Fatalf("frame has %d lines, want 30:\n%s", len(lines), plain)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 100 {
			t.Fatalf("line %d is %d wide:\n%s", i, w, l)
		}
	}
	for _, want := range []string{
		"desk", "1 now", "1 soon", "1 optional", "1 coming", "1 script",
		"Now", "When you have 10 minutes", "Optional", "Coming", "Scripts",
		"▸ Rotate the deploy key", "03-merge-1201", "abababababab",
		"from worker-a", "notifies t1k", "1. Open the vault", "respond: d done · r reply · l link · s snooze",
		"d done · r reply",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("frame lacks %q:\n%s", want, plain)
		}
	}
}
