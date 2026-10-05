package tui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// deskNow is every desk fixture's clock: a fixed instant, in UTC, so a frame
// is the same bytes on every machine.
var deskNow = time.Date(2026, 10, 5, 19, 7, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return deskNow.Add(-d) }

func agoPtr(d time.Duration) *time.Time { t := ago(d); return &t }

func rcPtr(rc int) *int { return &rc }

func deskOpts() DeskFrameOptions {
	th := theme.Default()
	return DeskFrameOptions{
		Host:    "host-a",
		Version: "0.0.0-test",
		Dir:     "~/.local/state/forgectl/desk",
		Started: ago(4*time.Hour + 37*time.Minute),
		Theme:   &th,
	}
}

func item(name string, kind desk.Kind, state desk.State) desk.Item {
	n, stem := desk.SplitName(name)
	return desk.Item{Name: name, Number: n, Stem: stem, Kind: kind, State: state}
}

const busyScript = `#!/bin/bash
# WHAT: Merge the feature branch once checks pass
# WHY: The review is done; merges are yours
set -euo pipefail

gh pr merge 42 --repo example/project --squash --auto
gh pr view 42 --repo example/project --json state
echo merged
	echo "an indented line"
`

const busyManifest = `# WHAT: Rebuild the nightly artifacts
# WHY: The cache was cleared
fetch -- echo fetch
lint after=fetch -- echo lint
test after=fetch -- echo test
build after=lint,test -- echo build
docs after=fetch -- echo docs
pack after=build -- echo pack
publish after=pack -- echo publish
`

// busySnapshot has one of everything: a waiting script, a TTY item, a stale
// item, a running batch, a running script with history, a finished run, a
// failed run, and an item that changed after it was queued.
func busySnapshot() (*desk.Snapshot, DeskFrameOptions) {
	merge := item("17-merge-feature-branch", desk.KindScript, desk.StateWaiting)
	merge.Headers = desk.ParseHeaders([]byte(busyScript))
	merge.Content = []byte(busyScript)
	merge.Meta = desk.Meta{AddedAt: agoPtr(12 * time.Minute), SHA256: desk.SHA256Hex([]byte("merge"))}

	tty := item("18-refresh-credentials", desk.KindScript, desk.StateWaiting)
	tty.Content = []byte("#!/bin/bash\n# WHAT: Refresh the sudo ticket\n# WHY: The next step needs root\n# TTY: yes\nsudo -v\n")
	tty.Headers = desk.ParseHeaders(tty.Content)
	tty.Meta = desk.Meta{AddedAt: agoPtr(5 * time.Minute), SHA256: desk.SHA256Hex([]byte("tty"))}

	stale := item("19-old-cleanup", desk.KindScript, desk.StateWaiting)
	stale.Content = []byte("# WHAT: Clear old caches\n# WHY: Disk is filling\nrm -rf ./cache\n")
	stale.Headers = desk.ParseHeaders(stale.Content)
	stale.Meta = desk.Meta{AddedAt: agoPtr(30 * time.Hour), SHA256: desk.SHA256Hex([]byte("stale"))}
	stale.Stale = true

	batch := item("16-nightly-batch", desk.KindBatch, desk.StateRunning)
	batch.Headers = desk.ParseHeaders([]byte(busyManifest))
	batch.Started = ago(3 * time.Minute)
	batch.Meta = desk.Meta{AddedAt: agoPtr(20 * time.Minute), StartedAt: &batch.Started}

	sync := item("14-sync-mirror", desk.KindScript, desk.StateRunning)
	sync.Headers = desk.Headers{What: "Sync the package mirror", Why: "Upstream published a release"}
	sync.Started = ago(50 * time.Second)

	ok := item("15-merge-1169", desk.KindScript, desk.StateDone)
	ok.Headers = desk.Headers{What: "Merge 1169", Why: "Checks are green"}
	ok.Started, ok.Ended, ok.ExitCode = ago(23*time.Minute+41*time.Second), ago(23*time.Minute), rcPtr(0)
	ok.Meta.AddedAt = agoPtr(40 * time.Minute)

	failed := item("12-canary-probe", desk.KindScript, desk.StateDone)
	failed.Headers = desk.Headers{What: "Probe the canary", Why: "Confirm the fix"}
	failed.Started, failed.Ended, failed.ExitCode = ago(20*time.Minute+18*time.Second), ago(20*time.Minute), rcPtr(1)

	prev := item("09-sync-mirror", desk.KindScript, desk.StateDone)
	prev.Started, prev.Ended, prev.ExitCode = ago(5*time.Hour+2*time.Minute), ago(5*time.Hour), rcPtr(0)

	changed := item("13-rotate-keys", desk.KindScript, desk.StateSkipped)
	changed.Headers = desk.Headers{What: "Rotate the deploy keys", Why: "Quarterly rotation"}
	changed.Meta = desk.Meta{AddedAt: agoPtr(2 * time.Hour), SkipReason: desk.SkipChanged}

	snap := &desk.Snapshot{
		Dir:     "/home/op/.local/state/forgectl/desk",
		Taken:   deskNow,
		Pending: []desk.Item{merge, tty, stale},
		Running: []desk.Item{sync, batch},
		Done:    []desk.Item{failed, ok, prev},
		Skipped: []desk.Item{changed},
	}
	opts := deskOpts()
	opts.Steps = map[string][]desk.StepStatus{"16-nightly-batch": {
		{ID: "fetch", State: desk.StepOK}, {ID: "lint", State: desk.StepOK},
		{ID: "test", State: desk.StepOK}, {ID: "build", State: desk.StepRunning},
		{ID: "docs", State: desk.StepOK}, {ID: "pack", State: desk.StepPending},
		{ID: "publish", State: desk.StepPending},
	}}
	opts.Records = map[string][]byte{"16-nightly-batch": []byte(busyManifest)}
	return snap, opts
}

