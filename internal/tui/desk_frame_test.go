package tui

import (
	"bytes"
	"fmt"
	"slices"
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

// longHeaderScript carries a WHAT and WHY far longer than any window, and a
// body that opens with an embedded program's docstring, the shape that made
// the focus panel unreadable (forgectl#1082).
const longHeaderScript = `#!/bin/bash
# WHAT: Rebuild the package mirror index from the upstream release feed and prune every release older than the retention window
# WHY: Upstream published a release this morning and the nightly mirror job is paused until the index matches it
python3 - <<'PY'
print("rebuild")
PY
`

func longHeaderSnapshot() (*desk.Snapshot, DeskFrameOptions) {
	it := item("21-rebuild-mirror", desk.KindScript, desk.StateWaiting)
	it.Content = []byte(longHeaderScript)
	it.Headers = desk.ParseHeaders(it.Content)
	it.Meta = desk.Meta{AddedAt: agoPtr(4 * time.Minute), SHA256: desk.SHA256Hex([]byte("mirror"))}
	snap := &desk.Snapshot{Dir: "/home/op/.local/state/forgectl/desk", Taken: deskNow, Pending: []desk.Item{it}}
	return snap, deskOpts()
}

// TestGoldenDeskFocusWrap pins the focus panel at 80, 100 and 160 columns:
// WHAT and WHY wrap under their labels, in full, and the script preview is
// labelled.
func TestGoldenDeskFocusWrap(t *testing.T) {
	forceTrueColor(t)
	for _, w := range []int{80, 100, 160} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			snap, opts := longHeaderSnapshot()
			got := RenderDeskFrame(snap, w, 30, deskNow, opts)
			plain := strings.Join(strings.Fields(ansi.Strip(got)), " ")
			for _, want := range []string{"retention window", "matches it", "script ·"} {
				if want == "retention window" || want == "matches it" {
					// The tail of each field: wrapping must not lose it.
					if !strings.Contains(strings.ReplaceAll(plain, "│ │", ""), want) {
						t.Errorf("%d columns: %q was cut from the focus panel", w, want)
					}
					continue
				}
				if !strings.Contains(plain, want) {
					t.Errorf("%d columns: the script preview has no label (%q)", w, want)
				}
			}
			assertGolden(t, fmt.Sprintf("desk_focuswrap_%d", w), got)
		})
	}
}

const (
	longWhat = "Rebuild the mirror index for every release channel, prune releases older than the retention window, and re-sign the manifest with the rotated key so downstream clients accept it again."
	longWhy  = "The nightly mirror job has been failing since the key rotation, and clients now reject the stale manifest; this restores the mirror before the morning sync window opens and the retention prune keeps the disk from filling again."
)

// focusText is the plain text of the focus panel's body, panel borders
// stripped, one string per line.
func focusText(out string) []string {
	var lines []string
	in := false
	for _, l := range strings.Split(ansi.Strip(out), "\n") {
		switch {
		case strings.HasPrefix(l, "╭ focus"):
			in = true
		case in && strings.HasPrefix(l, "╰"):
			return lines
		case in:
			lines = append(lines, strings.TrimSpace(strings.Trim(l, "│ ")))
		}
	}
	return lines
}

