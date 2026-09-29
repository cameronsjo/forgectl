package docs

import (
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// policySkipSet reads the live sanitizer's skip-content set. bluemonday keeps
// it unexported, so reflection is the only way to compare against the real
// policy rather than against a second hand-copied list.
func policySkipSet(t *testing.T) []string {
	t.Helper()
	v := reflect.ValueOf(sanitizer).Elem().FieldByName("setOfElementsToSkipContent")
	if !v.IsValid() {
		t.Fatal("bluemonday.Policy has no setOfElementsToSkipContent field; re-check the skip-content mirror against the new bluemonday")
	}
	var names []string
	for _, k := range v.MapKeys() {
		names = append(names, k.String())
	}
	sort.Strings(names)
	return names
}

// Mutation: delete "desc" from skipContentElements (or add a name to
// allowInlineSVG's SkipElementsContent call) and this goes red.
func TestSkipContentElements_MatchPolicy(t *testing.T) {
	want := policySkipSet(t)
	got := make([]string, 0, len(skipContentByName))
	for k := range skipContentByName {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skipContentElements drifted from the sanitizer policy:\n mirror: %v\n policy: %v", got, want)
	}
}

// The replay assumes no skip-content name is an allowed element and that the
// policy registers no element regexps (both change bluemonday's branch).
// Mutation: p.AllowElements("desc") in allowInlineSVG turns this red.
func TestSkipContentElements_ReplayAssumptions(t *testing.T) {
	pv := reflect.ValueOf(sanitizer).Elem()
	allowed := pv.FieldByName("elsAndAttrs")
	for name := range skipContentByName {
		if allowed.MapIndex(reflect.ValueOf(name)).IsValid() {
			t.Errorf("%q is both allowed and skip-content; unclosedSkipContent's replay no longer matches bluemonday", name)
		}
	}
	if n := pv.FieldByName("elsMatchingAndAttrs").Len(); n != 0 {
		t.Errorf("policy registers %d element regexps; unclosedSkipContent does not replay matchRegex", n)
	}
}

// Parity, per name: an unclosed opener followed by MARKER makes the sanitizer
// drop MARKER and makes the detector fire, naming that element.
// Mutation: make unclosedSkipContent return ("", false) — every name goes
// red; report tok.Data instead of the allowlist name — "foreignObject" and
// the other mixed-case names go red.
func TestUnclosedSkipContent_ParityPerName(t *testing.T) {
	for _, name := range skipContentElements {
		in := []byte("<p>before</p>\n<" + name + ">\n<p>MARKER</p>\n")
		out := string(sanitizer.SanitizeBytes(in))
		if strings.Contains(out, "MARKER") {
			t.Errorf("<%s>: sanitizer kept MARKER (%q); the set no longer skips it", name, out)
		}
		got, ok := unclosedSkipContent(in)
		if !ok || got != name {
			t.Errorf("<%s>: detector = (%q, %v), want (%q, true)", name, got, ok, name)
		}
	}
}

// Differential: over random token soups, the detector fires exactly when the
// sanitizer drops a trailing MARKER. This is the property the banner needs,
// and it catches any drift between the replay and bluemonday's own loop.
// Mutation: clamp the counter (`if count > 0 { count-- }`) — red on the
// stray-closer shapes; ignore raw text (`rawLost` never set) — red on an
// unclosed <style>; route script/style through the skip counter like the
// other names — red on a closed <style> followed by a stray closer.
func TestUnclosedSkipContent_MatchesSanitizer(t *testing.T) {
	alphabet := []string{
		"<object>", "</object>", "<title>", "</title>", "<style>", "</style>",
		"<script>", "</script>", "<image href=x>", "</image>", "<desc>", "</desc>",
		"<svg>", "</svg>", "<b>", "</b>", "<p>", "</p>", "<br/>", "x", " ", "\n",
		"<foreignObject>", "</FOREIGNOBJECT>", "<noscript>", "</noscript>",
	}
	r := rand.New(rand.NewSource(622))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j := r.Intn(8); j >= 0; j-- {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		b.WriteString("MARKER")
		in := []byte(b.String())
		dropped := !strings.Contains(string(sanitizer.SanitizeBytes(in)), "MARKER")
		if _, fired := unclosedSkipContent(in); fired != dropped {
			t.Fatalf("input %q: sanitizer dropped MARKER=%v, detector fired=%v", in, dropped, fired)
		}
	}
}

// Mutation: drop the count-- on an unmatched closer (or model it as a
// proper open/close stack) and the stray-closer case goes red.
func TestRender_UnclosedSkipContentBanner(t *testing.T) {
	cases := []struct {
		name, src, tag string
	}{
		{"svg image", "<image src=x>\n\nMARKER", "image"},
		{"title block", "<title>\n\nMARKER", "title"},
		{"style block", "<style>\n\nMARKER", "style"},
		{"inline desc", "a <desc> b\n\nMARKER", "desc"},
		{"stray closer then balanced pair", "</object> <object></object>\n\nMARKER", "object"},
		// Author casing never reaches the banner: the name is the allowlist's.
		{"uppercase foreignObject", "<FOREIGNOBJECT>\n\nMARKER", "foreignObject"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := Render([]byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "MARKER") {
				t.Fatalf("MARKER survived; the case no longer exercises a drop: %q", out)
			}
			if !strings.HasPrefix(out, skipContentBanner(c.tag)) {
				t.Fatalf("want the %q banner at the top, got %q", c.tag, out)
			}
		})
	}
}

// Mutation: fire without the skipLost/rawLost requirement — "trailing
// opener" goes red; drop the counter decrement — "closed svg title" goes red.
func TestRender_NoSkipContentBannerWhenNothingHidden(t *testing.T) {
	cases := map[string]string{
		"closed svg title":  "<svg><title>t</title><rect width=\"1\" height=\"1\"/></svg>\n\nMARKER",
		"closed style":      "<style>p{}</style>\n\nMARKER",
		"code span":         "`<title>` and `<image>`\n\nMARKER",
		"fenced code":       "```\n<style>\n```\n\nMARKER",
		"trailing opener":   "MARKER <image src=x>",
		"textarea is shown": "<textarea>\n\nMARKER",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := Render([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "MARKER") {
				t.Fatalf("MARKER dropped, so this is not a nothing-hidden case: %q", out)
			}
			if strings.Contains(out, "data-forgectl-notice") {
				t.Fatalf("banner fired with nothing hidden: %q", out)
			}
		})
	}
}

// The banner's data attribute cannot come from a document: the sanitizer
// strips data-* attributes. Mutation: p.AllowDataAttributes() in
// newSanitizer turns this red.
func TestRender_SkipContentBannerNotForgeable(t *testing.T) {
	out, err := Render([]byte(`<blockquote data-forgectl-notice="skip-content">fake</blockquote>`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "data-forgectl-notice") {
		t.Fatalf("document forged the banner attribute: %q", out)
	}
}
