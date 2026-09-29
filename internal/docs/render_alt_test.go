package docs

import (
	"strings"
	"testing"
)

// Ordinary punctuation in an alt must survive, '#' included: the pattern only
// refuses control characters, so none of these may drop the alt.
func TestRender_ImgAltPunctuation(t *testing.T) {
	for _, tc := range []struct{ md, want string }{
		{"![#1 chart](i.png)", `alt="#1 chart"`},
		{"![Figure 1: chart](i.png)", `alt="Figure 1: chart"`},
		{"![Why? 50% more](i.png)", `alt="Why? 50% more"`},
		{"![Q &amp; A](i.png)", `alt="Q &amp; A"`},
		{`![Say "hi"](i.png)`, `alt="Say &#34;hi&#34;"`},
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

// These hostile alts pin bluemonday's output ESCAPING, not the pattern (which
// admits quotes and angle brackets): the payload must come out as attribute
// text, never as an attribute, tag or entity of its own.
func TestRender_ImgAltHostileIsEscaped(t *testing.T) {
	for _, alt := range []string{`x" onerror="alert(1)`, `x<script>alert(1)</script>`, `x&#x22; onerror=y`, `x' onerror='y`} {
		got, err := Render([]byte("![" + alt + "](i.png)\n"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "<script") || strings.Contains(got, `" onerror`) || strings.Contains(got, "' onerror") {
			t.Errorf("hostile alt %q escaped its attribute: %q", alt, got)
		}
		if strings.Count(got, "<img") != 1 || strings.Count(got, `alt="`) != 1 {
			t.Errorf("hostile alt %q changed the element shape: %q", alt, got)
		}
	}
}
