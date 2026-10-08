package docs

// Frontmatter decode contract (#910, frontmatter_check.go):
//   [x] Happy: a YAML block the map decode accepts is frontmatter, decoded once
//   [x] Sad: a YAML block the map decode refuses is refused without it
//       (duplicate keys at any depth, a non-scalar key, a self-containing
//       anchor, a tag that does not fit, a top level that is not a mapping)
//   [x] Sad: a merge key is refused, since no consumer would apply it
//   [x] Happy: aliases come off the node: a list, a bare string, an alias
//   [x] Sad: a non-string alias value is dropped, as the map decode dropped it
//   [x] Happy: TOML within its cap is frontmatter and renders once, sorted
//   [x] Sad: a TOML block one byte over its cap is refused, whatever its shape
//   [x] Unhappy: each superlinear YAML shape costs about what plain keys cost
//   [x] Unhappy: each superlinear TOML shape past the TOML cap, the quote
//       desync payloads included, costs about what plain keys at the cap cost

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/perftest"
	"gopkg.in/yaml.v3"
)

// Every row the map decode refused is refused, and every row it accepted
// is accepted, except the merge rows, which it accepted and which are
// refused here on purpose. The map decode is asserted alongside, so a row
// that stops matching it is visible. Mutations: dropping the duplicate-key
// check, the key-kind check, the onPath check, the tagged-scalar decode,
// or the top-level-mapping check each turns its rows red; dropping the
// merge check turns the merge rows red.
func TestYAMLFrontmatterRoot_MatchesTheMapDecode(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        bool
		merge       bool
	}{
		{"empty", "", true, false},
		{"comment only", "# nothing\n", true, false},
		{"null", "null\n", true, false},
		{"plain", "a: 1\nb: [x, y]\nc: {d: e}\n", true, false},
		{"quoted merge-looking key", "'<<': 1\n", true, false},
		{"alias value", "a: &x v\nb: *x\n", true, false},
		{"alias key", "a: &k key\n*k : v\n", true, false},
		{"good explicit tag", "a: !!int 7\n", true, false},
		{"duplicate top key", "a: 1\nb: 2\na: 3\n", false, false},
		{"duplicate nested key", "a:\n  b: 1\n  b: 2\n", false, false},
		{"duplicate key in a list item", "a:\n  - {b: 1, b: 2}\n", false, false},
		{"sequence key", "? [a]\n: v\n", false, false},
		{"mapping key", "? {a: b}\n: v\n", false, false},
		{"self-containing anchor", "a: &x [1, *x]\n", false, false},
		{"bad explicit tag", "a: !!int seven\n", false, false},
		{"sequence at the top", "- a\n- b\n", false, false},
		{"string at the top", "hello\n", false, false},
		{"undecodable", "a: [\n", false, false},
		{"merge key", "b: &b {x: 1}\nc:\n  <<: *b\n  y: 2\n", false, true},
		{"merge list", "b: &b {x: 1}\nc: {<<: [*b, *b]}\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			mapOK := yaml.Unmarshal([]byte(tc.block), &m) == nil
			if mapOK != (tc.want || tc.merge) {
				t.Fatalf("the map decode says %v for %q; the row's premise is wrong", mapOK, tc.block)
			}
			if _, ok := yamlFrontmatterRoot([]byte(tc.block)); ok != tc.want {
				t.Errorf("yamlFrontmatterRoot(%q) ok = %v, want %v", tc.block, ok, tc.want)
			}
			if _, ok := splitFrontmatter([]byte("---\n" + tc.block + "---\n# T\n")); ok != tc.want {
				t.Errorf("splitFrontmatter ok = %v, want %v", ok, tc.want)
			}
		})
	}
}

// Aliases come off the node with the map decode's typing: strings only,
// a bare string folded to a list, an alias followed one step. Mutations:
// keeping non-string scalars turns the number rows red; not following an
// alias turns the alias rows red.
func TestAliasesFromNode(t *testing.T) {
	for _, tc := range []struct {
		block string
		want  []string
	}{
		{"aliases: [one, two]\n", []string{"one", "two"}},
		{"aliases: solo\n", []string{"solo"}},
		{"aliases: 123\n", nil},
		{"aliases: [one, 2, true, null, '3']\n", []string{"one", "3"}},
		{"n: &n named\naliases: [*n, other]\n", []string{"named", "other"}},
		{"l: &l [x, y]\naliases: *l\n", []string{"x", "y"}},
		{"aliases:\n", nil},
		{"other: [x]\n", nil},
	} {
		root, ok := yamlFrontmatterRoot([]byte(tc.block))
		if !ok || root == nil {
			t.Fatalf("yamlFrontmatterRoot refused %q", tc.block)
		}
		if got := aliasesFromNode(root); !slices.Equal(got, tc.want) {
			t.Errorf("aliasesFromNode(%q) = %q, want %q", tc.block, got, tc.want)
		}
	}
}

