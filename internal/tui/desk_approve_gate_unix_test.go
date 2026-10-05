// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/runview"
)

// longBody is a script too long for the approve dialog to show at once.
func longBody() string {
	var b strings.Builder
	for i := range 200 {
		b.WriteString("echo line " + strings.Repeat("x", i%7) + "\n")
	}
	return b.String()
}

// TestAHeldYNeverApproves: a terminal that does not report repeats sends a
// held y as plain presses about 30 ms apart. A y held from just after a,
// whose first press is too soon, keeps arriving well past every delay, and
// none of it approves. A lone y after a pause does. The dashboard also asks for key event types,
// so a terminal that can mark repeats does.
func TestAHeldYNeverApproves(t *testing.T) {
	d, _ := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	if !m.View().KeyboardEnhancements.ReportEventTypes {
		t.Error("the dashboard does not ask the terminal for key event types")
	}
	m = pressDesk(t, m, key("a"))
	if !confirmOpen(m) {
		t.Fatal("a did not open the approve dialog")
	}
	for i := range 60 {
		c.add(30 * time.Millisecond)
		m = pressDesk(t, m, key("y"))
		if n, _, _ := rec.counts(); n != 0 {
			t.Fatalf("y %d of a held stream approved", i+1)
		}
	}
	mustContain(t, "note", screen(t, m), "too fast: press y once, on its own")
	c.add(300 * time.Millisecond)
	_ = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 1 {
		t.Fatalf("a lone y after a 300 ms pause: %d claims, want 1", n)
	}
}

// TestAReleaseCountsAsAKeyEvent: a y right after another key's release is
// as close to that key as a y right after its press.
func TestAReleaseCountsAsAKeyEvent(t *testing.T) {
	d, _ := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	c.add(2 * time.Second)
	next, _ := m.Update(tea.KeyReleaseMsg{Code: 'j', Text: "j"})
	m = next.(deskModel)
	c.add(100 * time.Millisecond)
	_ = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 0 {
		t.Fatal("a y 100 ms after a key release approved")
	}
}

// TestKeysTypedAheadNeverApprove: a, G and y typed back to back, with the
// model's clock moving a few milliseconds between them, approve nothing,
// whether the body fits on screen or not, and whether the keys arrive
// before or after the body has been read.
func TestKeysTypedAheadNeverApprove(t *testing.T) {
	for _, tc := range []struct{ name, body string }{{"fits", "echo hi\n"}, {"long", longBody()}} {
		t.Run(tc.name+"/after the read", func(t *testing.T) {
			d, _ := queueDesk(t, false, tc.body)
			c := newClock()
			m, rec := openQueue(t, d, c, 120, 30)
			for _, k := range []string{"a", "G", "y"} {
				c.add(3 * time.Millisecond)
				m = pressDesk(t, m, key(k))
			}
			if n, _, _ := rec.counts(); n != 0 {
				t.Fatal("a, G, y typed together approved")
			}
			if !confirmOpen(m) {
				t.Fatal("the dialog did not open")
			}
		})
		t.Run(tc.name+"/before the read", func(t *testing.T) {
			d, _ := queueDesk(t, false, tc.body)
			c := newClock()
			m, rec := openQueue(t, d, c, 120, 30)
			// The read a started is still running when G and y arrive.
			next, read := m.Update(key("a"))
			m = next.(deskModel)
			for _, k := range []string{"G", "y"} {
				c.add(3 * time.Millisecond)
				m = pressDesk(t, m, key(k))
			}
			m = pumpDesk(t, m, read)
			if !confirmOpen(m) {
				t.Fatal("the dialog did not open once the body was read")
			}
			c.add(3 * time.Millisecond)
			_ = pressDesk(t, m, key("y"))
			if n, _, _ := rec.counts(); n != 0 {
				t.Fatal("keys typed before the body was read approved")
			}
		})
	}
}

// TestTheDelayStartsOverWhenTheEndComesOnScreen: a long item open well past
// the delay, scrolled to its end with G, still needs the delay after G before
// y counts. The y is past the key-gap rule, so only the restart refuses it.
func TestTheDelayStartsOverWhenTheEndComesOnScreen(t *testing.T) {
	d, _ := queueDesk(t, false, longBody())
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	c.add(time.Second)
	m = pressDesk(t, m, key("G"))
	c.add(300 * time.Millisecond)
	m = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 0 {
		t.Fatal("y 300 ms after the end came on screen approved")
	}
	mustContain(t, "note", screen(t, m), "too soon: y counts a moment after the end is on screen")
	c.add(300 * time.Millisecond)
	_ = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 1 {
		t.Fatalf("y 600 ms after the end: %d claims, want 1", n)
	}
}

// TestTheApproveBodyIsLaidOutOnce: messages that change neither the item nor
// the width reuse the laid-out body; a resize lays it out again.
func TestTheApproveBodyIsLaidOutOnce(t *testing.T) {
	d, _ := queueDesk(t, false, longBody())
	c := newClock()
	m, _ := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	first := &m.topModal().lines[0]
	for _, msg := range []tea.Msg{deskTickMsg{}, key("j"), key("k"), tea.MouseWheelMsg{Button: tea.MouseWheelDown}} {
		next, _ := m.Update(msg)
		m = next.(deskModel)
		if &m.topModal().lines[0] != first {
			t.Fatalf("%T laid the body out again", msg)
		}
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m = next.(deskModel)
	if &m.topModal().lines[0] == first {
		t.Error("a resize kept the body laid out for the old width")
	}
}

// TestTheQueueSaysWhatTheScanLeftUnread: items past the scan's caps are
// counted under the queue.
func TestTheQueueSaysWhatTheScanLeftUnread(t *testing.T) {
	src := &fakeDeskSource{name: "desk", unread: 7, items: []runview.QueueItem{{Name: "0001-a", Kind: "script", State: "waiting"}}}
	m := openDesk(t, optsFor(src), 120, 30)
	mustContain(t, "queue", screen(t, m), "7 more pending not shown")
}
