// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The approve dialog and the queue verbs. These tests work against a real
// desk in a temp dir: Claim, Abandon, Skip and Unskip are the desk's own.
// Launch is replaced, because the real one starts a detached supervisor by
// re-running this binary.

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// recVerbs records every verb and passes it to the desk, except Launch,
// which returns launchPID and launchErr.
type recVerbs struct {
	d *desk.Desk

	mu        sync.Mutex
	claims    []string // the sha each Claim was given
	launched  []*desk.Claimed
	abandons  []string
	skips     []string
	unskips   []string
	launchPID int
	launchErr error
}

func (r *recVerbs) Claim(name, sha string) (*desk.Claimed, error) {
	r.mu.Lock()
	r.claims = append(r.claims, sha)
	r.mu.Unlock()
	return r.d.Claim(name, sha)
}

func (r *recVerbs) Launch(c *desk.Claimed) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launched = append(r.launched, c)
	return r.launchPID, r.launchErr
}

func (r *recVerbs) Abandon(name, reason string) error {
	r.mu.Lock()
	r.abandons = append(r.abandons, reason)
	r.mu.Unlock()
	return r.d.Abandon(name, reason)
}

func (r *recVerbs) Skip(name, reason string) error {
	r.mu.Lock()
	r.skips = append(r.skips, reason)
	r.mu.Unlock()
	return r.d.Skip(name, reason)
}

func (r *recVerbs) Unskip(name string) error {
	r.mu.Lock()
	r.unskips = append(r.unskips, name)
	r.mu.Unlock()
	return r.d.Unskip(name)
}

// scratchHome points HOME and TMPDIR at temp dirs, so nothing a test does
// reaches the real home directory.
func scratchHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
}

// queueDesk is a desk in a temp dir with one queued script per body.
func queueDesk(t *testing.T, tty bool, bodies ...string) (*desk.Desk, []desk.Added) {
	t.Helper()
	scratchHome(t)
	d, err := desk.Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var added []desk.Added
	for i, body := range bodies {
		src := filepath.Join(t.TempDir(), "item"+string(rune('a'+i))+".sh")
		if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := d.Add(src, "tidy the scratch dir", "it filled up", tty)
		if err != nil {
			t.Fatal(err)
		}
		added = append(added, a)
	}
	if _, err := d.Scan(); err != nil {
		t.Fatal(err)
	}
	return d, added
}

// deskClock is a clock a test moves by hand.
type deskClock struct{ t time.Time }

func (c *deskClock) now() time.Time      { return c.t }
func (c *deskClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *deskClock               { return &deskClock{t: deskNow} }
func queueOpts(d *desk.Desk, c *deskClock) DeskOptions {
	return DeskOptions{Sources: []runview.Source{runview.NewDeskSource(d)}, Theme: theme.Default(), Now: c.now}
}

// openQueue opens the dashboard on d's queue with rec as its verbs.
func openQueue(t *testing.T, d *desk.Desk, c *deskClock, w, h int) (deskModel, *recVerbs) {
	t.Helper()
	m := openDesk(t, queueOpts(d, c), w, h)
	if m.sideShows() != viewQueue {
		m = pressDesk(t, m, key("6"))
	}
	rec := &recVerbs{d: d, launchPID: 4242}
	m.verbs = rec
	return m, rec
}

func (r *recVerbs) counts() (claims, launches, abandons int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.claims), len(r.launched), len(r.abandons)
}