// squash joins wrapped lines back into the words they hold.
func squash(lines []string) string {
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// A long what and why wrap in full, however long, continuation lines sit
// under the text, and the script preview gives up its rows for them.
func TestDeskFrame_LongFieldsWrapInFull(t *testing.T) {
	snap, opts := longHeaderSnapshot()
	it := &snap.Pending[0]
	what := strings.TrimSpace(strings.Repeat(longWhat+" ", 2))
	it.Headers = desk.Headers{What: what, Why: longWhy}
	it.Content = []byte("#!/bin/bash\n" + strings.Repeat("echo hi\n", 12))
	raw := RenderDeskFrame(snap, 100, 30, deskNow, opts)
	out := ansi.Strip(raw)
	body := squash(focusText(raw))
	for _, want := range []string{what, longWhy} {
		if !strings.Contains(body, want) {
			t.Errorf("a field was cut:\n%s", out)
		}
	}
	if strings.Contains(strings.SplitN(out, "why ", 2)[0], "…") {
		t.Errorf("what ends in an ellipsis though the panel has room:\n%s", out)
	}
	short, _ := longHeaderSnapshot()
	short.Pending[0].Headers = desk.Headers{What: "Rebuild", Why: "Restore"}
	short.Pending[0].Content = it.Content
	before := strings.Count(ansi.Strip(RenderDeskFrame(short, 100, 30, deskNow, opts)), "┆ echo hi")
	if after := strings.Count(out, "┆ echo hi"); after >= before {
		t.Errorf("the preview kept %d rows, %d with short fields:\n%s", after, before, out)
	}
}

// With fewer than deskScriptMin rows left, the preview is one line.
func TestDeskFrame_PreviewCollapsesToOneLine(t *testing.T) {
	snap, opts := longHeaderSnapshot()
	it := &snap.Pending[0]
	it.Headers = desk.Headers{What: strings.Repeat(longWhat+" ", 2), Why: longWhy}
	it.Content = []byte("#!/bin/bash\n" + strings.Repeat("echo hi\n", 12))
	raw := RenderDeskFrame(snap, 100, 24, deskNow, opts)
	got := strings.Join(focusText(raw), "\n")
	if !strings.Contains(got, "script: 12 lines · v to view") || strings.Contains(got, "┆ echo hi") {
		t.Errorf("want the one-line script summary:\n%s", ansi.Strip(raw))
	}
	if !strings.Contains(squash(focusText(raw)), longWhy) {
		t.Errorf("why was cut while the preview collapsed:\n%s", ansi.Strip(raw))
	}
}

// On a window too short for both fields, they still win over the preview and
// are cut only when they alone exceed the panel, ending in an ellipsis; the
// header line and its sha256 stay.
func TestDeskFrame_ShortWindowCutsFieldsLast(t *testing.T) {
	snap, opts := longHeaderSnapshot()
	it := &snap.Pending[0]
	it.Headers = desk.Headers{What: strings.Repeat(longWhat+" ", 6), Why: strings.Repeat(longWhy+" ", 6)}
	raw := RenderDeskFrame(snap, 100, 20, deskNow, opts)
	out := ansi.Strip(raw)
	lines := strings.Split(out, "\n")
	if len(lines) > 20 {
		t.Errorf("frame is %d lines in a 20-line window", len(lines))
	}
	if strings.Contains(out, "┆") || !strings.Contains(out, "…") {
		t.Errorf("want no preview and a cut ending in an ellipsis:\n%s", out)
	}
	if !strings.Contains(out, "sha256 "+it.Meta.SHA256[:deskShortHash]) || !strings.Contains(out, "what ") || !strings.Contains(out, "why ") {
		t.Errorf("the head, what or why is missing:\n%s", out)
	}
}

// A finished or failed item wraps what and why in full too, above its
// "l to view the log" line.
func TestDeskFrame_DoneItemFieldsWrapInFull(t *testing.T) {
	// Doubled, each wraps to 5 lines at 100 columns, past the old 4-line cap.
	what := longWhat + " " + longWhat
	why := longWhy + " " + longWhy
	for _, code := range []int{0, 1} {
		it := item("15-merge-1169", desk.KindScript, desk.StateDone)
		it.Headers = desk.Headers{What: what, Why: why}
		it.Started, it.Ended, it.ExitCode = ago(10*time.Minute), ago(9*time.Minute), rcPtr(code)
		it.Meta = desk.Meta{AddedAt: agoPtr(time.Hour), SHA256: desk.SHA256Hex([]byte("x"))}
		snap := &desk.Snapshot{Dir: "/d", Taken: deskNow, Done: []desk.Item{it}}
		raw := RenderDeskFrame(snap, 100, 30, deskNow, deskOpts())
		got := squash(focusText(raw))
		if !strings.Contains(got, what) || !strings.Contains(got, why) || !strings.Contains(got, "l to view the log") {
			t.Errorf("exit %d: done item cut what/why or lost the log hint:\n%s", code, ansi.Strip(raw))
		}
	}
}

// The focus panel must read without colour: WHAT and WHY are told apart by
// their labels and the hanging indent, and the script preview by its label.
func TestGoldenDeskFocusWrapNoColor(t *testing.T) {
	for _, w := range []int{80, 100, 160} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			snap, opts := longHeaderSnapshot()
			var buf bytes.Buffer
			wr := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
			if _, err := wr.Write([]byte(RenderDeskFrame(snap, w, 30, deskNow, opts))); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(buf.String(), "\x1b") {
				t.Fatal("the no-color render still carries escapes")
			}
			assertGolden(t, fmt.Sprintf("desk_focuswrap_%d_nocolor", w), buf.String())
		})
	}
}