// legacySnapshot is a desk the old Bash desk left behind: 44 finished runs
// and no meta. 40 logs end in EXIT= (four of them failures) and 4 do not.
func legacySnapshot() (*desk.Snapshot, DeskFrameOptions) {
	snap := &desk.Snapshot{Dir: "/home/op/.local/state/forgectl/desk", Taken: deskNow}
	for i := 44; i >= 1; i-- {
		it := item(fmt.Sprintf("%02d-legacy-task-%02d", i, i), desk.KindScript, desk.StateDone)
		it.Ended = ago(time.Duration(45-i) * 3 * time.Hour)
		it.Started = it.Ended.Add(-time.Duration(10+i) * time.Second)
		switch {
		case i%11 == 0:
			// no EXIT= line: ExitCode stays nil
		case i%10 == 0:
			it.ExitCode = rcPtr(i / 10)
		default:
			it.ExitCode = rcPtr(0)
		}
		snap.Done = append(snap.Done, it)
	}
	return snap, deskOpts()
}

func emptySnapshot() (*desk.Snapshot, DeskFrameOptions) {
	return &desk.Snapshot{Dir: "/home/op/.local/state/forgectl/desk", Taken: deskNow}, deskOpts()
}

var deskFixtures = []struct {
	name   string
	build  func() (*desk.Snapshot, DeskFrameOptions)
	height int
}{
	{"empty", emptySnapshot, 30},
	{"busy", busySnapshot, 40},
	{"legacy", legacySnapshot, 60},
}

func TestLegacyFixtureShape(t *testing.T) {
	snap, _ := legacySnapshot()
	withExit, without := 0, 0
	for _, it := range snap.Done {
		if it.ExitCode == nil {
			without++
		} else {
			withExit++
		}
	}
	if withExit != 40 || without != 4 {
		t.Fatalf("legacy fixture has %d with EXIT and %d without, want 40 and 4", withExit, without)
	}
}

