package docs

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/perftest"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
)

// headingIDValues is the vocabulary the equivalence test draws from: small
// enough that random sequences collide constantly, and seeded with the
// shapes that stress the suffix cursor — literal suffixed ids that collide
// with generated ones, values that slug to the same base, empty slugs (the
// "heading"/"id" fallback and its own suffixes), and non-ASCII text goldmark
// drops from the slug.
var headingIDValues = []string{
	"a", "A", "a-1", "a-2", "a 1", "a_1", "  a  ", "a-1-1", "a--1",
	"b", "b-1", "", "   ", "-", "é", "日本", "héading", "heading", "heading-1",
	"heading-2", "id", "id-1", "a-10", "a-3", "Straße", "x y z", "x-y-z",
}

// TestHeadingIDs_MatchGoldmarkDefault drives goldmark's default collection
// and headingIDs through the same random sequence of Generate and Put calls
// and requires every returned id to match byte for byte.
//
// Mutation: advance the cursor to `i + 2` instead of `i + 1` in Generate —
// it then skips a free suffix, and the first sequence that generates one
// base three times goes red.
func TestHeadingIDs_MatchGoldmarkDefault(t *testing.T) {
	rng := rand.New(rand.NewSource(596)) //nolint:gosec // G404: deterministic test fixture, not crypto
	kinds := []ast.NodeKind{ast.KindHeading, ast.KindParagraph}
	for doc := range 10000 {
		want := parser.NewContext().IDs()
		got := newHeadingIDs()
		ops := 1 + rng.Intn(60)
		var trace []string
		for range ops {
			v := headingIDValues[rng.Intn(len(headingIDValues))]
			if rng.Intn(8) == 0 {
				// Put of an explicit id, as goldmark does for a heading that
				// already carries one: it joins the used set without a slug.
				trace = append(trace, "Put("+v+")")
				want.Put([]byte(v))
				got.Put([]byte(v))
				continue
			}
			k := kinds[rng.Intn(len(kinds))]
			w := string(want.Generate([]byte(v), k))
			g := string(got.Generate([]byte(v), k))
			trace = append(trace, fmt.Sprintf("Generate(%q,%v)=%q", v, k, w))
			if w != g {
				t.Fatalf("doc %d: id mismatch: goldmark %q, headingIDs %q\ntrace:\n%s",
					doc, w, g, strings.Join(trace, "\n"))
			}
		}
	}
}

// TestHeadingIDs_LiteralSuffixCollision pins the two ways a literal heading
// meets a generated suffix, with goldmark's default output as the want:
//
//   - the literal last, "a", "a", "a-1": the literal's own base "a-1" is
//     already taken by the generated suffix, so it becomes "a-1-1";
//   - the literal first, "a", "a-1", "a": the generated suffix must step
//     OVER the literal's "a-1" to "a-2", and the next copy continues at "a-3".
//
// Mutation: in Generate's probe loop, replace the used check with `if true`
// (hand out the cursor's suffix unconditionally) — the second case then
// hands out "a-1" twice and this goes red.
func TestHeadingIDs_LiteralSuffixCollision(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"a", "a", "a-1", "a"}, []string{"a", "a-1", "a-1-1", "a-2"}},
		{[]string{"a", "a-1", "a", "a-1", "a"}, []string{"a", "a-1", "a-2", "a-1-1", "a-3"}},
	}
	for _, c := range cases {
		def, got := parser.NewContext().IDs(), newHeadingIDs()
		var gotIDs, defIDs []string
		for _, v := range c.in {
			gotIDs = append(gotIDs, string(got.Generate([]byte(v), ast.KindHeading)))
			defIDs = append(defIDs, string(def.Generate([]byte(v), ast.KindHeading)))
		}
		if strings.Join(defIDs, ",") != strings.Join(c.want, ",") {
			t.Fatalf("goldmark default gives %v for %v; this test's want %v is stale", defIDs, c.in, c.want)
		}
		if strings.Join(gotIDs, ",") != strings.Join(c.want, ",") {
			t.Errorf("headingIDs gives %v for %v, want %v", gotIDs, c.in, c.want)
		}
	}
}

// TestRender_DuplicateHeadings_LinearTime bounds the render and the link
// scan of 8k identical headings against the same work over an eighth as
// many. goldmark's default id collection probes every suffix already taken,
// so the duplicate case is quadratic (minutes at 32k); headingIDs keeps it
// linear. The check is a ratio (perftest.Linear, #919) in process CPU time;
// it was a wall-clock budget for 32k over a baseline of distinct headings,
// which host load broke.
//
// Mutation: in newParseContext, return parser.NewContext() (goldmark's
// default ids) — the duplicate render then goes red on the ratio, or on
// perftest.Ceiling if the one large run is slower than that.
func TestRender_DuplicateHeadings_LinearTime(t *testing.T) {
	const n, k = 8000, 8
	work := func(n int) func() {
		src := []byte(strings.Repeat("## a\n", n))
		return func() {
			if _, err := render(src, RootDocs); err != nil {
				t.Fatalf("render: %v", err)
			}
			if _, err := scanBodyFor(RootVault, src); err != nil {
				t.Fatalf("scan: %v", err)
			}
		}
	}
	perftest.Linear(t, fmt.Sprintf("%d duplicate headings", n), k, work(n/k), work(n))
}

// TestRender_DuplicateHeadings_IDsUnchanged pins, through the real render
// and scan pipelines of both root kinds, the ids goldmark's default gives a
// document mixing duplicates, a literal suffix, and an empty-slug heading.
//
// Mutation: in Generate, start the probe at i := s.next[base] + 1 — the
// first duplicate then gets "a-2" instead of "a-1" and this goes red.
func TestRender_DuplicateHeadings_IDsUnchanged(t *testing.T) {
	src := []byte("## a\n\n## a\n\n## a-1\n\n## a\n\n## ¿?\n\n## ¿?\n")
	wantIDs := []string{"a", "a-1", "a-1-1", "a-2", "heading", "heading-1"}
	for _, kind := range []RootKind{RootDocs, RootVault} {
		out, err := render(src, kind)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		for _, id := range wantIDs {
			if !strings.Contains(out, `id="`+id+`"`) {
				t.Errorf("kind %v: render lacks id %q:\n%s", kind, id, out)
			}
		}
		scan, err := scanBodyFor(kind, src)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		var got []string
		for _, h := range scan.headings {
			got = append(got, h.Slug)
		}
		if strings.Join(got, ",") != strings.Join(wantIDs, ",") {
			t.Errorf("kind %v: scan slugs = %v, want %v", kind, got, wantIDs)
		}
	}
}
