package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
					Review: &workerwatch.Review{AtHead: 2, Crit: 0, Imp: 3, Reviewers: []string{"pr-reviewer", "security-reviewer"}}},
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

func newWorkersTest(t *testing.T, f *workersFakes, wake <-chan struct{}) workersModel {
	t.Helper()
	var stream *workerwatch.EventStream
	if wake != nil {
		stream = &workerwatch.EventStream{Wake: wake}
	}
	m := newWorkersModel(context.Background(), WorkersOptions{
		Collect: f.collect, Read: f.read, Poll: 5 * time.Second, Theme: theme.Default(),
	}, stream)
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

// The workers goldens store raw truecolor SGR, so a palette change reddens
// them too. Regenerate with: go test ./internal/tui -run WorkersGolden -update
func TestWorkersGolden(t *testing.T) {
	forceTrueColor(t)
	for _, size := range []struct {
		name string
		w, h int
	}{{"workers", 140, 30}, {"workers_120x30", 120, 30}, {"workers_80x24", 80, 24}} {
		f := &workersFakes{screen: "\x1b[1mBuilding\x1b[0m internal/tui\n✻ Cogitating… (12s · esc to interrupt)\n\n"}
		m := loadedWorkers(t, f)
		out, _ := m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		assertGolden(t, size.name, out.(workersModel).View().Content)
	}
}

func TestWorkersRowsShowEveryField(t *testing.T) {
	f := &workersFakes{screen: "hello from the worker"}
	m := loadedWorkers(t, f)
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{
		"cos-alpha-fix", "t1n", "working", "12m", "opus", "0/1 report.md …", "#1384 37af206", "✓ pass", "c0 i3 ×2",
		"BLOCKED", "no PR yet",
		"2/2 report-round2.md ✓", "✗ fail", "none",
		"gone",
		"STATUS*", "REVIEW*", "* = a claim, see ?",
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
	m := newWorkersTest(t, f, ch)
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

	// The safety tick scheduled while events flowed belongs to the old
	// chain: it is dropped, and the polling chain's tick collects.
	m = feed(t, m, workersTickMsg{gen: 0})
	if len(f.collects) != 2 {
		t.Errorf("the safety tick from before the stream ended still collected; collects=%v", f.collects)
	}
	m = feed(t, m, workersTickMsg{gen: m.tickGen})
	if len(f.collects) != 3 {
		t.Errorf("the polling chain's tick did not collect; collects=%v", f.collects)
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
	lines := out.(workersModel).bannerLines(200, 30)
	if len(lines) != workersMaxBanners {
		t.Fatalf("banner has %d lines, want %d", len(lines), workersMaxBanners)
	}
	if !strings.Contains(ansi.Strip(lines[len(lines)-1]), "5 more blocked: cos-blk-1, cos-blk-2, cos-blk-3, …") {
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

func TestStopStreamWaitsForTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reaped := make(chan struct{})
	go func() { // a child that only exits once its context is cancelled
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(reaped)
		close(done)
	}()
	if !stopStream(cancel, done, time.Second) {
		t.Fatal("stopStream gave up on a child that exits after cancel")
	}
	select {
	case <-reaped:
	default:
		t.Fatal("stopStream returned before the child was reaped")
	}
	if stopStream(func() {}, make(chan struct{}), 20*time.Millisecond) {
		t.Error("stopStream reported a child that never exits as reaped")
	}
}

func sizedWorkers(t *testing.T, w, h int) workersModel {
	t.Helper()
	m := loadedWorkers(t, &workersFakes{screen: "s"})
	out, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return out.(workersModel)
}

func TestWorkersNarrowKeepsVerdictColumns(t *testing.T) {
	for _, w := range []int{80, 60} {
		plain := ansi.Strip(sizedWorkers(t, w, 24).View().Content)
		for _, want := range []string{"STATUS*", "REVIEW*", "c0 i3", "BLOCKED", "→ ", "q quit", "hc read t1y"} {
			if !strings.Contains(plain, want) {
				t.Errorf("at %d columns the view lost %q\n%s", w, want, plain)
			}
		}
		for i, line := range strings.Split(plain, "\n") {
			if lw := lipgloss.Width(line); lw > w {
				t.Errorf("at %d columns line %d is %d wide", w, i, lw)
			}
		}
	}
	wide := ansi.Strip(sizedWorkers(t, 200, 30).View().Content)
	if strings.Contains(wide, "more columns") {
		t.Errorf("a 200-column view still hides columns\n%s", wide)
	}
	for _, want := range []string{"MODEL", "HC", "report-round2.md"} {
		if !strings.Contains(wide, want) {
			t.Errorf("a wide view lost %q", want)
		}
	}
}

func TestWorkersLayoutDropsLowPriorityFirst(t *testing.T) {
	cols, hidden := workersLayout(80)
	kinds := map[int]bool{}
	for _, c := range cols {
		kinds[c.kind] = true
	}
	for _, k := range []int{colName, colStatus, colReview, colCI} {
		if !kinds[k] {
			t.Errorf("80 columns dropped a verdict column (kind %d): %+v", k, cols)
		}
	}
	if kinds[colModel] || hidden == 0 {
		t.Errorf("80 columns kept MODEL or hid nothing: %+v hidden=%d", cols, hidden)
	}
	used := 2
	for _, c := range cols {
		used += c.width + 1
	}
	if used+workersHintWidth > 80+1 {
		t.Errorf("the kept columns (%d) leave no room for the hint", used)
	}
}

func TestWorkersWideNamesKeepColumnsAligned(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	snap := workersFixture()
	snap.Workers[0].Name = "cos-" + strings.Repeat("界", 30)         // 60 cells
	snap.Workers[2].Name = "cos-" + strings.Repeat("long-name-", 5) // 54 runes
	out, _ := m.Update(workersSnapMsg{snap: snap})
	out, _ = out.(workersModel).Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	lines := strings.Split(ansi.Strip(out.(workersModel).View().Content), "\n")
	col := func(line, sub string) int {
		i := strings.Index(line, sub)
		if i < 0 {
			return -1
		}
		return lipgloss.Width(line[:i])
	}
	var header string
	for _, l := range lines {
		if strings.Contains(l, "STATUS*") {
			header = l
		}
	}
	want := col(header, "STATUS*")
	for _, l := range lines {
		for _, st := range []string{"working", "done "} {
			if c := col(l, st); c >= 0 && strings.Contains(l, "cos-") && c != want {
				t.Errorf("status %q starts at cell %d, header at %d:\n%s", st, c, want, l)
			}
		}
	}
}

func TestWorkersHelpAndEsc(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	out, cmd := m.Update(key("?"))
	if cmd != nil {
		t.Error("? returned a command")
	}
	plain := ansi.Strip(out.(workersModel).View().Content)
	for _, want := range []string{"keys and what the columns mean", "summed the way forgectl sweep sums them", "only reads"} {
		if !strings.Contains(plain, want) {
			t.Errorf("help is missing %q", want)
		}
	}
	out, cmd = out.(workersModel).Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("esc quit the watch")
		}
	}
	if out.(workersModel).help {
		t.Error("esc did not close help")
	}
	if !strings.Contains(ansi.Strip(out.(workersModel).headerLine(200)), "q quit") {
		t.Error("the header does not carry the quit key")
	}
}

func TestWorkersShortTerminal(t *testing.T) {
	for _, h := range []int{11, 8} {
		v := sizedWorkers(t, 120, h).View().Content
		if n := strings.Count(v, "\n") + 1; n > h {
			t.Errorf("a %d-row terminal got %d lines", h, n)
		}
		if !strings.Contains(ansi.Strip(v), "q quit") {
			t.Errorf("a %d-row terminal lost the quit key", h)
		}
	}
	if !strings.Contains(ansi.Strip(sizedWorkers(t, 120, 5).View().Content), "too small: need 30x7") {
		t.Error("a 5-row terminal does not say it is too small")
	}
}

// RunWorkers must not return until the hc events child it started has been
// cancelled and reaped: the stub's Done closes only after its context ends,
// with a delay standing in for the kill and wait.
func TestRunWorkersReapsTheStreamBeforeReturning(t *testing.T) {
	var streamCtx context.Context
	done := make(chan struct{})
	reaped := false
	var mu sync.Mutex
	opts := WorkersOptions{
		Collect: (&workersFakes{}).collect,
		Theme:   theme.Default(),
		Events: func(ctx context.Context) workerwatch.EventStream {
			streamCtx = ctx
			go func() {
				<-ctx.Done()
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				reaped = true
				mu.Unlock()
				close(done)
			}()
			return workerwatch.EventStream{Wake: make(chan struct{}), Done: done}
		},
	}
	quit := func(context.Context, tea.Model) error { return nil } // the operator pressed q
	if err := runWorkers(context.Background(), opts, quit); err != nil {
		t.Fatal(err)
	}
	if streamCtx.Err() == nil {
		t.Error("RunWorkers returned without cancelling the event stream")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reaped {
		t.Error("RunWorkers returned before the hc events child was reaped")
	}
}

func TestWorkersHelpFitsEightyColumns(t *testing.T) {
	m := sizedWorkers(t, 80, 30)
	m.help = true
	for i, line := range m.helpLines() {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("help line %d is %d wide: %q", i, w, ansi.Strip(line))
		}
	}
}

func TestWorkersLayoutIsStrictPriority(t *testing.T) {
	for w := 20; w <= 220; w++ {
		cols, _ := workersLayout(w)
		kept := map[int]bool{}
		for _, c := range cols {
			kept[c.prio] = true
		}
		for p := 1; p <= len(workersAllCols); p++ {
			if !kept[p] {
				for q := p + 1; q <= len(workersAllCols); q++ {
					if kept[q] {
						t.Fatalf("at %d columns priority %d was dropped but %d kept: %+v", w, p, q, cols)
					}
				}
				break
			}
		}
	}
	if cols, _ := workersLayout(42); len(cols) < 2 || cols[1].kind != colStatus {
		t.Errorf("at 42 columns STATUS is not kept beside NAME: %+v", cols)
	}
}

func TestWorkersCutTableSaysSo(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	many := workersFixture()
	for i := range 20 {
		many.Workers = append(many.Workers, workerwatch.Worker{Name: fmt.Sprintf("cos-w-%02d", i), State: "live", Status: workerwatch.StatusWorking})
	}
	snapped, _ := m.Update(workersSnapMsg{snap: many})
	// The cue must survive at every width down to the narrowest supported
	// one, and on the shortest supported screen, not only on a wide one.
	for _, size := range [][2]int{{140, 24}, {80, 24}, {80, 8}, {60, 12}, {workersMinCols, 24}, {workersMinCols, workersMinRows}} {
		out, _ := snapped.(workersModel).Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansi.Strip(out.(workersModel).View().Content)
		if !strings.Contains(view, "more rows") {
			t.Errorf("%dx%d: a table cut to fit the screen does not say so in full:\n%s", size[0], size[1], view)
		}
		for i, line := range strings.Split(view, "\n") {
			if w := lipgloss.Width(line); w > size[0] {
				t.Errorf("%dx%d: line %d is %d wide", size[0], size[1], i, w)
			}
		}
	}
	if strings.Contains(ansi.Strip(sizedWorkers(t, 140, 40).View().Content), "more rows") {
		t.Error("a table that fits says rows are cut")
	}
}

func TestWorkersCutCountsAreSingularAware(t *testing.T) {
	if got := plural(1, "more row", "more rows"); got != "1 more row" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(3, "more row", "more rows"); got != "3 more rows" {
		t.Errorf("plural(3) = %q", got)
	}
}

func TestWorkersBelowTheMinimumSaysSo(t *testing.T) {
	for _, size := range [][2]int{{workersMinCols - 1, 24}, {120, workersMinRows - 1}} {
		if view := ansi.Strip(sizedWorkers(t, size[0], size[1]).View().Content); !strings.Contains(view, "too small") {
			t.Errorf("%dx%d does not say the terminal is too small:\n%s", size[0], size[1], view)
		}
	}
}

// The header shortens, then drops whole parts from its end: it never ends
// mid-word, and at 80 columns the freshness part still shows.
func TestWorkersHeaderNeverEndsMidWord(t *testing.T) {
	m := sizedWorkers(t, 200, 30)
	ends := []string{"help", "workers", "need you", "live", "(hc events)", "5s", "ago", "…"}
	for width := workersMinCols; width <= 200; width++ {
		h := ansi.Strip(m.headerLine(width))
		if w := lipgloss.Width(h); w > width {
			t.Fatalf("width %d: header is %d wide: %q", width, w, h)
		}
		whole := false
		for _, e := range ends {
			whole = whole || strings.HasSuffix(h, e)
		}
		if !whole {
			t.Errorf("width %d: header ends mid-part: %q", width, h)
		}
	}
	if h := ansi.Strip(m.headerLine(80)); !strings.Contains(h, "ago") {
		t.Errorf("at 80 columns the header loses its freshness: %q", h)
	}
}

// TestWorkersStatusCellTellsGoneFromUnread: only hc's not_found is "gone".
func TestWorkersStatusCellTellsGoneFromUnread(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	for _, tt := range []struct {
		w    workerwatch.Worker
		want string
	}{
		{workerwatch.Worker{HCError: "no hc terminal by this name"}, "gone"},
		{workerwatch.Worker{HCError: "hc could not read this terminal"}, "unread"},
	} {
		if got, _ := m.statusCell(tt.w); got != tt.want {
			t.Errorf("statusCell(%q) = %q, want %q", tt.w.HCError, got, tt.want)
		}
	}
}

// TestWorkersBannerOnAShortTerminalNamesAWorker: below 14 rows the banner is
// one line, and with two workers blocked that line still names the first
// and the command that shows its dialog, with a count of the other.
func TestWorkersBannerOnAShortTerminalNamesAWorker(t *testing.T) {
	m := loadedWorkers(t, &workersFakes{})
	two := workersFixture()
	two.Workers = append(two.Workers, workerwatch.Worker{Name: "cos-blk-2", Address: "t52", State: "live", Status: workerwatch.StatusBlocked})
	out, _ := m.Update(workersSnapMsg{snap: two})
	lines := out.(workersModel).bannerLines(200, 10)
	if len(lines) != 1 {
		t.Fatalf("short-terminal banner has %d lines, want 1", len(lines))
	}
	plain := ansi.Strip(lines[0])
	for _, want := range []string{"cos-beta-docs is blocked", "hc read t1y", "+1 more blocked"} {
		if !strings.Contains(plain, want) {
			t.Errorf("short-terminal banner %q lacks %q", plain, want)
		}
	}
}