// twoWaitingSnapshot is the #1098 reproduction: two waiting scripts, each
// with a hash, what and why.
func twoWaitingSnapshot() (*desk.Snapshot, DeskFrameOptions) {
	var pending []desk.Item
	for i, name := range []string{"01-s1", "02-s2"} {
		it := item(name, desk.KindScript, desk.StateWaiting)
		it.Content = []byte("#!/bin/bash\n# WHAT: Print step " + name + "\n# WHY: The approval check\necho " + name + "\n")
		it.Headers = desk.ParseHeaders(it.Content)
		it.Meta = desk.Meta{AddedAt: agoPtr(time.Duration(i+1) * time.Minute), SHA256: desk.SHA256Hex(it.Content)}
		pending = append(pending, it)
	}
	return &desk.Snapshot{Dir: "/home/op/.local/state/forgectl/desk", Taken: deskNow, Pending: pending}, deskOpts()
}

// focusOnScreen reports whether a frame shows the selected item's short hash,
// what and why: the three things the operator approves from.
func focusOnScreen(frame string, it desk.Item) bool {
	plain := ansi.Strip(frame)
	return strings.Contains(plain, "sha256 "+it.Meta.SHA256[:deskShortHash]) &&
		strings.Contains(plain, "what  "+it.What) && strings.Contains(plain, "why   "+it.Why)
}

// TestDeskFrame_FocusKeepsItsRowsInShortWindows is #1098: a short window gives
// up the tiles, the summary and queue rows before the focus panel's hash,
// what and why. Before the fix the focus panel was the first thing cut.
func TestDeskFrame_FocusKeepsItsRowsInShortWindows(t *testing.T) {
	for _, size := range [][2]int{{100, 14}, {100, 12}, {80, 10}, {40, 10}} {
		snap, opts := twoWaitingSnapshot()
		f := deskFrame{snap: snap, width: size[0], height: size[1], now: deskNow, opts: opts}
		out := f.render()
		if !focusOnScreen(out, snap.Pending[0]) {
			t.Errorf("%dx%d: the focus panel lost its hash, what or why:\n%s", size[0], size[1], ansi.Strip(out))
		}
		if !f.focusShown() {
			t.Errorf("%dx%d: focusShown is false for a frame that shows the focus panel", size[0], size[1])
		}
		if n := len(strings.Split(out, "\n")); n != size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], n)
		}
	}
}

// Below the smallest layout, or narrower than the frame, the desk says it is
// too small (T13) and focusShown is false, which is what makes y refuse.
func TestDeskFrame_TooSmallSaysSoAndIsNotShown(t *testing.T) {
	for _, size := range [][2]int{{80, 9}, {100, 7}, {30, 8}, {39, 30}} {
		snap, opts := twoWaitingSnapshot()
		f := deskFrame{snap: snap, width: size[0], height: size[1], now: deskNow, opts: opts}
		out := ansi.Strip(f.render())
		if f.focusShown() {
			t.Errorf("%dx%d: focusShown is true below the minimum:\n%s", size[0], size[1], out)
		}
		if !strings.Contains(out, "too small") {
			t.Errorf("%dx%d: the frame does not say it is too small:\n%s", size[0], size[1], out)
		}
		if n := len(strings.Split(out, "\n")); n != size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], n)
		}
	}
}

// A long name is cut before the hash is: the head keeps the short sha256 at
// the narrowest frame.
func TestDeskFrame_LongNameNeverPushesTheHashOff(t *testing.T) {
	snap, opts := twoWaitingSnapshot()
	snap.Pending[0].Name = "01-" + strings.Repeat("a-very-long-item-name-", 4)
	f := deskFrame{snap: snap, width: 40, height: 20, now: deskNow, opts: opts}
	if !strings.Contains(ansi.Strip(f.render()), "sha256 "+snap.Pending[0].Meta.SHA256[:deskShortHash]) || !f.focusShown() {
		t.Errorf("a long name pushed the hash out of the focus head:\n%s", ansi.Strip(f.render()))
	}
}