// tomlRows are TOML blocks of every shape the pre-scan of an earlier
// round tried to bound: long dotted keys and headers, quoted and spaced
// segments, dots in every string kind and in comments, deep inline tables,
// and the multi-line strings closed by extra quotes that desynchronized
// it. Each is under maxTOMLFrontmatterBytes, so each is frontmatter, and
// each is cheap to decode because the cap bounds the decode by size.
func tomlRows() []string {
	nine := strings.Repeat("a.", 9) + "b"
	nest := func(d int) string { return "x = " + strings.Repeat("{b=", d) + "1" + strings.Repeat("}", d) + "\n" }
	return []string{
		"[params.author]\nname = \"A\"\n",
		nine + " = 1\n",
		"[" + nine + "]\n",
		"[[" + nine + "]]\n",
		strings.ReplaceAll(nine, ".", " . ") + " = 1\n",
		strings.Repeat(`"a".`, 9) + "b = 1\n",
		"x = { " + nine + " = 1 }\n",
		`v = "` + nine + `"` + "\n",
		`v = '` + nine + `'` + "\n",
		`v = """` + "\n" + nine + "\n" + `"""` + "\n",
		`v = '''` + "\n" + nine + "\n" + `'''` + "\n",
		`v = "\"` + nine + `"` + "\n",
		"# " + nine + "\nk = 1\n",
		"f = [1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5]\n",
		`t = "{{{{{{{{{{{{"` + "\n",
		nest(9),
		`k = {s = """x"""", ` + nine + " = 1}\n",
		`k = {s = '''x''''', ` + nine + " = 1}\n",
		`s = """x"""""` + "\n" + nine + " = 1\n",
		`s = '''x''''` + "\n" + nine + " = 1\n",
	}
}

// The TOML cap is the 1 KiB the docs promise, and every tomlRows block,
// whatever its shape, is within it and so is frontmatter (the at-cap and
// over-cap rows are TestSplitFrontmatter_Cap's). Mutation: any other
// maxTOMLFrontmatterBytes turns the first check red.
func TestSplitFrontmatter_TOMLCap(t *testing.T) {
	if maxTOMLFrontmatterBytes != 1<<10 {
		t.Fatalf("maxTOMLFrontmatterBytes = %d, want %d (1 KiB, as docs/commands/docs.md says)", maxTOMLFrontmatterBytes, 1<<10)
	}
	for _, block := range tomlRows() {
		if _, ok := splitFrontmatter([]byte("+++\n" + block + "+++\n# T\n")); !ok {
			t.Errorf("a %d-byte +++ block within the cap was refused: %q", len(block), block)
		}
	}
}

