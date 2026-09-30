package docs

// Frontmatter decode contract (#910, frontmatter_check.go):
//   [x] Happy: a YAML block the map decode accepts is frontmatter, decoded once
//   [x] Sad: a YAML block the map decode refuses is refused without it
//       (duplicate keys at any depth, a non-scalar key, a self-containing
//       anchor, a tag that does not fit, a top level that is not a mapping)
//   [x] Sad: a merge key is refused, since no consumer would apply it
//   [x] Happy: aliases come off the node: a list, a bare string, an alias
//   [x] Sad: a non-string alias value is dropped, as the map decode dropped it
//   [x] Happy: TOML within the bounds is frontmatter and renders once, sorted
//   [x] Sad: a dotted key, header or inline-table nest past the bounds is refused
//   [x] Sad: dots and braces inside strings and comments do not count
//   [x] Unhappy: each superlinear shape costs about what plain keys cost

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

// The TOML bounds count dots and braces outside strings and comments, per
// run between separators. Mutations: not skipping strings turns the string
// rows red; not skipping comments turns the comment row red; not resetting
// at a comma turns the float-array row red; raising either bound turns its
// refused row red.
func TestTOMLFrontmatterBounded(t *testing.T) {
	nine := strings.Repeat("a.", maxTOMLKeyDots+1) + "b"
	eight := strings.Repeat("a.", maxTOMLKeyDots) + "b"
	nest := func(d int) string { return "x = " + strings.Repeat("{b=", d) + "1" + strings.Repeat("}", d) + "\n" }
	for _, tc := range []struct {
		name, block string
		want        bool
	}{
		{"table header", "[params.author]\nname = \"A\"\n", true},
		{"key at the bound", eight + " = 1\n", true},
		{"key past the bound", nine + " = 1\n", false},
		{"header past the bound", "[" + nine + "]\n", false},
		{"array header past the bound", "[[" + nine + "]]\n", false},
		{"spaced key past the bound", strings.ReplaceAll(nine, ".", " . ") + " = 1\n", false},
		{"quoted segments past the bound", strings.Repeat(`"a".`, maxTOMLKeyDots+1) + "b = 1\n", false},
		{"inline-table key past the bound", "x = { " + nine + " = 1 }\n", false},
		{"dots in a basic string", `v = "` + nine + `"` + "\n", true},
		{"dots in a literal string", "v = '" + nine + "'\n", true},
		{"dots in a multi-line string", `v = """` + "\n" + nine + "\n" + `"""` + "\n", true},
		{"dots in a multi-line literal", "v = '''\n" + nine + "\n'''\n", true},
		{"escaped quote in a string", `v = "\"` + nine + `"` + "\n", true},
		{"dots in a comment", "# " + nine + "\nk = 1\n", true},
		{"float array", "f = [1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5]\n", true},
		{"braces in a string", `t = "{{{{{{{{{{{{"` + "\n", true},
		{"inline tables at the bound", nest(maxTOMLInlineDepth), true},
		{"inline tables past the bound", nest(maxTOMLInlineDepth + 1), false},
		{"sibling inline tables", strings.Repeat("{a=1},", 20) + "\n", true},
	} {
		if got := tomlFrontmatterBounded([]byte(tc.block)); got != tc.want {
			t.Errorf("%s: tomlFrontmatterBounded(%q) = %v, want %v", tc.name, tc.block, got, tc.want)
		}
	}
}

// A TOML block renders as a properties block with its keys sorted, and
// one past the bounds renders as text. Mutation: frontmatterHTML returning
// "" for a +++ block turns the first half red; tomlFrontmatterBounded
// returning true turns the second half red.
func TestRender_TOMLFrontmatter(t *testing.T) {
	got, err := Render([]byte("+++\ntitle = \"Tee\"\nauthor = \"Ay\"\n+++\n\n# T\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	ia, it := strings.Index(got, ">author</span>"), strings.Index(got, ">title</span>")
	if !strings.Contains(got, "data-forgectl-props") || ia < 0 || it < 0 || ia > it {
		t.Errorf("TOML frontmatter is not a sorted properties block:\n%s", got)
	}
	got, err = Render([]byte("+++\n" + strings.Repeat("a.", maxTOMLKeyDots+1) + "b = 1\n+++\n\n# T\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "data-forgectl-props") {
		t.Errorf("a TOML block past the bounds rendered as a properties block:\n%s", got)
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

// frontmatterWork is everything a page and the index do with a document's
// frontmatter: the split, the properties block and the aliases.
func frontmatterWork(src []byte, reps int) func() {
	return func() {
		for range reps {
			fm, ok := splitFrontmatter(src)
			frontmatterSink += len(frontmatterHTML(fm, ok))
			if root := frontmatterRoot(fm); root != nil {
				frontmatterSink += len(aliasesFromNode(root))
			}
		}
	}
}

// Each shape that made a decode superlinear costs, at the cap, about what
// plain keys of the same size cost (perftest.Within, process CPU time).
// Measured with the fix: about 0.5 for the YAML shapes and under 0.01 for
// the TOML ones, which are refused before any decode. Without it:
// duplicate keys 48, merge keys 9, a dotted key 129, a table header 53,
// nested inline tables 29.
//
// Mutations: yamlFrontmatterRoot decoding the block into a map first, as
// splitFrontmatter used to, turns the YAML rows red; tomlFrontmatterBounded
// returning true turns the TOML rows red.
func TestFrontmatter_SuperlinearShapesCostLikePlainKeys(t *testing.T) {
	const n, reps = maxFrontmatterBytes - 64, 3
	yamlBase := fmLines(n, func(i int) string { return fmt.Sprintf("%x:\n", i) })
	tomlBase := fmLines(n, func(i int) string { return fmt.Sprintf("k%x = 1\n", i) })
	var merge strings.Builder
	merge.WriteString("b: &b {")
	for i := 0; merge.Len() < n/2; i++ {
		fmt.Fprintf(&merge, "k%x: 1, ", i)
	}
	merge.WriteString("z: 1}\nc:\n  <<: [" + strings.Repeat("*b,", (n/2-24)/3) + "*b]\n")
	inline := (n - 10) / 8
	for _, tc := range []struct {
		name, fence, base, shape string
	}{
		{"duplicate keys", "---", yamlBase, strings.Repeat("a: 1\n", n/5)},
		{"merge keys", "---", yamlBase, merge.String()},
		{"dotted key", "+++", tomlBase, strings.Repeat("a.", (n-8)/2) + "b = 1\n"},
		{"table header", "+++", tomlBase, "[" + strings.Repeat("a.", (n-8)/2) + "b]\n"},
		{"inline tables", "+++", tomlBase, "a = " + strings.Repeat("{ b = ", inline) + "1" + strings.Repeat(" }", inline) + "\n"},
	} {
		doc := func(block string) []byte { return []byte(tc.fence + "\n" + block + tc.fence + "\n\n# T\n") }
		if len(tc.shape) > maxFrontmatterBytes || len(tc.base) > maxFrontmatterBytes {
			t.Fatalf("%s: a block is over the cap (%d, %d bytes); the test would time the cap", tc.name, len(tc.shape), len(tc.base))
		}
		perftest.Within(t, "frontmatter with "+tc.name, 4, frontmatterWork(doc(tc.base), reps), frontmatterWork(doc(tc.shape), reps))
	}
}