// focusShown follows the selected row, and a row with no valid hash is never
// shown as approvable.
func TestDeskFrame_FocusShownNeedsAValidHash(t *testing.T) {
	snap, opts := twoWaitingSnapshot()
	snap.Pending[0].Meta.SHA256 = "not-a-hash"
	f := deskFrame{snap: snap, width: 100, height: 30, now: deskNow, opts: opts}
	if f.focusShown() {
		t.Error("a row with no valid hash reads as shown")
	}
	f.cursor = 1
	if !f.focusShown() {
		t.Error("the second row, with a valid hash, reads as not shown")
	}
}

// TestGoldenDeskShort pins the short-window frames from #1098.
func TestGoldenDeskShort(t *testing.T) {
	forceTrueColor(t)
	for _, size := range [][2]int{{100, 14}, {100, 12}, {80, 10}, {30, 8}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			snap, opts := twoWaitingSnapshot()
			got := RenderDeskFrame(snap, size[0], size[1], deskNow, opts)
			assertGolden(t, fmt.Sprintf("desk_short_%dx%d", size[0], size[1]), got)
			var buf bytes.Buffer
			wr := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
			if _, err := wr.Write([]byte(got)); err != nil {
				t.Fatal(err)
			}
			assertGolden(t, fmt.Sprintf("desk_short_%dx%d_nocolor", size[0], size[1]), buf.String())
		})
	}
}

// The too-small notice names the rows at which the focus panel fits at the
// current width.
func TestDeskFrame_TooSmallNamesASizeThatFits(t *testing.T) {
	snap, opts := twoWaitingSnapshot()
	snap.Pending[0].What = strings.Repeat("rebuild the index ", 6)
	snap.Pending[0].Why = strings.Repeat("the cache was cleared ", 4)
	out := ansi.Strip(RenderDeskFrame(snap, 120, 8, deskNow, opts))
	w := 120
	var h int
	if i := strings.Index(out, "make the window "); i < 0 {
		t.Fatalf("no size in the notice:\n%s", out)
	} else if _, err := fmt.Sscanf(out[i+len("make the window "):], "%d rows", &h); err != nil {
		t.Fatalf("parse size: %v\n%s", err, out)
	}
	f := deskFrame{snap: snap, width: w, height: h, now: deskNow, opts: opts}
	if !f.focusShown() {
		t.Errorf("the advertised %dx%d still does not show the focus panel:\n%s", w, h, ansi.Strip(f.render()))
	}
}

// No frame draws the history panel as a lone top border.
func TestDeskFrame_NoLoneHistoryBorder(t *testing.T) {
	for _, fx := range []func() (*desk.Snapshot, DeskFrameOptions){twoWaitingSnapshot, busySnapshot} {
		for h := 8; h <= 30; h++ {
			snap, opts := fx()
			lines := strings.Split(ansi.Strip(RenderDeskFrame(snap, 80, h, deskNow, opts)), "\n")
			for i, l := range lines {
				if strings.HasPrefix(l, "╭ history") && (i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "│")) {
					t.Errorf("80x%d: history has a border and no body:\n%s", h, strings.Join(lines, "\n"))
				}
			}
		}
	}
}

// The too-small frame does not offer y, which refuses there.
func TestDeskFrame_TooSmallHidesY(t *testing.T) {
	snap, opts := twoWaitingSnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 100, 7, deskNow, opts))
	if strings.Contains(out, "y run") || !strings.Contains(out, "a all") {
		t.Errorf("too-small hints:\n%s", out)
	}
}