// A TOML block renders as a properties block with its keys sorted, and
// one past the TOML cap renders as text. Mutation: frontmatterHTML
// returning "" for a +++ block turns the first half red; dropping the TOML
// cap turns the second half red.
func TestRender_TOMLFrontmatter(t *testing.T) {
	got, err := Render([]byte("+++\ntitle = \"Tee\"\nauthor = \"Ay\"\n+++\n\n# T\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	ia, it := strings.Index(got, ">author</span>"), strings.Index(got, ">title</span>")
	if !strings.Contains(got, "data-forgectl-props") || ia < 0 || it < 0 || ia > it {
		t.Errorf("TOML frontmatter is not a sorted properties block:\n%s", got)
	}
	var b strings.Builder
	for i := 0; b.Len() <= maxTOMLFrontmatterBytes; i++ {
		fmt.Fprintf(&b, "k%d = %d\n", i, i)
	}
	got, err = Render([]byte("+++\n" + b.String() + "+++\n\n# T\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "data-forgectl-props") {
		t.Errorf("a TOML block past its cap rendered as a properties block")
	}
}

// fmLines returns n bytes or a little less of lines from line(i).
func fmLines(n int, line func(i int) string) string {
	var b strings.Builder
	for i := 0; ; i++ {
		l := line(i)
		if b.Len()+len(l) > n {
			return b.String()
		}
		b.WriteString(l)
	}
}

// frontmatterSink keeps the timed frontmatter work from being optimized away.
var frontmatterSink int

// fmPtr is splitFrontmatter's result as wellFormedFrontmatter returns it.
func fmPtr(fm frontmatterBlock, ok bool) *frontmatterBlock {
	if !ok {
		return nil
	}
	return &fm
}

// frontmatterWork is everything a page and the index do with a document's
// frontmatter: the split, the properties block and the aliases.
func frontmatterWork(src []byte, reps int) func() {
	return func() {
		for range reps {
			fm, ok := splitFrontmatter(src)
			frontmatterSink += len(frontmatterHTML(fmPtr(fm, ok)))
			if root := frontmatterRoot(fm); root != nil {
				frontmatterSink += len(aliasesFromNode(root))
			}
		}
	}
}

// Each YAML shape that made the decode superlinear costs, at the cap,
// about what plain keys of the same size cost (perftest.Within, process
// CPU time). Measured with the fix: about 0.5. Without it: duplicate keys
// 48, merge keys 9.
//
// Mutation: yamlFrontmatterRoot decoding the block into a map first, as
// splitFrontmatter used to, turns both rows red.
func TestFrontmatter_SuperlinearYAMLCostsLikePlainKeys(t *testing.T) {
	const n, reps = maxFrontmatterBytes - 64, 3
	base := fmLines(n, func(i int) string { return fmt.Sprintf("%x:\n", i) })
	var merge strings.Builder
	merge.WriteString("b: &b {")
	for i := 0; merge.Len() < n/2; i++ {
		fmt.Fprintf(&merge, "k%x: 1, ", i)
	}
	merge.WriteString("z: 1}\nc:\n  <<: [" + strings.Repeat("*b,", (n/2-24)/3) + "*b]\n")
	doc := func(block string) []byte { return []byte("---\n" + block + "---\n\n# T\n") }
	for _, tc := range []struct{ name, shape string }{
		{"duplicate keys", strings.Repeat("a: 1\n", n/5)},
		{"merge keys", merge.String()},
	} {
		if len(tc.shape) > maxFrontmatterBytes || len(base) > maxFrontmatterBytes {
			t.Fatalf("%s: a block is over the cap (%d, %d bytes); the test would time the cap", tc.name, len(tc.shape), len(base))
		}
		baseRun, shapeRun := perftest.Amortize(frontmatterWork(doc(base), reps), frontmatterWork(doc(tc.shape), reps))
		perftest.Within(t, "frontmatter with "+tc.name, 4, baseRun, shapeRun)
	}
}

// Each TOML shape BurntSushi/toml decodes superlinearly, the quote desync
// payloads included, costs about what plain keys at the TOML cap cost once
// it is past that cap (perftest.Within, process CPU time): the split
// refuses it before any decode. Measured with the cap: under 0.02.
// Without it, at 4 KiB: 30 to 37 for the inline-table shapes, 82 for the
// table header, 160 to 208 for the dotted key and every desync payload.
//
// Mutation: dropping the TOML cap in splitFrontmatter turns every row red.
func TestFrontmatter_SuperlinearTOMLPastItsCapIsNotDecoded(t *testing.T) {
	// Four times the TOML cap: far enough past it that a decode would
	// show, small enough that a red run takes seconds, not minutes.
	const n, reps = 4 * maxTOMLFrontmatterBytes, 3
	base := fmLines(maxTOMLFrontmatterBytes, func(i int) string { return fmt.Sprintf("k%x = 1\n", i) })
	dots := func(m int) string { return strings.Repeat("a.", m/2) + "b" }
	inline := (n - 10) / 8
	doc := func(block string) []byte { return []byte("+++\n" + block + "+++\n\n# T\n") }
	for _, tc := range []struct{ name, shape string }{
		{"dotted key", dots(n-8) + " = 1\n"},
		{"table header", "[" + dots(n-8) + "]\n"},
		{"inline tables", "a = " + strings.Repeat("{ b = ", inline) + "1" + strings.Repeat(" }", inline) + "\n"},
		{`inline """ desync, one extra quote`, `k = {s = """x"""", ` + dots(n-40) + " = 1}\n"},
		{`inline """ desync, two extra quotes`, `k = {s = """x""""", ` + dots(n-40) + " = 1}\n"},
		{`inline ''' desync, one extra quote`, `k = {s = '''x'''', ` + dots(n-40) + " = 1}\n"},
		{`inline ''' desync, two extra quotes`, `k = {s = '''x''''', ` + dots(n-40) + " = 1}\n"},
		{`top-level """ desync`, `s = """x"""""` + "\n" + dots(n-40) + " = 1\n"},
		{`top-level ''' desync`, `s = '''x''''` + "\n" + dots(n-40) + " = 1\n"},
		{"nested inline desync", `k = {s = """x"""", ` + strings.Repeat("a = { ", inline-8) + "b = 1" + strings.Repeat(" }", inline-8) + "}\n"},
	} {
		if len(tc.shape) <= maxTOMLFrontmatterBytes || len(tc.shape) > maxFrontmatterBytes {
			t.Fatalf("%s: %d bytes is not between the TOML cap and the 16 KiB cap", tc.name, len(tc.shape))
		}
		baseRun, shapeRun := perftest.Amortize(frontmatterWork(doc(base), reps), frontmatterWork(doc(tc.shape), reps))
		perftest.Within(t, "frontmatter with "+tc.name, 4, baseRun, shapeRun)
	}
}