func TestGoldenDesk(t *testing.T) {
	forceTrueColor(t)
	for _, fx := range deskFixtures {
		for _, w := range []int{60, 80, 120} {
			t.Run(fmt.Sprintf("%s/%d", fx.name, w), func(t *testing.T) {
				snap, opts := fx.build()
				assertGolden(t, fmt.Sprintf("desk_%s_%d", fx.name, w), RenderDeskFrame(snap, w, fx.height, deskNow, opts))
			})
		}
	}
}

func TestGoldenDeskNoColor(t *testing.T) {
	for _, fx := range deskFixtures {
		for _, w := range []int{60, 80, 120} {
			t.Run(fmt.Sprintf("%s/%d", fx.name, w), func(t *testing.T) {
				snap, opts := fx.build()
				var buf bytes.Buffer
				wr := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
				if _, err := wr.Write([]byte(RenderDeskFrame(snap, w, fx.height, deskNow, opts))); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(buf.String(), "\x1b") {
					t.Fatal("the no-color render still carries escapes")
				}
				assertGolden(t, fmt.Sprintf("desk_%s_%d_nocolor", fx.name, w), buf.String())
			})
		}
	}
}

// TestDeskFrame_FitsTheWindow pins the layout contract: exactly height lines,
// none wider than the window, at every fixture and width.
func TestDeskFrame_FitsTheWindow(t *testing.T) {
	for _, fx := range deskFixtures {
		for _, w := range []int{40, 60, 71, 72, 80, 120, 200} {
			for _, h := range []int{12, 24, fx.height} {
				snap, opts := fx.build()
				lines := strings.Split(RenderDeskFrame(snap, w, h, deskNow, opts), "\n")
				if len(lines) != h {
					t.Errorf("%s %dx%d: %d lines", fx.name, w, h, len(lines))
				}
				for i, l := range lines {
					if got := ansi.StringWidth(l); got > w {
						t.Errorf("%s %dx%d: line %d is %d cells: %q", fx.name, w, h, i, got, ansi.Strip(l))
					}
				}
			}
		}
	}
}

func TestDeskFrame_TilesCollapseBelow72(t *testing.T) {
	snap, opts := busySnapshot()
	wide := ansi.Strip(RenderDeskFrame(snap, 72, 40, deskNow, opts))
	narrow := ansi.Strip(RenderDeskFrame(snap, 71, 40, deskNow, opts))
	if !strings.Contains(wide, "╭ waiting") || !strings.Contains(wide, "╭ outcomes") {
		t.Errorf("72 columns should draw the three tiles:\n%s", wide)
	}
	if strings.Contains(narrow, "╭ waiting") {
		t.Errorf("71 columns should collapse the tiles:\n%s", narrow)
	}
	if !strings.Contains(narrow, "3 waiting") {
		t.Errorf("the summary line lost the waiting count:\n%s", narrow)
	}
}

// TestDeskFrame_Legacy shows every legacy run: 40 with an exit code and 4 as
// "no exit recorded", none hidden.
func TestDeskFrame_Legacy(t *testing.T) {
	snap, opts := legacySnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 120, 60, deskNow, opts))
	if n := strings.Count(out, "no exit recorded"); n != 4 {
		t.Errorf("%d rows say no exit recorded, want 4", n)
	}
	if n := strings.Count(out, "legacy-task-"); n != 44 {
		t.Errorf("%d history rows, want 44", n)
	}
	if strings.Contains(out, "older") {
		t.Error("a 60-line window has room for all 44; none should be folded")
	}
}

func TestDeskFrame_Queue(t *testing.T) {
	snap, opts := busySnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 120, 40, deskNow, opts))
	for _, want := range []string{
		"● 2 running  ◌ 3 waiting  ⌨ 1 tty", // the status strip
		"▸ ◌ waiting  17 merge-feature-branch",
		"needs you", "stale", "4/7 steps", "changed", "exit 1",
		"unchanged since queued 12m ago",
		"what  Merge the feature branch once checks pass",
		"┆ set -euo pipefail",
		"┆ echo merged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "# WHAT") {
		t.Error("the focus panel shows comment lines; it shows the first lines that run")
	}
}

