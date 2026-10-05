package tui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func testStyles() theme.Styles { return theme.Default().Styles() }

// plain drops the styling so a test can assert on layout alone.
func plain(s string) string { return ansi.Strip(s) }

func TestPanel_EveryRowIsExactlyWidthCells(t *testing.T) {
	long := strings.Repeat("a long name ", 20)
	cases := []struct {
		name, title, strip string
		lines              []string
	}{
		{"plain", "queue", "", []string{"one", "two"}},
		{"strip", "queue", "● 1 running  ◌ 2 waiting", []string{"one"}},
		{"long content", "queue", "", []string{long}},
		{"cjk content", "queue", "", []string{"日本語のとても長い名前日本語のとても長い名前日本語のとても長い名前"}},
		{"cjk title", "待機中のキュー", "", []string{"x"}},
		{"long title and strip", strings.Repeat("t", 80), strings.Repeat("s", 80), []string{"x"}},
		{"empty", "", "", nil},
	}
	for _, c := range cases {
		for _, w := range []int{4, 5, 10, 24, 60, 120} {
			t.Run(fmt.Sprintf("%s/%d", c.name, w), func(t *testing.T) {
				out := Panel(testStyles(), w, c.title, c.strip, c.lines)
				rows := strings.Split(plain(out), "\n")
				if want := len(c.lines) + 2; len(rows) != want {
					t.Fatalf("%d rows, want %d (content lines + 2 borders)", len(rows), want)
				}
				for i, r := range rows {
					if got := ansi.StringWidth(r); got != w {
						t.Errorf("row %d is %d cells, want %d: %q", i, got, w, r)
					}
				}
			})
		}
	}
}

func TestPanel_Shape(t *testing.T) {
	got := plain(Panel(testStyles(), 40, "queue", "● 1 running", []string{"hello"}))
	want := strings.Join([]string{
		"╭ queue ─┤ ● 1 running ├───────────────╮",
		"│ hello                                │",
		"╰──────────────────────────────────────╯",
	}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestPanel_CutsWithEllipsisAndNeverWraps(t *testing.T) {
	got := plain(Panel(testStyles(), 12, "t", "", []string{"0123456789abcdef"}))
	rows := strings.Split(got, "\n")
	if len(rows) != 3 {
		t.Fatalf("wrapped: %d rows", len(rows))
	}
	if rows[1] != "│ 0123456… │" {
		t.Errorf("content row = %q", rows[1])
	}
	// A wide rune that would straddle the edge is dropped whole, not split.
	wide := plain(Panel(testStyles(), 10, "t", "", []string{"日本語日本語"}))
	if row := strings.Split(wide, "\n")[1]; row != "│ 日本語… │" && row != "│ 日本…  │" {
		t.Errorf("wide row = %q", row)
	}
}

func TestPanel_TinyWidthIsClamped(t *testing.T) {
	for _, w := range []int{-3, 0, 1, 3} {
		rows := strings.Split(plain(Panel(testStyles(), w, "title", "strip", []string{"content"})), "\n")
		for _, r := range rows {
			if ansi.StringWidth(r) != minPanelWidth {
				t.Errorf("width %d: row %q is not %d cells", w, r, minPanelWidth)
			}
		}
	}
}

func TestSparkline(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		width  int
		want   string
	}{
		{"scaled to max", []float64{0, 1, 2, 4, 8}, 5, "▁▂▃▅█"},
		{"all zero", []float64{0, 0, 0}, 3, "▁▁▁"},
		{"tiny positive stays above baseline", []float64{0, 0.001, 100}, 3, "▁▂█"},
		{"negative is baseline", []float64{-5, 2}, 2, "▁█"},
		{"keeps the most recent", []float64{9, 9, 1, 2, 3}, 3, "▄▆█"},
		{"short series right-aligned", []float64{4, 8}, 5, "   ▅█"},
		{"no data", nil, 4, "    "},
		{"zero width", []float64{1}, 0, ""},
		{"negative width", []float64{1}, -2, ""},
		{"exact ramp", []float64{1, 2, 3, 4, 5, 6, 7, 8}, 8, "▂▃▄▅▅▆▇█"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sparkline(c.values, c.width)
			if got != c.want {
				t.Errorf("Sparkline = %q, want %q", got, c.want)
			}
			if w := ansi.StringWidth(got); c.width > 0 && w != c.width {
				t.Errorf("width = %d, want %d", w, c.width)
			}
		})
	}
}

func TestBarSolid(t *testing.T) {
	cases := []struct {
		frac  float64
		width int
		want  string
	}{
		{0, 8, "░░░░░░░░"},
		{1, 8, "████████"},
		{0.5, 8, "████░░░░"},
		{0.3, 10, "███░░░░░░░"},
		{-1, 4, "░░░░"},
		{7, 4, "████"},
		{0.5, 0, ""},
	}
	for _, c := range cases {
		if got := plain(BarSolid(testStyles(), c.width, c.frac)); got != c.want {
			t.Errorf("BarSolid(%v,%d) = %q, want %q", c.frac, c.width, got, c.want)
		}
	}
}

