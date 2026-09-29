package docs

import (
	"strings"
	"testing"
)

// Ordinary punctuation in a link or image title must survive: the title rule
// scoped to a and img refuses control characters only.
func TestRender_TitlePunctuation(t *testing.T) {
	for _, tc := range []struct{ md, want string }{
		{`![x](i.png "#t")`, `title="#t"`},
		{`[a](b "Note: see")`, `title="Note: see"`},
		{`[a](b "Why? 50% more")`, `title="Why? 50% more"`},
		{`![x](i.png "Q &amp; A")`, `title="Q &amp; A"`},
		{`![x](i.png "Figure 1: chart")`, `title="Figure 1: chart"`},
		{`[a](b 'Say "hi"')`, `title="Say &#34;hi&#34;"`},
		{`![x](i.png 'Say "hi"')`, `title="Say &#34;hi&#34;"`},
	} {
		got, err := Render([]byte(tc.md + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %s in %q", tc.md, tc.want, got)
		}
	}
}

// Hostile titles pin bluemonday's output ESCAPING, not the pattern (which
// admits quotes and angle brackets): the payload must come out as attribute
// text, never as an attribute or tag of its own.
func TestRender_TitleHostileIsEscaped(t *testing.T) {
	for _, tc := range []struct{ md, want string }{
		{`[a](b 'x" onmouseover="alert(1)')`, `title="x&#34; onmouseover=&#34;alert(1)"`},
		{`![x](i.png 'x" onmouseover="alert(1)')`, `title="x&#34; onmouseover=&#34;alert(1)"`},
		{`[a](b 'x<script>alert(1)</script>')`, `title="x&lt;script&gt;alert(1)&lt;/script&gt;"`},
		{`![x](i.png 'x<script>alert(1)</script>')`, `title="x&lt;script&gt;alert(1)&lt;/script&gt;"`},
	} {
		got, err := Render([]byte(tc.md + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %s in %q", tc.md, tc.want, got)
		}
		if strings.Contains(got, "<script") || strings.Contains(got, `" onmouseover`) {
			t.Errorf("%s: escaped its attribute: %q", tc.md, got)
		}
	}
}

// A control character (\x01, not \f: bluemonday's OR'd Paragraph rule admits
// \f) drops the title while the element itself survives.
func TestRender_TitleControlCharDropped(t *testing.T) {
	for _, md := range []string{"[a](b \"t\x01x\")", "![x](i.png \"t\x01x\")"} {
		got, err := Render([]byte(md + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "title=") {
			t.Errorf("%q: control-char title kept: %q", md, got)
		}
		if !strings.Contains(got, "<a ") && !strings.Contains(got, "<img ") {
			t.Errorf("%q: element lost: %q", md, got)
		}
	}
}
