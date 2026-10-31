package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
)

var workersNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

const workersHead = "37af20656a1b735002881032cf54f2a0ede6d244"

func workersFixture() workerwatch.Snapshot {
	return workerwatch.Snapshot{
		Taken: workersNow,
		Workers: []workerwatch.Worker{
			{
				Name: "cos-alpha-fix", Address: "t1n", State: "live", Status: workerwatch.StatusWorking, AgeSeconds: 754, Model: "opus",
				BriefDir: "/b/a", Worktree: "/wt/a", Source: "file",
				Reports: workerwatch.Reports{Files: []workerwatch.ReportFile{{Name: "report.md"}}},
				PR: &workerwatch.PR{Repo: "o/r", Number: 1384, State: "OPEN", Draft: true, Head: workersHead, Checks: "SUCCESS",
					Marker: &workerwatch.Marker{Reviewer: "pr-reviewer", Head: workersHead, Crit: 0, Imp: 3, AtHead: true}},
				PRAt: workersNow,
			},
			{
				Name: "cos-beta-docs", Address: "t1y", State: "live", Status: workerwatch.StatusBlocked, AgeSeconds: 7200, Model: "sonnet",
				BriefDir: "/b/b", Worktree: "/wt/b", Source: "file", PRIssue: workerwatch.PRIssueNoPR, PRNote: "no PR yet", PRAt: workersNow,
			},
			{
				Name: "cos-gamma", Address: "t20", State: "live", Status: workerwatch.StatusDone, AgeSeconds: 40,
				BriefDir: "/b/c", Worktree: "/wt/c", Source: "hc",
				Reports: workerwatch.Reports{Files: []workerwatch.ReportFile{{Name: "report-round2.md", Complete: true}, {Name: "report.md", Complete: true}}},
				PR:      &workerwatch.PR{Repo: "o/r", Number: 1390, State: "OPEN", Head: workersHead, Checks: "FAILURE"},
				PRAt:    workersNow,
			},
			{Name: "cos-gone", HCError: "no hc terminal by this name", Source: "file"},
		},
		Notes: []string{"line 7: want name|worktree|brief dir"},
	}
}

type workersFakes struct {
	mu       sync.Mutex
	collects []bool
	reads    []string
	screen   string
}

func (f *workersFakes) collect(_ context.Context, force bool) workerwatch.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collects = append(f.collects, force)
	return workersFixture()
}

func (f *workersFakes) read(_ context.Context, addr string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, addr)
	return f.screen, nil
}

func newWorkersTest(t *testing.T, f *workersFakes, events func(context.Context) <-chan struct{}) workersModel {
	t.Helper()
	m := newWorkersModel(context.Background(), WorkersOptions{
		Collect: f.collect, Read: f.read, Events: events, Poll: 5 * time.Second, Theme: theme.Default(),
	})
	m.now = func() time.Time { return workersNow }
	out, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	return out.(workersModel)
}

// feed runs cmd and every message it yields that the model knows, one level
// of batching deep, skipping timers (they would sleep).
func feed(t *testing.T, m workersModel, msg tea.Msg) workersModel {
	t.Helper()
	out, cmd := m.Update(msg)
	m = out.(workersModel)
	for _, c := range flatten(cmd) {
		if r := c(); r != nil {
			switch r.(type) {
			case workersSnapMsg, workersTailMsg:
				out, _ = m.Update(r)
				m = out.(workersModel)
			}
		}
	}
	return m
}

// flatten expands a batch without running timers: only commands that return
// at once are kept, which is every collect and read command here.
func flatten(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if b, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Cmd
			for _, c := range b {
				out = append(out, flatten(c)...)
			}
			return out
		}
		return []tea.Cmd{func() tea.Msg { return msg }}
	case <-time.After(50 * time.Millisecond):
		return nil // a timer
	}
}

func loadedWorkers(t *testing.T, f *workersFakes) workersModel {
	t.Helper()
	m := newWorkersTest(t, f, nil)
	return feed(t, m, workersSnapMsg{snap: workersFixture()})
}

func TestWorkersGolden(t *testing.T) {
	forceTrueColor(t)
	f := &workersFakes{screen: "\x1b[1mBuilding\x1b[0m internal/tui\n✻ Cogitating… (12s · esc to interrupt)\n\n"}
	m := loadedWorkers(t, f)
	assertGolden(t, "workers", m.View().Content)
}

func TestWorkersRowsShowEveryField(t *testing.T) {
	f := &workersFakes{screen: "hello from the worker"}
	m := loadedWorkers(t, f)
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{
		"cos-alpha-fix", "t1n", "working", "12m", "opus", "0/1 report.md …", "#1384 37af206", "✓ pass", "pr-reviewer crit=0 imp=3 @head",
		"BLOCKED", "no PR yet",
		"2/2 report-round2.md ✓", "✗ fail", "no marker",
		"gone",
		"STATUS*", "status is the agent's own report",
		"note: line 7",
		"── screen: cos-alpha-fix (t1n)", "hello from the worker",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("view is missing %q\n%s", want, plain)
		}
	}
	if !strings.Contains(plain, "polling every 5s") {
		t.Errorf("a model with no event stream should say it polls\n%s", plain)
	}
}