// A lost item's panel says what happened and what to do, in plain words; a
// changed one shows the hash it was queued at and the hash it has now
// (#1106).
func TestDeskFrame_LostAndChangedSayWhatToDo(t *testing.T) {
	lost := item("01-long", desk.KindScript, desk.StateLost)
	lost.Headers = desk.Headers{What: "Sleep", Why: "A test"}
	lost.Meta.SHA256 = desk.SHA256Hex([]byte("long"))
	queued, now := desk.SHA256Hex([]byte("v1")), desk.SHA256Hex([]byte("v2"))
	changed := item("02-s2", desk.KindScript, desk.StateSkipped)
	changed.Headers = desk.Headers{What: "Print", Why: "A test"}
	changed.Meta = desk.Meta{AddedAt: agoPtr(time.Minute), SHA256: queued, ChangedSHA256: now, SkipReason: desk.SkipChanged}
	snap := &desk.Snapshot{Dir: "/d", Taken: deskNow, Running: []desk.Item{lost}, Skipped: []desk.Item{changed}}
	_, opts := emptySnapshot()

	flat := func(cursor int) string {
		out := ansi.Strip(deskFrame{snap: snap, width: 80, height: 30, now: deskNow, opts: opts, cursor: cursor}.render())
		return strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " ")
	}
	got := flat(0)
	for _, want := range []string{"01 long · sha256 " + lost.Meta.SHA256[:12] + " · lost", "the desk stopped watching it mid-run, so it may have partly run", "s clears it", "l shows its output"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost panel lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "RUN-END") || strings.Contains(got, "owner") {
		t.Errorf("lost panel still uses internal words:\n%s", got)
	}
	got = flat(1)
	for _, want := range []string{"02 s2 · changed", "queued " + queued[:12] + ", now " + now[:12], "not run, moved to skipped", "ask Claude to queue it again"} {
		if !strings.Contains(got, want) {
			t.Errorf("changed panel lacks %q:\n%s", want, got)
		}
	}
	// A forged hash read back from meta is never drawn.
	snap.Skipped[0].Meta.ChangedSHA256 = "\x1b]0;x\a" + now[:40]
	if got := flat(1); strings.Contains(got, ", now") || strings.Contains(got, "\x1b") {
		t.Errorf("an invalid changed hash was drawn:\n%s", got)
	}
}

// The lost or changed note is not part of the focus panel's minimum: a
// small window that shows a waiting row's panel shows a lost row's too,
// without the note.
func TestDeskFrame_NoteDoesNotForceTooSmall(t *testing.T) {
	lost := item("01-long", desk.KindScript, desk.StateLost)
	lost.Headers = desk.Headers{What: "Sleep", Why: "A test"}
	lost.Meta.SHA256 = desk.SHA256Hex([]byte("long"))
	snap := &desk.Snapshot{Dir: "/d", Taken: deskNow, Running: []desk.Item{lost}}
	_, opts := emptySnapshot()
	out := ansi.Strip(deskFrame{snap: snap, width: 40, height: 10, now: deskNow, opts: opts}.render())
	if strings.Contains(out, "too small") || !strings.Contains(out, "01 long") {
		t.Errorf("a lost row forced the too-small frame at 40x10:\n%s", out)
	}
}

// A changed hash equal to the queued one (a forged or old meta) is not shown
// as "now": the note says only that it changed.
func TestDeskFrame_ChangedHashEqualToQueuedIsNotShown(t *testing.T) {
	h := desk.SHA256Hex([]byte("v1"))
	changed := item("02-s2", desk.KindScript, desk.StateSkipped)
	changed.Meta = desk.Meta{AddedAt: agoPtr(time.Minute), SHA256: h, ChangedSHA256: h, SkipReason: desk.SkipChanged}
	snap := &desk.Snapshot{Dir: "/d", Taken: deskNow, Skipped: []desk.Item{changed}}
	_, opts := emptySnapshot()
	out := ansi.Strip(deskFrame{snap: snap, width: 100, height: 30, now: deskNow, opts: opts}.render())
	if strings.Contains(out, ", now ") || !strings.Contains(out, "queued "+h[:12]+", since changed") {
		t.Errorf("changed note:\n%s", out)
	}
}

// A lost run is not counted as running: the queue strip and the outcomes
// tile count it as lost (#1106).
func TestDeskFrame_LostIsNotCountedRunning(t *testing.T) {
	lost := item("01-long", desk.KindScript, desk.StateLost)
	snap := &desk.Snapshot{Dir: "/d", Taken: deskNow, Running: []desk.Item{lost}}
	_, opts := emptySnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 100, 30, deskNow, opts))
	if !strings.Contains(out, "● 0 running") || !strings.Contains(out, "? 1 lost") || !strings.Contains(out, "0 changed · 1 lost") {
		t.Errorf("lost counts:\n%s", out)
	}
}