func TestDeskFrame_RunningBars(t *testing.T) {
	snap, opts := busySnapshot()
	f := deskFrame{snap: snap, now: deskNow, opts: opts}
	st := testStyles()
	// 14-sync-mirror has a 2m history: 50s against it is a partial solid bar.
	bar, _ := f.rowBar(st, queueRow{rowRunning, snap.Running[0]}, 16)
	if got := plain(bar); got != "███████░░░░░░░░░" {
		t.Errorf("sync bar = %q, want 50s of a 2m median", got)
	}
	// With no history the bar is the shimmer, a pure function of elapsed time.
	snap.Done = nil
	bar, _ = f.rowBar(st, queueRow{rowRunning, snap.Running[0]}, 16)
	if got, want := plain(bar), plain(BarShimmer(st, 16, 50)); got != want {
		t.Errorf("no-history bar = %q, want the shimmer %q", got, want)
	}
	// The batch is segmented by its step states.
	bar, detail := f.rowBar(st, queueRow{rowRunning, snap.Running[1]}, 7)
	if got := plain(bar); got != "███▓█░░" {
		t.Errorf("batch bar = %q", got)
	}
	if plain(detail) != "4/7 steps" {
		t.Errorf("batch detail = %q", plain(detail))
	}
}

func TestDeskFrame_FocusShowsBatchStepsInWaveOrder(t *testing.T) {
	snap, opts := busySnapshot()
	rows := deskRows(snap, deskNow)
	cursor := -1
	for i, r := range rows {
		if r.item.Name == "16-nightly-batch" {
			cursor = i
		}
	}
	out := ansi.Strip(deskFrame{snap: snap, width: 120, height: 60, now: deskNow, opts: opts, cursor: cursor}.render())
	for _, want := range []string{
		"order fetch -> lint,test,docs -> build -> pack -> publish",
		"┆ 1 ✓ fetch",
		"┆ 2 ✓ lint",
		"┆ 3 ● build",
		"┆ 5 ◌ publish",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("focus lacks %q:\n%s", want, out)
		}
	}
	// With no height to spare the list stops at three steps and says so.
	short := ansi.Strip(deskFrame{snap: snap, width: 120, now: deskNow, opts: opts, cursor: cursor}.render())
	if !strings.Contains(short, "… 4 more steps · v to view") || strings.Contains(short, "┆ 5 ◌ publish") {
		t.Errorf("short focus should list three steps and count the rest:\n%s", short)
	}
}

// The focus panel shows the selected item's short hash, the one `desk add`
// prints and the agent reports, so the operator can match the two. A hash
// that is not 64 hex characters is never drawn.
func TestDeskFrame_FocusShowsTheHashPrefix(t *testing.T) {
	snap, opts := busySnapshot()
	sha := snap.Pending[0].Meta.SHA256
	out := ansi.Strip(RenderDeskFrame(snap, 120, 40, deskNow, opts))
	if !strings.Contains(out, snap.Pending[0].Name[3:]+" · sha256 "+sha[:12]+" · unchanged since queued") {
		t.Errorf("the focus head lacks the hash prefix %s:\n%s", sha[:12], out)
	}
	snap.Pending[0].Meta.SHA256 = "\x1b]0;PWNED\a" + sha[:50]
	out = RenderDeskFrame(snap, 120, 40, deskNow, opts)
	if strings.Contains(out, "PWNED") || strings.Contains(ansi.Strip(out), "sha256 ") {
		t.Errorf("a malformed hash was drawn:\n%s", out)
	}
}

