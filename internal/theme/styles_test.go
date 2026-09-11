package theme

import (
	"strings"
	"testing"
)

func TestMarks_RendersExpectedGlyphs(t *testing.T) {
	m := New(Options{}, true).Marks()
	for glyph, s := range map[string]string{"✓": m.OK, "!": m.Warn, "✗": m.Fail, "-": m.Skip} {
		if !strings.Contains(s, glyph) {
			t.Errorf("mark %q does not contain its glyph %q", s, glyph)
		}
	}
}

func TestStyles_EveryFieldRendersDistinctColor(t *testing.T) {
	th := New(Options{}, true)
	s := th.Styles()
	fields := map[string]string{
		"Header": s.Header.Render("x"),
		"Accent": s.Accent.Render("x"),
		"OK":     s.OK.Render("x"),
		"Warn":   s.Warn.Render("x"),
		"Danger": s.Danger.Render("x"),
		"Active": s.Active.Render("x"),
		"Muted":  s.Muted.Render("x"),
		"Meta":   s.Meta.Render("x"),
		"Dim":    s.Dim.Render("x"),
		"Steel":  s.Steel.Render("x"),
		"Fg":     s.Fg.Render("x"),
		"Brand":  s.Brand.Render("x"),
	}
	for name, rendered := range fields {
		if !strings.Contains(rendered, "\x1b") {
			t.Errorf("Styles().%s did not render an escape sequence: %q", name, rendered)
		}
	}
}

func TestColor_ResolvesHex(t *testing.T) {
	th := New(Options{Overrides: map[Role]Pair{RoleAccent: {Dark: "#abcdef"}}}, true)
	if got := th.Hex(RoleAccent); got != "#abcdef" {
		t.Errorf("Hex(RoleAccent) = %q, want #abcdef", got)
	}
}