// An empty desk says how the queue fills and offers only keys that can act
// (#1107).
func TestDeskFrame_EmptyStateSaysHowItFills(t *testing.T) {
	snap, opts := emptySnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 100, 30, deskNow, opts))
	if !strings.Contains(out, "Claude queues scripts with forgectl desk add; they appear here") {
		t.Errorf("the empty queue does not say how it fills:\n%s", out)
	}
	footer := out[strings.LastIndex(out, "\n")+1:]
	for _, k := range []string{"y run", "s skip", "v view", "a all", "j/k"} {
		if strings.Contains(footer, k) {
			t.Errorf("empty desk offers %q: %q", k, footer)
		}
	}
	if !strings.Contains(footer, "q quit") {
		t.Errorf("footer lost q quit: %q", footer)
	}
}

// Every indicator carries a word (#1107): the header dot, the history bars'
// legend, a running item with no earlier run to compare with; and the
// counts agree: "started today" beside "no finished runs yet".
func TestDeskFrame_IndicatorsAreLabelled(t *testing.T) {
	snap, opts := busySnapshot()
	out := ansi.Strip(RenderDeskFrame(snap, 120, 40, deskNow, opts))
	for _, want := range []string{"desk ● 3 waiting", "bar: run length, longest full", "started today"} {
		if !strings.Contains(out, want) {
			t.Errorf("busy frame lacks %q:\n%s", want, out)
		}
	}
	run := item("01-new", desk.KindScript, desk.StateRunning)
	run.Started = ago(12 * time.Second)
	snap = &desk.Snapshot{Dir: "/d", Taken: deskNow, Running: []desk.Item{run}}
	out = ansi.Strip(RenderDeskFrame(snap, 120, 30, deskNow, opts))
	for _, want := range []string{"desk ● 1 running", "0:12 so far", "no finished runs yet", "1 started today"[2:]} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no runs yet") || strings.Contains(out, "runs today") {
		t.Errorf("frame still has the old wording:\n%s", out)
	}
}

// --no-icons maps every glyph to one ASCII character of the same width.
func TestDeskFrame_ASCIIKeepsTheLayout(t *testing.T) {
	for _, fx := range deskFixtures {
		snap, opts := fx.build()
		assertASCIIFrame(t, fx.name, snap, opts)
	}
	snap, opts := twoWaitingSnapshot()
	assertASCIIFrame(t, "short", snap, opts)
}

func assertASCIIFrame(t *testing.T, name string, snap *desk.Snapshot, opts DeskFrameOptions) {
	t.Helper()
	icons := ansi.Strip(RenderDeskFrame(snap, 120, 40, deskNow, opts))
	opts.ASCII = true
	ascii := ansi.Strip(RenderDeskFrame(snap, 120, 40, deskNow, opts))
	for _, r := range ascii {
		if r >= 0x80 && !strings.ContainsRune("·…±", r) {
			t.Fatalf("%s: ASCII frame draws %q:\n%s", name, r, ascii)
		}
	}
	il, al := strings.Split(icons, "\n"), strings.Split(ascii, "\n")
	for i := range il {
		if ansi.StringWidth(il[i]) != ansi.StringWidth(al[i]) {
			t.Errorf("line %d: width %d vs %d", i, ansi.StringWidth(il[i]), ansi.StringWidth(al[i]))
		}
	}
}

// The footer follows the selected row: y and s only where they act, a
// whenever something waits, and nothing item-shaped on an empty queue even
// in a too-small window (#1107).
func TestDeskFrame_HintsFollowTheSelection(t *testing.T) {
	snap, opts := busySnapshot()
	rows := deskRows(snap, deskNow)
	footer := func(cursor, w, h int) string {
		out := ansi.Strip(deskFrame{snap: snap, width: w, height: h, now: deskNow, opts: opts, cursor: cursor}.render())
		return out[strings.LastIndex(out, "\n")+1:]
	}
	running := slices.IndexFunc(rows, func(r queueRow) bool { return r.kind == rowRunning })
	if f := footer(0, 120, 40); !strings.Contains(f, "y run") || !strings.Contains(f, "s skip") {
		t.Errorf("waiting row: %q", f)
	}
	if f := footer(running, 120, 40); strings.Contains(f, "y run") || strings.Contains(f, "s skip") || !strings.Contains(f, "a all") {
		t.Errorf("running row: %q", f)
	}
	empty, eopts := emptySnapshot()
	out := ansi.Strip(deskFrame{snap: empty, width: 100, height: 6, now: deskNow, opts: eopts}.render())
	if f := out[strings.LastIndex(out, "\n")+1:]; strings.Contains(f, "s skip") || strings.Contains(f, "j/k") || !strings.Contains(f, "q quit") {
		t.Errorf("empty, too small: %q", f)
	}
}