func TestWorkersNeedsYouBanner(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{"1 need you", "NEEDS YOU  cos-beta-docs is blocked", "hc read t1y", "hc attach t1y"} {
		if !strings.Contains(plain, want) {
			t.Errorf("banner is missing %q\n%s", want, plain)
		}
	}
	calm := workersFixture()
	calm.Workers[1].Status = workerwatch.StatusIdle
	out, _ := m.Update(workersSnapMsg{snap: calm})
	if strings.Contains(ansi.Strip(out.(workersModel).View().Content), "NEEDS YOU") {
		t.Error("the banner stayed after the worker stopped being blocked")
	}
}

func TestWorkersHostileScreenIsInert(t *testing.T) {
	hostile := strings.Join([]string{
		"ok line",
		"\x1b]52;c;cm0gLXJmIH4K\x07clipboard",
		"\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\",
		"\x1b[2J\x1b[Hclear",
		"\x1b[?1049lleave alt screen",
		"bidi \u202eevil\u202c done",
		"bell\x07 and bare esc \x1b and del \x7f",
		"\x1bP+q544e\x1b\\dcs",
	}, "\n")
	m := loadedWorkers(t, &workersFakes{screen: hostile})
	view := m.View().Content
	for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[H", "\x1b[?1049", "\u202e", "\u202c", "\x1bP", "\x7f", "evil.example"} {
		if strings.Contains(view, bad) {
			t.Errorf("rendered view still holds %q", bad)
		}
	}
	plain := ansi.Strip(view)
	for _, want := range []string{"clipboard", "click", "clear", `bidi \u202eevil\u202c done`, "ok line"} {
		if !strings.Contains(plain, want) {
			t.Errorf("sanitized tail lost the visible text %q\n%s", want, plain)
		}
	}
	// Only SGR (…m) sequences, from the view's own styles, may remain.
	for i := 0; i < len(view); i++ {
		if view[i] != 0x1b {
			continue
		}
		j := i + 1
		if j >= len(view) || view[j] != '[' {
			t.Fatalf("non-CSI escape at %d: %q", i, view[i:min(len(view), i+12)])
		}
		for j++; j < len(view) && (view[j] >= '0' && view[j] <= '9' || view[j] == ';' || view[j] == ':'); j++ {
		}
		if j >= len(view) || view[j] != 'm' {
			t.Fatalf("non-SGR CSI at %d: %q", i, view[i:min(len(view), i+12)])
		}
	}
}

func TestSanitizeScreen(t *testing.T) {
	got := SanitizeScreen("a\r\n\x1b[31mred\x1b[0m\tx\n\xff\n\n  \n")
	want := []string{"a", "red    x", "�"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("SanitizeScreen = %q, want %q", got, want)
	}
	long := strings.Repeat("line\n", workersMaxTailLines*3)
	if n := len(SanitizeScreen(long)); n != workersMaxTailLines {
		t.Errorf("kept %d lines, want %d", n, workersMaxTailLines)
	}
}

func TestWorkersSelectionReadsOnlyTheSelectedScreen(t *testing.T) {
	f := &workersFakes{screen: "s"}
	m := loadedWorkers(t, f)
	if len(f.reads) != 1 || f.reads[0] != "t1n" {
		t.Fatalf("first load read %v, want [t1n]", f.reads)
	}
	m = feed(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if f.reads[len(f.reads)-1] != "t1y" {
		t.Errorf("moving down read %v, want t1y last", f.reads)
	}
	m = feed(t, m, key("j"))
	m = feed(t, m, key("j"))
	before := len(f.reads)
	m = feed(t, m, key("j")) // already on the last row
	if len(f.reads) != before {
		t.Errorf("a key that moved nothing read a screen: %v", f.reads)
	}
	if m.cursor != 3 {
		t.Fatalf("cursor = %d, want 3", m.cursor)
	}
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "no hc terminal by this name") {
		t.Errorf("a gone worker's tail should say why\n%s", plain)
	}
	if f.reads[len(f.reads)-1] == "" {
		t.Error("read was called with an empty address")
	}
}

func TestWorkersRefreshKeyForces(t *testing.T) {
	f := &workersFakes{}
	m := loadedWorkers(t, f)
	m = feed(t, m, key("r"))
	if len(f.collects) != 1 || !f.collects[0] {
		t.Errorf("r ran collects %v, want one forced", f.collects)
	}
	_, cmd := m.Update(key("q"))
	if cmd == nil {
		t.Fatal("q did nothing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q did not quit")
	}
}

func TestWorkersEventsCoalesceAndFallBack(t *testing.T) {
	ch := make(chan struct{}, 1)
	f := &workersFakes{}
	m := newWorkersTest(t, f, func(context.Context) <-chan struct{} { return ch })
	if !m.busy {
		t.Fatal("a new model should be busy with its first collect")
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "live (hc events)") {
		t.Error("header does not say events are live")
	}
	m = feed(t, m, workersSnapMsg{snap: workersFixture()})

	// An event inside the gap after the last start queues, not collects.
	out, _ := m.Update(workersEventMsg{})
	m = out.(workersModel)
	if !m.pending || m.busy {
		t.Fatalf("event within %s: pending=%v busy=%v, want queued", workersEventGap, m.pending, m.busy)
	}
	// Past the gap, the kick collects once.
	m.now = func() time.Time { return workersNow.Add(2 * workersEventGap) }
	m = feed(t, m, workersKickMsg{})
	if len(f.collects) != 1 || m.pending {
		t.Fatalf("kick: collects=%v pending=%v, want one collect", f.collects, m.pending)
	}

	// An event while a collect runs only queues.
	m.busy = true
	out, _ = m.Update(workersEventMsg{})
	m = out.(workersModel)
	if !m.pending || len(f.collects) != 1 {
		t.Fatalf("event while busy: pending=%v collects=%v", m.pending, f.collects)
	}
	m.busy = false
	m.pending = false

	m = feed(t, m, workersEventsEnd{})
	if m.eventsUp {
		t.Fatal("the stream ended but the model still thinks events are live")
	}
	if len(f.collects) != 2 {
		t.Errorf("stream end should collect at once; collects=%v", f.collects)
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "polling every 5s") {
		t.Error("header does not say it fell back to polling")
	}
}

func TestWorkersEmpty(t *testing.T) {
	f := &workersFakes{}
	m := newWorkersTest(t, f, nil)
	if !strings.Contains(ansi.Strip(m.View().Content), "reading workers") {
		t.Error("before the first snapshot the view should say it is reading")
	}
	out, _ := m.Update(workersSnapMsg{snap: workerwatch.Snapshot{Taken: workersNow}})
	if !strings.Contains(ansi.Strip(out.(workersModel).View().Content), "no workers") {
		t.Error("an empty snapshot should say there are no workers")
	}
}

func TestWorkersSelectionFollowsTheWorkerAcrossRefreshes(t *testing.T) {
	f := &workersFakes{}
	m := loadedWorkers(t, f)
	m = feed(t, m, key("j")) // cos-beta-docs
	grown := workersFixture()
	grown.Workers = append([]workerwatch.Worker{{Name: "cos-aaa-new", Address: "t9", State: "live", Status: workerwatch.StatusWorking}}, grown.Workers...)
	out, _ := m.Update(workersSnapMsg{snap: grown})
	if w, _ := out.(workersModel).selected(); w.Name != "cos-beta-docs" {
		t.Errorf("after a row was added above, the selection moved to %s", w.Name)
	}
}

func TestWorkersBannerIsCapped(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	many := workersFixture()
	for i := range 6 {
		many.Workers = append(many.Workers, workerwatch.Worker{Name: fmt.Sprintf("cos-blk-%d", i), Address: fmt.Sprintf("t%d", 50+i), State: "live", Status: workerwatch.StatusBlocked})
	}
	out, _ := m.Update(workersSnapMsg{snap: many})
	lines := out.(workersModel).bannerLines()
	if len(lines) != workersMaxBanners {
		t.Fatalf("banner has %d lines, want %d", len(lines), workersMaxBanners)
	}
	if !strings.Contains(ansi.Strip(lines[len(lines)-1]), "5 more blocked workers") {
		t.Errorf("last banner line = %q, want a count of the rest", ansi.Strip(lines[len(lines)-1]))
	}
	view := out.(workersModel).View().Content
	if n := strings.Count(view, "\n") + 1; n > 30 {
		t.Errorf("view is %d lines on a 30-line terminal", n)
	}
}

func TestWorkersStalePRAfterGHError(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	stale := workersFixture()
	stale.Workers[0].PRIssue, stale.Workers[0].PRNote = workerwatch.PRIssueGH, "the PR could not be read: rate limited"
	stale.Workers[2].PR, stale.Workers[2].PRIssue = nil, workerwatch.PRIssueGH
	out, _ := m.Update(workersSnapMsg{snap: stale})
	plain := ansi.Strip(out.(workersModel).View().Content)
	if !strings.Contains(plain, "#1384 37af206 !") {
		t.Errorf("a kept PR after a failed read is not marked stale\n%s", plain)
	}
	if !strings.Contains(plain, "gh error") {
		t.Errorf("a failed first read does not say gh error\n%s", plain)
	}
}

func TestWorkersFinishedBlockedIsNotNeedsYou(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	snap := workersFixture()
	snap.Workers[1].State = "finished"
	out, _ := m.Update(workersSnapMsg{snap: snap})
	plain := ansi.Strip(out.(workersModel).View().Content)
	if strings.Contains(plain, "NEEDS YOU") || strings.Contains(plain, "need you") {
		t.Errorf("an exited terminal whose last report was blocked raised NEEDS YOU\n%s", plain)
	}
}