func scanDesk(t *testing.T, d *desk.Desk) *desk.Snapshot {
	t.Helper()
	s, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func confirmOpen(m deskModel) bool {
	md := m.topModal()
	return md != nil && md.kind == modalConfirm
}

// TestAYBurstWithinTheDelayClaimsNothing: a and y typed together, or y
// before the dialog has been open 400 ms by the model's clock, approves
// nothing. The same y after the delay does, which shows the test can go red.
func TestAYBurstWithinTheDelayClaimsNothing(t *testing.T) {
	d, added := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)

	m = pressDesk(t, m, key("a"))
	if !confirmOpen(m) {
		t.Fatalf("a did not open the approve dialog:\n%s", screen(t, m))
	}
	m = pressDesk(t, m, key("y"))
	c.add(399 * time.Millisecond)
	m = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 0 {
		t.Fatalf("a y within 400 ms claimed (%d claims)", n)
	}
	if !strings.Contains(screen(t, m), "too soon") {
		t.Errorf("the dialog does not say why y did nothing:\n%s", screen(t, m))
	}
	if s := scanDesk(t, d); len(s.Pending) != 1 {
		t.Fatalf("the item left pending/: %+v", s)
	}

	c.add(2 * time.Millisecond)
	m = pressDesk(t, m, key("y"))
	if n, l, _ := rec.counts(); n != 1 || l != 1 {
		t.Fatalf("y after the delay: %d claims, %d launches; want 1 and 1", n, l)
	}
	if rec.claims[0] != added[0].SHA256 || !hex64.MatchString(rec.claims[0]) {
		t.Errorf("claimed with %q, want the queued %q", rec.claims[0], added[0].SHA256)
	}
	if confirmOpen(m) {
		t.Error("the dialog stayed open after the approval")
	}
	if !strings.Contains(screen(t, m), "started "+added[0].Name) {
		t.Errorf("no started note:\n%s", screen(t, m))
	}
}

// TestOnlyAPlainYPressApproves: enter, clicks inside and outside the
// dialog, a held y, and y with a modifier never approve; the dialog stays
// open through all of them, and a plain y then approves.
func TestOnlyAPlainYPressApproves(t *testing.T) {
	d, _ := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	c.add(time.Second)

	x, y, w, h := m.modalRect()
	inputs := []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyEnter},
		tea.MouseClickMsg{X: x + w/2, Y: y + h/2, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: x + w/2, Y: y + h/2, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft},
		tea.KeyPressMsg{Code: 'y', Text: "y", IsRepeat: true},
		tea.KeyPressMsg{Code: 'y', Mod: tea.ModAlt},
		tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl},
		tea.KeyPressMsg{Code: 'Y', Text: "Y"},
	}
	for i, in := range inputs {
		next, cmd := m.Update(in)
		m = pumpDesk(t, next.(deskModel), cmd)
		if n, _, _ := rec.counts(); n != 0 {
			t.Fatalf("input %d (%T %+v) claimed", i, in, in)
		}
		if !confirmOpen(m) {
			t.Fatalf("input %d (%T %+v) closed the approve dialog", i, in, in)
		}
	}
	_ = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 1 {
		t.Fatalf("a plain y after the inputs: %d claims, want 1", n)
	}
}

// TestALongItemMustBeReadToTheEnd: y does nothing until the body's last
// line has been on screen, then approves.
func TestALongItemMustBeReadToTheEnd(t *testing.T) {
	var body strings.Builder
	for i := range 200 {
		body.WriteString("echo line " + strings.Repeat("x", i%7) + "\n")
	}
	d, _ := queueDesk(t, false, body.String())
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	c.add(time.Second)

	out := screen(t, m)
	mustContain(t, "unread", out, "more lines ↓")
	m = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 0 {
		t.Fatal("y approved an item whose end was never on screen")
	}
	mustContain(t, "note", screen(t, m), "read it to the end first")

	m = pressDesk(t, m, key("j"))
	m = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 0 {
		t.Fatal("y approved after one line of scrolling")
	}
	m = pressDesk(t, m, key("G"))
	mustContain(t, "at the end", screen(t, m), "end of item")
	m = pressDesk(t, m, key("g")) // back to the top: the end has been seen
	_ = pressDesk(t, m, key("y"))
	if n, _, _ := rec.counts(); n != 1 {
		t.Fatalf("y after reading to the end: %d claims, want 1", n)
	}
}

