package docs

// Tests for the per-note fragment-parse budget (#630 item 1): fragmentBudget,
// matchFragment's use of it, and its per-render and per-document scoping.

import (
	"strings"
	"testing"
)

// budgetFragment is a maxRenderedFragment-byte fragment that reaches "## x"
// only through its rendered text: "[x](y)" flattens to "x" and the trailing
// %% comment renders to nothing. No slug or as-written match can catch it.
func budgetFragment() string {
	const head, tail = "[x](y)%%", "%%"
	return head + strings.Repeat("c", maxRenderedFragment-len(head)-len(tail)) + tail
}

func budgetDoc() *Doc {
	return &Doc{Headings: []Heading{{Text: "x", Slug: "x"}, {Text: "other", Slug: "other"}}}
}

// TestFragmentBudget_Boundary: a budget of exactly two fragments serves the
// first two rendered matches and refuses the third. Mutations: `>` to `>=`
// in take turns the second call red; dropping the `remaining -= n` charge
// or the `!budget.take` check in matchFragment turns the third red.
func TestFragmentBudget_Boundary(t *testing.T) {
	frag := budgetFragment()
	if len(frag) != maxRenderedFragment {
		t.Fatalf("fixture is %d bytes, want %d", len(frag), maxRenderedFragment)
	}
	doc := budgetDoc()
	b := &fragmentBudget{remaining: 2 * maxRenderedFragment}
	for i := 1; i <= 2; i++ {
		if anchor, ok := matchFragment(RootVault, doc, frag, b); !ok || anchor != "x" {
			t.Fatalf("fragment %d within budget: (%q, %v), want (x, true)", i, anchor, ok)
		}
	}
	if anchor, ok := matchFragment(RootVault, doc, frag, b); ok || anchor != "" {
		t.Errorf("fragment 3 past budget: (%q, %v), want a miss", anchor, ok)
	}
}

// TestFragmentBudget_PastBudgetKeepsExactMatches: an exhausted budget only
// removes the parse pass. Slug and as-written matches still hit, a
// rendered-only fragment misses (never another heading), and a refusal
// charges nothing, so a short fragment still fits after a long one is
// refused. Mutations: gating the exact pass on the budget turns the slug
// and as-written cases red; charging before the size check in take turns the
// short-fragment case red.
func TestFragmentBudget_PastBudgetKeepsExactMatches(t *testing.T) {
	doc := budgetDoc()
	b := &fragmentBudget{remaining: 10}
	for _, f := range []string{"x", "X", "Other"} {
		if _, ok := matchFragment(RootVault, doc, f, b); !ok {
			t.Errorf("exact fragment %q missed with a spent budget", f)
		}
	}
	if anchor, ok := matchFragment(RootVault, doc, budgetFragment(), b); ok {
		t.Errorf("over-budget rendered fragment resolved to %q, want a miss", anchor)
	}
	if b.remaining != 10 {
		t.Errorf("a refused fragment charged the budget: remaining %d, want 10", b.remaining)
	}
	if anchor, ok := matchFragment(RootVault, doc, "*x*", b); !ok || anchor != "x" {
		t.Errorf("short fragment after a refusal: (%q, %v), want (x, true)", anchor, ok)
	}
}

// TestFragmentBudget_NilIsUnlimited: ResolveLink's nil budget keeps today's
// behaviour however many fragments it parses. Mutation: making take return
// false for a nil receiver turns this red.
func TestFragmentBudget_NilIsUnlimited(t *testing.T) {
	doc := budgetDoc()
	frag := budgetFragment()
	for i := 0; i < 2*maxFragmentParseBytes/maxRenderedFragment; i++ {
		if _, ok := matchFragment(RootVault, doc, frag, nil); !ok {
			t.Fatalf("nil budget refused fragment %d", i)
		}
	}
}

// TestFragmentBudget_UnmarkedFragmentsAreFree: a fragment with no markup
// byte is never parsed, so it is never charged. Mutation: charging before
// the fragmentMayRender check (or charging in the exact pass) drains the
// budget and turns this red.
func TestFragmentBudget_UnmarkedFragmentsAreFree(t *testing.T) {
	doc := budgetDoc()
	b := newFragmentBudget()
	for i := 0; i < 1000; i++ {
		matchFragment(RootVault, doc, "no markup here at all", b)
	}
	if b.remaining != maxFragmentParseBytes {
		t.Errorf("unparsed fragments charged the budget: remaining %d, want %d", b.remaining, maxFragmentParseBytes)
	}
}

func budgetRenderNote(links int) string {
	var sb strings.Builder
	sb.WriteString("# Note\n\n## x\n\n")
	for i := 0; i < links; i++ {
		sb.WriteString("[[Note#" + budgetFragment() + "]] ")
	}
	return sb.String()
}

func countJumps(html string) int { return strings.Count(html, `.md#x"`) }

// TestRenderDocFor_FragmentBudgetPerRender: a normal note is unaffected, a
// hostile one jumps for its first maxFragmentParseBytes/512 links and links
// the rest to the note without a fragment, and every render starts with a
// full budget. Mutations: a shared package-level budget turns the second
// render red; a budget per link (built inside the closure) turns the hostile
// case red; a bigger constant turns the exact count red.
func TestRenderDocFor_FragmentBudgetPerRender(t *testing.T) {
	const budgetLinks = maxFragmentParseBytes / maxRenderedFragment
	for _, tc := range []struct {
		name        string
		links, want int
	}{
		{"normal note", 20, 20},
		{"exactly at budget", budgetLinks, budgetLinks},
		{"hostile note", budgetLinks + 50, budgetLinks},
	} {
		note := budgetRenderNote(tc.links)
		idx, _ := newMatchVault(t, note)
		from, ok := idx.Find(idx.Roots()[0].Label, "Note.md")
		if !ok {
			t.Fatal("Note.md not indexed")
		}
		for pass := 1; pass <= 2; pass++ {
			r, err := RenderDocFor(RootVault, []byte(note), idx, &from)
			if err != nil {
				t.Fatal(err)
			}
			if got := countJumps(r.HTML); got != tc.want {
				t.Errorf("%s, render %d: %d links jumped to #x, want %d", tc.name, pass, got, tc.want)
			}
			// A link past the budget is a marked miss that still links to
			// the note, with no fragment: never a jump to another heading.
			if got := strings.Count(r.HTML, `class="wikilink wikilink-miss"`); got != tc.links-tc.want {
				t.Errorf("%s, render %d: %d broken links, want %d", tc.name, pass, got, tc.links-tc.want)
			}
			if got := strings.Count(r.HTML, `href="/doc/`+idx.Roots()[0].Label+`/Note.md"`); got != tc.links-tc.want {
				t.Errorf("%s, render %d: %d fragment-less links to the note, want %d", tc.name, pass, got, tc.links-tc.want)
			}
		}
	}
}