func TestBarSegments(t *testing.T) {
	d, r, w, f := SegDone, SegRunning, SegWaiting, SegFailed
	cases := []struct {
		name  string
		segs  []Seg
		width int
		want  string
	}{
		{"seven steps in sixteen", []Seg{d, d, d, d, r, w, w}, 14, plainSeven()},
		{"one cell each", []Seg{d, f, r, w}, 4, "█✗▓░"},
		{"two cells each", []Seg{d, r, w}, 6, "██▓▓░░"},
		{"more segments than cells", []Seg{d, d, d, d, r, w}, 3, "██▓"},
		{"none", nil, 4, "░░░░"},
		{"zero width", []Seg{d}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := plain(BarSegments(testStyles(), c.width, c.segs))
			if got != c.want {
				t.Errorf("= %q, want %q", got, c.want)
			}
		})
	}
}

// plainSeven is the expected 14-cell render of {d,d,d,d,r,w,w}: cell i maps to
// segment i*7/14, i.e. two cells per segment.
func plainSeven() string { return "████████▓▓░░░░" }

func TestBarShimmer(t *testing.T) {
	// Cell 0 shows the highlight's last cell on entry, so derive expectations
	// from the glyph order rather than guessing: ▒▓▒ enters head-first.
	want := map[int]string{
		0:  "░░░░░░░░",
		1:  "▒░░░░░░░",
		2:  "▓▒░░░░░░",
		3:  "▒▓▒░░░░░",
		6:  "░░░▒▓▒░░",
		9:  "░░░░░░▒▓",
		10: "░░░░░░░▒",
		11: "░░░░░░░░",
		12: "▒░░░░░░░",
		-1: "░░░░░░░▒",
	}
	for frame, w := range want {
		if got := plain(BarShimmer(testStyles(), 8, frame)); got != w {
			t.Errorf("frame %d = %q, want %q", frame, got, w)
		}
	}
	if BarShimmer(testStyles(), 0, 3) != "" {
		t.Error("zero width should be empty")
	}
	if a, b := BarShimmer(testStyles(), 8, 4), BarShimmer(testStyles(), 8, 4); a != b {
		t.Error("same frame drew differently")
	}
}

// goldenPanel is a fixed fixture: a strip, a wide-rune row, an over-long row.
func goldenPanel(width int) string {
	st := testStyles()
	return Panel(st, width, "queue", "● 1 running  ◌ 2 waiting", []string{
		"▸ ◌ waiting  17 merge-when-green-1201  " + BarSolid(st, 8, 0.25),
		"  ● running  16 runner-pool-batch     " + BarSegments(st, 8, []Seg{SegDone, SegDone, SegRunning, SegWaiting}),
		"  ✓ done     15 日本語のとても長い名前のジョブ日本語のとても長い名前のジョブ",
		"  ● running  14 shimmer              " + BarShimmer(st, 8, 3) + "  " + Sparkline([]float64{1, 3, 2, 8, 5}, 8),
	})
}

func TestGoldenPanel(t *testing.T) {
	forceTrueColor(t)
	for _, w := range []int{60, 80, 120} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			assertGolden(t, fmt.Sprintf("panel_%d", w), goldenPanel(w))
		})
	}
}

func TestGoldenPanelNoColor(t *testing.T) {
	for _, w := range []int{60, 80, 120} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			var buf bytes.Buffer
			wr := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
			if _, err := wr.Write([]byte(goldenPanel(w))); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(buf.String(), "\x1b") {
				t.Fatal("the no-color render still carries escapes")
			}
			assertGolden(t, fmt.Sprintf("panel_%d_nocolor", w), buf.String())
		})
	}
}

// TestPanel_DrawsNothingUnsafe is the contract the helpers rely on: text a
// caller has put through termsafe.SafeLine renders without a control sequence.
// The helpers do not sanitize, so this is also what an unsanitized caller
// would fail.
func TestPanel_DrawsNothingUnsafe(t *testing.T) {
	st := testStyles()
	hostile := termsafetest.Hostile("work")
	safe := termsafe.SafeLine(hostile)
	out := Panel(st, 40, safe, safe, []string{safe, "ok " + safe})
	termsafetest.AssertInert(t, "panel with sanitized hostile text", out)

	// Negative control: the same text unsanitized must be caught, or the
	// assertion above could not have failed.
	raw := Panel(st, 40, hostile, hostile, []string{hostile})
	probe := &captureReporter{}
	termsafetest.AssertInert(probe, "raw", raw)
	if !probe.failed {
		t.Error("AssertInert accepted an unsanitized hostile panel; the check cannot fail")
	}
}

type captureReporter struct{ failed bool }

func (*captureReporter) Helper()        {}
func (c *captureReporter) Fatal(...any) { c.failed = true }