func TestDeskFrame_EmptySaysNothingWaiting(t *testing.T) {
	snap, opts := emptySnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 80, 30, deskNow, opts))
	if n := strings.Count(out, "nothing waiting"); n < 2 {
		t.Errorf("an empty desk should say nothing waiting in the queue and focus panels:\n%s", out)
	}
}

func TestTildePath(t *testing.T) {
	for _, tc := range []struct{ dir, home, want string }{
		{"/home/op/x/desk", "/home/op", "~/x/desk"},
		{"/home/op", "/home/op", "~"},
		{"/home/opx/desk", "/home/op", "/home/opx/desk"},
		{"/var/desk", "", "/var/desk"},
		{"/var/desk", "/", "/var/desk"},
	} {
		if got := TildePath(tc.dir, tc.home); got != tc.want {
			t.Errorf("TildePath(%q, %q) = %q, want %q", tc.dir, tc.home, got, tc.want)
		}
	}
}

// hostileSnapshot puts termsafetest.Hostile in every field the frame draws
// that a script or the desk directory supplies.
func hostileSnapshot() (*desk.Snapshot, DeskFrameOptions) {
	h := termsafetest.Hostile
	snap, opts := busySnapshot()
	script := []byte("# WHAT: " + h("what") + "\n# WHY: " + h("why") + "\n" + h("echo line") + "\n" + h("more") + "\n")
	manifest := []byte("# WHAT: " + h("what") + "\nfetch -- " + h("cmd") + "\nlint after=fetch -- " + h("cmd2") + "\n")
	for _, list := range [][]desk.Item{snap.Pending, snap.Running, snap.Done, snap.Skipped} {
		for i := range list {
			list[i].Name = h(list[i].Name)
			list[i].Stem = h(list[i].Stem)
			list[i].What, list[i].Why = h("what"), h("why")
			list[i].Refusal = h("refusal")
			if list[i].Content != nil {
				list[i].Content = script
			}
		}
	}
	refused := item("20-refused", desk.KindScript, desk.StateRefused)
	refused.Name, refused.Refusal = h("20-refused"), h("a symlink")
	snap.Pending = append(snap.Pending, refused)
	batch := item(h("21-batch"), desk.KindBatch, desk.StateWaiting)
	batch.Content, batch.What = manifest, h("what")
	snap.Pending = append(snap.Pending, batch)
	opts.Host, opts.Version, opts.Dir = h("host"), h("1.0"), h("~/desk")
	opts.Records = map[string][]byte{snap.Running[0].Name: script, snap.Running[1].Name: manifest}
	opts.Steps = map[string][]desk.StepStatus{snap.Running[1].Name: {{ID: "fetch", State: desk.StepOK}}}
	return snap, opts
}

// TestDeskFrame_DrawsNothingUnsafe renders the hostile snapshot with the
// cursor on every row in turn (so each row's focus panel is drawn), at every
// width, and asserts the whole frame inert.
func TestDeskFrame_DrawsNothingUnsafe(t *testing.T) {
	snap, opts := hostileSnapshot()
	rows := deskRows(snap, deskNow)
	if len(rows) < 8 {
		t.Fatalf("hostile fixture has %d rows; it should list every kind", len(rows))
	}
	for _, w := range []int{60, 80, 120} {
		for c := range rows {
			out := deskFrame{snap: snap, width: w, height: 60, now: deskNow, opts: opts, cursor: c}.render()
			termsafetest.AssertInert(t, fmt.Sprintf("desk frame %d cols, cursor %d", w, c), out)
		}
	}

	// Negative control: the hostile text reaches the frame (escaped), so the
	// assertion above was looking at it.
	out := deskFrame{snap: snap, width: 200, height: 60, now: deskNow, opts: opts}.render()
	if !strings.Contains(out, `what\x1b[31m`) {
		t.Fatalf("the hostile WHAT is not in the frame; the inertness check proves nothing:\n%s", ansi.Strip(out))
	}
}
