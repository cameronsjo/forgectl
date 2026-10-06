package worker

import (
	"errors"
	"strings"
	"testing"
)

const testMarker = "0a1b2c3d4e5f"

func TestNewMarkerShape(t *testing.T) {
	a, err := NewMarker()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewMarker()
	if err != nil {
		t.Fatal(err)
	}
	if !validMarker(a) || !validMarker(b) {
		t.Fatalf("markers %q, %q do not have the marker shape", a, b)
	}
	if a == b {
		t.Fatalf("two markers are equal: %q", a)
	}
}

func TestCheckBrief(t *testing.T) {
	long := strings.Repeat("x", MaxTypedBrief)
	cases := []struct {
		name string
		text string
		via  string
		ok   bool
	}{
		{"plain launch", "Fix the login bug.", ViaLaunch, true},
		{"launch keeps newlines and tabs", "Step 1\n\tStep 2", ViaLaunch, true},
		{"launch may start with a dash", "- item one", ViaLaunch, true},
		{"plain typed", "Now run the tests.", ViaTyped, true},
		{"empty", "  ", ViaTyped, false},
		{"typed newline is Enter", "one\ntwo", ViaTyped, false},
		{"typed carriage return", "one\rtwo", ViaTyped, false},
		{"typed tab", "one\ttwo", ViaTyped, false},
		{"escape sequence", "a\x1b[31mred", ViaLaunch, false},
		{"bidi override", "a\u202eb", ViaLaunch, false},
		{"zero width space", "a\u200bb", ViaTyped, false},
		{"invalid utf8", "a\xffb", ViaLaunch, false},
		{"typed leading dash", "-h", ViaTyped, false},
		{"typed too long with instruction", long, ViaTyped, false},
		{"launch too long", strings.Repeat("x", MaxLaunchBrief+1), ViaLaunch, false},
		{"unknown via", "x", "carrier-pigeon", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckBrief(c.text, c.via)
			if c.ok && err != nil {
				t.Fatalf("CheckBrief(%q, %s) = %v, want nil", c.text, c.via, err)
			}
			if !c.ok && !errors.Is(err, ErrInvalidBrief) {
				t.Fatalf("CheckBrief(%q, %s) = %v, want ErrInvalidBrief", c.text, c.via, err)
			}
		})
	}
}

func TestComposeTypedStaysOneLine(t *testing.T) {
	got := Compose("Run the tests.", testMarker, ViaTyped)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("typed brief %q holds a line break", got)
	}
	if err := CheckBrief(got, ViaTyped); err != nil {
		t.Fatalf("the composed typed brief fails its own check: %v", err)
	}
}

// The brief's own instruction, echoed on the worker's screen, must never
// read as the worker's report.
func TestFindReportIgnoresTheEcho(t *testing.T) {
	for _, via := range []string{ViaLaunch, ViaTyped} {
		brief := Compose("Fix the login bug.", testMarker, via)
		screen := "❯ " + strings.ReplaceAll(brief, "\n", "\n  ") + "\n\n⏺ Working on it.\n"
		if r, ok := FindReport(screen, testMarker); ok {
			t.Fatalf("%s: echo alone yielded report %q", via, r)
		}
	}
}

func TestFindReport(t *testing.T) {
	echo := "❯ " + Compose("Fix it.", testMarker, ViaTyped)
	cases := []struct {
		name   string
		screen string
		want   string
		ok     bool
	}{
		{
			name:   "claude bullet",
			screen: echo + "\n\n⏺ REPORT " + testMarker + ": fixed on feat/x, commit abc\n\n✻ Brewed for 2s\n",
			want:   "fixed on feat/x, commit abc", ok: true,
		},
		{
			name:   "indented last line of a longer message",
			screen: echo + "\n\n⏺ Done.\n\n  REPORT " + testMarker + ": opened PR #12\n",
			want:   "opened PR #12", ok: true,
		},
		{
			name:   "codex bullet and bold",
			screen: echo + "\n• **REPORT " + testMarker + ":** tests pass\n",
			want:   "tests pass", ok: true,
		},
		{
			name:   "wrapped onto indented lines",
			screen: echo + "\n\n⏺ REPORT " + testMarker + ": fixed the bug and\n  pushed feat/x\n\n─────\n❯ \n",
			want:   "fixed the bug and pushed feat/x", ok: true,
		},
		{
			name:   "last report wins",
			screen: echo + "\n⏺ REPORT " + testMarker + ": first\n⏺ REPORT " + testMarker + ": second\n",
			want:   "second", ok: true,
		},
		{
			name:   "another brief's marker",
			screen: echo + "\n⏺ REPORT ffffffffffff: not ours\n",
			ok:     false,
		},
		{
			name:   "a report before the echo is an earlier turn",
			screen: "⏺ REPORT " + testMarker + ": stale\n" + echo + "\n⏺ thinking\n",
			ok:     false,
		},
		{
			name:   "user echo line is not a report",
			screen: "❯ REPORT " + testMarker + ": typed by the user\n",
			ok:     false,
		},
		{
			name:   "no report yet",
			screen: echo + "\n⏺ Working.\n",
			ok:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FindReport(c.screen, testMarker)
			if ok != c.ok || got != c.want {
				t.Fatalf("FindReport = %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestFindReportRefusesABadMarker(t *testing.T) {
	if _, ok := FindReport("⏺ REPORT .*: x\n", ".*"); ok {
		t.Fatal("a marker that is a regular expression matched")
	}
}

func TestFindReportCapsLength(t *testing.T) {
	screen := "⏺ REPORT " + testMarker + ": " + strings.Repeat("y", 3*maxReportRunes) + "\n"
	got, ok := FindReport(screen, testMarker)
	if !ok || len([]rune(got)) != maxReportRunes {
		t.Fatalf("report length %d, ok %v; want %d", len([]rune(got)), ok, maxReportRunes)
	}
}