// TestTheClaimIsBoundToTheFrozenHash: the hash Claim gets is the full hash
// of the copy the dialog froze, and the claim that comes back carries it to
// Launch. A short or empty hash is refused by the desk and moves nothing.
func TestTheClaimIsBoundToTheFrozenHash(t *testing.T) {
	d, added := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	frozen := m.topModal().item.SHA256
	c.add(time.Second)
	_ = pressDesk(t, m, key("y"))

	if len(rec.claims) != 1 || rec.claims[0] != frozen || frozen != added[0].SHA256 || !hex64.MatchString(frozen) {
		t.Fatalf("claims %q, frozen %q, queued %q", rec.claims, frozen, added[0].SHA256)
	}
	if len(rec.launched) != 1 || rec.launched[0].SHA256 != frozen {
		t.Fatalf("Launch got %+v", rec.launched)
	}
	if s := scanDesk(t, d); len(s.Running) != 1 || s.Running[0].Name != added[0].Name {
		t.Errorf("after the claim: %+v", s)
	}

	d2, added2 := queueDesk(t, false, "echo two\n")
	for _, sha := range []string{"", added2[0].SHA256[:63], added2[0].SHA256[:12]} {
		if _, err := d2.Claim(added2[0].Name, sha); !errors.Is(err, desk.ErrRefused) {
			t.Errorf("Claim(%q) err = %v, want ErrRefused", sha, err)
		}
	}
	if s := scanDesk(t, d2); len(s.Pending) != 1 {
		t.Errorf("a refused claim moved the item: %+v", s)
	}
}

