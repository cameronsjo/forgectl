package docs

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

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
// scan of 32k identical headings against the same work over 32k DISTINCT
// headings, which never collide and so cost the same under either id
// collection. goldmark's default id collection makes the duplicate case
// quadratic (minutes); headingIDs keeps it within a small factor of the
// baseline. The ratio, not a wall-clock number, is the assertion, so a slow
// or loaded CI runner scales both sides together; the fixed floor absorbs
// timer noise on a fast one.
//
// Mutation: in newParseContext, return parser.NewContext() (goldmark's
// default ids) — the duplicate render then misses the budget and this goes
// red on the deadline rather than after the minutes the full run would take.
func TestRender_DuplicateHeadings_LinearTime(t *testing.T) {
	const n = 32000
	var distinct strings.Builder
	for i := range n {
		_, _ = fmt.Fprintf(&distinct, "## a%d\n", i)
	}
	dup := []byte(strings.Repeat("## a\n", n))

	work := func(src []byte) {
		if _, err := render(src, RootDocs); err != nil {
			t.Errorf("render: %v", err)
		}
		if _, err := scanBodyFor(RootVault, src); err != nil {
			t.Errorf("scan: %v", err)
		}
	}

	start := time.Now()
	work([]byte(distinct.String()))
	baseline := time.Since(start)

	budget := 10*baseline + 2*time.Second
	done := make(chan time.Duration, 1)
	go func() {
		s := time.Now()
		work(dup)
		done <- time.Since(s)
	}()
	select {
	case took := <-done:
		if took > budget {
			t.Fatalf("%d duplicate headings took %v, over the %v budget (baseline %v)", n, took, budget, baseline)
		}
		t.Logf("%d duplicate headings: %v (baseline %v)", n, took, baseline)
	case <-time.After(budget):
		t.Fatalf("%d duplicate headings did not finish within %v (baseline %v)", n, budget, baseline)
	}
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