// The footer drops the least important hints first and always keeps q quit
// and ? help (#1108: at 40 columns it used to cut "q quit" off first).
func TestDeskFrame_FooterKeepsQuitAndHelp(t *testing.T) {
	snap, opts := busySnapshot()
	for w := 30; w <= 140; w += 5 {
		out := ansi.Strip(RenderDeskFrame(snap, w, 40, deskNow, opts))
		footer := out[strings.LastIndex(out, "\n")+1:]
		if !strings.Contains(footer, "q quit") || !strings.Contains(footer, "? help") {
			t.Errorf("width %d: footer %q lost q quit or ? help", w, footer)
		}
		if ansi.StringWidth(footer) > max(w, deskMinWidth) || strings.Contains(footer, "…") {
			t.Errorf("width %d: footer %q is cut", w, footer)
		}
	}
	// j/k goes first, then r: the move hint is the least needed.
	out := ansi.Strip(RenderDeskFrame(snap, 75, 40, deskNow, opts))
	footer := out[strings.LastIndex(out, "\n")+1:]
	if strings.Contains(footer, "j/k") || !strings.Contains(footer, "r runs") {
		t.Errorf("75 columns: %q", footer)
	}
}

// The ? screen lists every key the footer can show, from the same table.
func TestDeskKeyLinesListEveryBinding(t *testing.T) {
	lines := strings.Join(deskKeyLines(), "\n")
	for _, b := range deskBindings {
		if !strings.Contains(lines, b.key) || !strings.Contains(lines, b.help) {
			t.Errorf("? screen lacks %q", b.key)
		}
	}
}

// The header names what the desk is doing when nothing waits: running, or
// idle (#1107 review: "nothing waiting" beside a running item misled).
func TestDeskFrame_HeaderSaysRunningOrIdle(t *testing.T) {
	run := item("01-a", desk.KindScript, desk.StateRunning)
	run.Started = ago(time.Second)
	_, opts := emptySnapshot()
	out := ansi.Strip(RenderDeskFrame(&desk.Snapshot{Dir: "/d", Taken: deskNow, Running: []desk.Item{run}}, 100, 30, deskNow, opts))
	if !strings.HasPrefix(out, "desk ● 1 running") {
		t.Errorf("header with one running item: %q", strings.SplitN(out, "\n", 2)[0])
	}
	empty, eopts := emptySnapshot()
	out = ansi.Strip(RenderDeskFrame(empty, 100, 30, deskNow, eopts))
	if !strings.HasPrefix(out, "desk ○ idle") {
		t.Errorf("empty header: %q", strings.SplitN(out, "\n", 2)[0])
	}
}

// u, l and r show only when they can act (#1108 review): no skip to undo, no
// log and no run on an empty desk; and l outranks v when the window is narrow.
func TestDeskFrame_HintsHideKeysWithNothingToActOn(t *testing.T) {
	empty, opts := emptySnapshot()
	out := ansi.Strip(RenderDeskFrame(empty, 120, 30, deskNow, opts))
	footer := out[strings.LastIndex(out, "\n")+1:]
	for _, k := range []string{"u undo", "l log", "r runs"} {
		if strings.Contains(footer, k) {
			t.Errorf("empty desk offers %q: %q", k, footer)
		}
	}
	f := deskFrame{snap: empty, width: 120, height: 30, now: deskNow, opts: opts, canUndo: true}
	if out := ansi.Strip(f.render()); !strings.Contains(out[strings.LastIndex(out, "\n")+1:], "u undo") {
		t.Error("u undo is not offered with a skip to undo")
	}
	snap, bopts := busySnapshot()
	rows := deskRows(snap, deskNow)
	done := slices.IndexFunc(rows, func(r queueRow) bool { return r.kind == rowDone || r.kind == rowFailed })
	for w := 40; w <= 70; w += 5 {
		out := ansi.Strip(deskFrame{snap: snap, width: w, height: 40, now: deskNow, opts: bopts, cursor: done}.render())
		footer := out[strings.LastIndex(out, "\n")+1:]
		if strings.Contains(footer, "v view") && !strings.Contains(footer, "l log") {
			t.Errorf("width %d: v view kept over l log: %q", w, footer)
		}
	}
}