// rewrite replaces a pending item's bytes. With meta, it also records the new
// hash, as a consistent re-queue under the same name would.
func rewrite(t *testing.T, d *desk.Desk, name, body string, meta bool) string {
	t.Helper()
	p := filepath.Join(d.Path(), desk.DirPending, name+".sh")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := desk.SHA256Hex([]byte(body))
	if !meta {
		return sum
	}
	mp := filepath.Join(d.Path(), desk.DirPending, name+".meta.json")
	raw, err := os.ReadFile(mp) //nolint:gosec // G304: a path inside the test's own temp desk
	if err != nil {
		t.Fatal(err)
	}
	var mm map[string]any
	if err := json.Unmarshal(raw, &mm); err != nil {
		t.Fatal(err)
	}
	mm["sha256"] = sum
	out, err := json.Marshal(mm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return sum
}

// TestAnItemEditedAfterTheDialogOpenedRunsNothing covers the three ways the
// item can change under an open dialog.
func TestAnItemEditedAfterTheDialogOpenedRunsNothing(t *testing.T) {
	t.Run("rewritten with its hash, then y: re-opens on the new bytes", func(t *testing.T) {
		d, added := queueDesk(t, false, "echo old\n")
		c := newClock()
		m, rec := openQueue(t, d, c, 120, 30)
		m = pressDesk(t, m, key("a"))
		newSum := rewrite(t, d, added[0].Name, "echo new\n", true)
		c.add(time.Second)
		m = pressDesk(t, m, key("y"))

		if _, l, _ := rec.counts(); l != 0 {
			t.Fatal("the edited item was launched")
		}
		if s := scanDesk(t, d); len(s.Pending) != 1 || len(s.Running) != 0 {
			t.Fatalf("after the refused claim: %+v", s)
		}
		mustContain(t, "changed", screen(t, m), "changed since you opened it")
		if !confirmOpen(m) {
			t.Fatalf("the dialog did not re-open:\n%s", screen(t, m))
		}
		md := m.topModal()
		if md.item.SHA256 != newSum || !strings.Contains(strings.Join(md.item.Body, "\n"), "echo new") {
			t.Fatalf("re-opened on %q %q, want the new bytes", md.item.SHA256, md.item.Body)
		}
		// The re-opened dialog is a new review: its delay starts over.
		_ = pressDesk(t, m, key("y"))
		if n, _, _ := rec.counts(); n != 1 {
			t.Fatalf("a y straight after the re-open claimed (%d claims)", n)
		}
	})
	t.Run("rewritten, then a refresh: closes with re-review", func(t *testing.T) {
		d, added := queueDesk(t, false, "echo old\n")
		c := newClock()
		m, rec := openQueue(t, d, c, 120, 30)
		m = pressDesk(t, m, key("a"))
		rewrite(t, d, added[0].Name, "echo new\n", true)
		m = relistDesk(t, m)
		if confirmOpen(m) {
			t.Fatal("the dialog stayed open on a changed item")
		}
		mustContain(t, "re-review", screen(t, m), "changed — re-review")
		c.add(time.Second)
		_ = pressDesk(t, m, key("y"))
		if n, _, _ := rec.counts(); n != 0 {
			t.Fatal("y after the close claimed")
		}
	})
	t.Run("bytes changed but not the hash: skipped as changed", func(t *testing.T) {
		d, added := queueDesk(t, false, "echo old\n")
		c := newClock()
		m, rec := openQueue(t, d, c, 120, 30)
		m = pressDesk(t, m, key("a"))
		rewrite(t, d, added[0].Name, "echo new\n", false)
		c.add(time.Second)
		m = pressDesk(t, m, key("y"))
		if _, l, a := rec.counts(); l != 0 || a != 0 {
			t.Fatalf("launches %d, abandons %d; want none", l, a)
		}
		s := scanDesk(t, d)
		if len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != desk.SkipChanged {
			t.Fatalf("after the claim: %+v", s)
		}
		mustContain(t, "changed", screen(t, m), "changed since you opened it")
	})
	t.Run("gone from the queue: closes with re-review", func(t *testing.T) {
		d, added := queueDesk(t, false, "echo old\n")
		m, _ := openQueue(t, d, newClock(), 120, 30)
		m = pressDesk(t, m, key("a"))
		if err := d.Skip(added[0].Name, desk.SkipOperator); err != nil {
			t.Fatal(err)
		}
		m = relistDesk(t, m)
		if confirmOpen(m) {
			t.Fatal("the dialog stayed open on an item no longer waiting")
		}
		mustContain(t, "re-review", screen(t, m), "changed — re-review")
	})
}

// TestTwoDesksApprovingOneItemLeaveOneWinner: two dashboards on one desk
// dir both approve; one claim wins and the other is told so.
func TestTwoDesksApprovingOneItemLeaveOneWinner(t *testing.T) {
	d1, added := queueDesk(t, false, "echo hi\n")
	d2, err := desk.Open(d1.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	c := newClock()
	m1, rec1 := openQueue(t, d1, c, 120, 30)
	m2, rec2 := openQueue(t, d2, c, 120, 30)
	m1 = pressDesk(t, m1, key("a"))
	m2 = pressDesk(t, m2, key("a"))
	c.add(time.Second)
	m1 = pressDesk(t, m1, key("y"))
	m2 = pressDesk(t, m2, key("y"))

	_, l1, _ := rec1.counts()
	_, l2, _ := rec2.counts()
	if l1+l2 != 1 {
		t.Fatalf("launches: %d and %d, want exactly one", l1, l2)
	}
	loser := m2
	if l2 == 1 {
		loser = m1
	}
	mustContain(t, "loser", screen(t, loser), "another desk claimed it")
	if s := scanDesk(t, d1); len(s.Running) != 1 || s.Running[0].Name != added[0].Name || len(s.Pending) != 0 {
		t.Errorf("after both approvals: %+v", s)
	}
}

// TestAFailedLaunchGivesTheClaimBack: an error after a successful claim and
// before a started supervisor leaves the item in skipped/ as launch-failed.
func TestAFailedLaunchGivesTheClaimBack(t *testing.T) {
	d, added := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	rec.launchPID, rec.launchErr = 0, errors.New("no supervisor binary")
	m = pressDesk(t, m, key("a"))
	c.add(time.Second)
	m = pressDesk(t, m, key("y"))

	if len(rec.abandons) != 1 || rec.abandons[0] != "launch-failed" {
		t.Fatalf("abandons = %q, want one launch-failed", rec.abandons)
	}
	s := scanDesk(t, d)
	if len(s.Skipped) != 1 || s.Skipped[0].Name != added[0].Name || s.Skipped[0].Meta.SkipReason != "launch-failed" || len(s.Running) != 0 {
		t.Fatalf("after the failed launch: %+v", s)
	}
	mustContain(t, "error", screen(t, m), "no supervisor binary", "moved to skipped: launch-failed")
}

// TestALaunchThatStartedIsNotAbandoned: Launch returning a pid with its
// error means the supervisor started and owns the item.
func TestALaunchThatStartedIsNotAbandoned(t *testing.T) {
	d, _ := queueDesk(t, false, "echo hi\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	rec.launchPID, rec.launchErr = 777, errors.New("pid not recorded")
	m = pressDesk(t, m, key("a"))
	c.add(time.Second)
	m = pressDesk(t, m, key("y"))

	if len(rec.abandons) != 0 {
		t.Fatalf("abandoned a started run: %q", rec.abandons)
	}
	if s := scanDesk(t, d); len(s.Running) != 1 || len(s.Skipped) != 0 {
		t.Fatalf("after a started launch: %+v", s)
	}
	mustContain(t, "warning", screen(t, m), "pid 777", "pid not recorded")
}

// TestATTYItemIsHandedToTheCaller: a TTY item is not claimed by the
// dashboard; it quits with the run action carrying the frozen hash.
func TestATTYItemIsHandedToTheCaller(t *testing.T) {
	d, added := queueDesk(t, true, "read -r answer\n")
	c := newClock()
	m, rec := openQueue(t, d, c, 120, 30)
	m = pressDesk(t, m, key("a"))
	mustContain(t, "title", screen(t, m), "now — in this terminal")
	c.add(time.Second)
	next, cmd := m.Update(key("y"))
	m = next.(deskModel)
	if cmd == nil {
		t.Fatal("y on a TTY item returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("y on a TTY item did not quit")
	}
	want := DeskRun{Name: added[0].Name, SHA: added[0].SHA256}
	if m.action != want {
		t.Fatalf("action = %#v, want %#v", m.action, want)
	}
	if n, _, _ := rec.counts(); n != 0 {
		t.Fatal("the dashboard claimed a TTY item")
	}
	if s := scanDesk(t, d); len(s.Pending) != 1 {
		t.Errorf("the TTY item left pending/: %+v", s)
	}
}

// TestApproveIsOnlyInTheQueue: outside the queue view a says where it is.
func TestApproveIsOnlyInTheQueue(t *testing.T) {
	d, _ := queueDesk(t, false, "echo hi\n")
	m, rec := openQueue(t, d, newClock(), 60, 30)
	m = pressDesk(t, m, key("1"))
	for _, k := range []string{"a", "s", "u"} {
		m = pressDesk(t, m, key(k))
		if len(m.modals) != 0 {
			t.Fatalf("%s opened a dialog outside the queue", k)
		}
	}
	m = pressDesk(t, m, key("a"))
	mustContain(t, "footer", screen(t, m), "approve is in the queue (6)")
	if n, _, _ := rec.counts(); n != 0 || len(rec.skips)+len(rec.unskips) != 0 {
		t.Fatal("a verb ran outside the queue")
	}
}

// TestAnItemThatCannotBeApprovedSaysWhy: a refusal, or a missing hash,
// opens no dialog.
func TestAnItemThatCannotBeApprovedSaysWhy(t *testing.T) {
	for _, it := range []runview.QueueItem{
		{Name: "0008-prune", Kind: "script", State: "waiting", SHA256: strings.Repeat("a", 64), Refusal: "recorded hash does not match the content"},
		{Name: "0009-short", Kind: "script", State: "waiting", SHA256: strings.Repeat("a", 63)},
		{Name: "0010-empty", Kind: "script", State: "waiting"},
	} {
		src := &fakeDeskSource{name: "desk", items: []runview.QueueItem{it}}
		m := openDesk(t, optsFor(src), 120, 30)
		m = pressDesk(t, m, key("a"))
		if len(m.modals) != 0 {
			t.Fatalf("%s: a dialog opened", it.Name)
		}
		mustContain(t, it.Name, screen(t, m), "cannot approve")
	}
}

// TestTheDialogShowsWhatItRuns: the title, the counts with invisible and
// non-ASCII runes counted apart, a marked line with its invisible rune
// spelled out, the batch note, and the footer.
func TestTheDialogShowsWhatItRuns(t *testing.T) {
	body := "ls\u3164 -la\necho plain\nprintf 'caf\u00e9'\n"
	content := []byte(body)
	it := runview.QueueItem{Name: "0011-look", Kind: "script", State: "waiting", What: "list things", Why: "asked",
		SHA256: strings.Repeat("b", 64), Bytes: len(content)}
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		it.Body = append(it.Body, l)
		it.Marks = append(it.Marks, runview.MarkLine(l))
	}
	src := &fakeDeskSource{name: "desk", items: []runview.QueueItem{it}}
	m := openDesk(t, optsFor(src), 120, 30)
	m = pressDesk(t, m, key("a"))
	out := screen(t, m)
	mustContain(t, "dialog", out,
		"Run 0011-look now — in the background",
		"kind script", "WHAT list things", "WHY  asked",
		"3 lines · "+strconv.Itoa(len(content))+" bytes · 0 escaped runes · 1 invisible · 2 non-ASCII · ! marks 2 lines",
		"1!│ls\\u3164 -la", "2 │echo plain", "3!│printf",
		"y approve · n/esc cancel")
	if strings.Contains(out, "hash covers") {
		t.Error("a script shows the batch note")
	}

	it.Kind = "batch"
	src = &fakeDeskSource{name: "desk", items: []runview.QueueItem{it}}
	m = pressDesk(t, openDesk(t, optsFor(src), 120, 30), key("a"))
	mustContain(t, "batch", screen(t, m), "hash covers the manifest's lines, not the scripts it calls")
}

// TestSkipAndUnskip: s skips a waiting item as the operator, u undoes it,
// and u on any other reason shows the hint without calling the desk.
func TestSkipAndUnskip(t *testing.T) {
	d, added := queueDesk(t, false, "echo hi\n")
	m, rec := openQueue(t, d, newClock(), 120, 30)
	name := added[0].Name

	m = pressDesk(t, m, key("s"))
	mustContain(t, "skip", screen(t, m), "skipped "+name+" · u to undo")
	if s := scanDesk(t, d); len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != desk.SkipOperator {
		t.Fatalf("after s: %+v", s)
	}
	m = relistDesk(t, m)
	m = pressDesk(t, m, key("u"))
	if s := scanDesk(t, d); len(s.Pending) != 1 {
		t.Fatalf("after u: %+v", s)
	}
	m = relistDesk(t, m)

	// A changed item cannot be re-armed.
	rewrite(t, d, name, "echo changed\n", false)
	m = relistDesk(t, m)
	if it, ok := m.selectedItem(); !ok || it.SkipReason != desk.SkipChanged {
		t.Fatalf("selected %+v, want the changed skip", it)
	}
	m = pressDesk(t, m, key("u"))
	mustContain(t, "hint", screen(t, m), "changed — re-queue to run it")
	if len(rec.unskips) != 1 {
		t.Fatalf("unskips = %q, want only the operator one", rec.unskips)
	}
	m = pressDesk(t, m, key("s"))
	mustContain(t, "skip a skipped item", screen(t, m), "only a waiting or lost item can be skipped")
	if len(rec.skips) != 1 {
		t.Fatalf("skips = %q", rec.skips)
	}
}

// skipOnlyVerbs accepts every Skip and records the name; the other verbs
// are not reached.
type skipOnlyVerbs struct {
	deskVerbs
	skips []string
}

func (s *skipOnlyVerbs) Skip(name, _ string) error {
	s.skips = append(s.skips, name)
	return nil
}

// TestSkipLostOffersNoUndo: a lost item was claimed, so Unskip refuses it,
// and the footer after s does not offer u.
func TestSkipLostOffersNoUndo(t *testing.T) {
	src := &fakeDeskSource{name: "desk", items: []runview.QueueItem{{Name: "0006-sweep", Kind: "batch", State: "lost", What: "sweep"}}}
	m := openDesk(t, optsFor(src), 120, 30)
	if m.sideShows() != viewQueue {
		m = pressDesk(t, m, key("6"))
	}
	verbs := &skipOnlyVerbs{}
	m.verbs = verbs
	m = pressDesk(t, m, key("s"))
	if len(verbs.skips) != 1 || verbs.skips[0] != "0006-sweep" {
		t.Fatalf("skips = %q, want the lost item", verbs.skips)
	}
	out := screen(t, m)
	mustContain(t, "skip lost", out, "skipped 0006-sweep")
	if strings.Contains(out, "u to undo") {
		t.Errorf("skipping a lost item offers u to undo:\n%s", out)
	}
}
