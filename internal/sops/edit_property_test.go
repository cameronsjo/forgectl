package sops

// A property test over document shapes SetScalar was not designed around.
//
// The table in edit_test.go pins exact output for shapes the line model DOES
// handle, and the refusal table pins the shapes it rejects by name. Neither
// answers the question that actually matters for an encrypted file: for
// everything else — the YAML features nobody thought to enumerate — does
// SetScalar either refuse, or leave the document correct?
//
// So this asserts an invariant rather than an output. For each input, one of
// exactly two things must be true:
//
//   - it refuses, which is always acceptable, or
//   - it emits a document that still PARSES, retains every top-level key it
//     had, and has the requested value reachable at the requested path.
//
// A mis-bounded block fails this three ways at once: the emitted document
// stops parsing (a map/sequence mix), or a top-level key vanishes (the key
// landed in the wrong block and displaced something), or the value is not
// where it was asked for. That is the failure worth catching, because in an
// encrypted file none of it is visible until a consumer reads the wrong value.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mapLookup reads key from either map shape yaml.v3 produces.
//
// A mapping with a non-string key anywhere — `123: 'v'` is the case here —
// decodes as map[interface{}]interface{} rather than map[string]interface{},
// so a walk that only handles the latter fails on the TYPE while the write it
// was checking is perfectly correct. Narrowing the assertion to one map shape
// would have reported a bug in SetScalar that does not exist.
func mapLookup(node any, key string) (any, bool) {
	switch m := node.(type) {
	case map[string]any:
		v, ok := m[key]
		return v, ok
	case map[any]any:
		v, ok := m[key]
		return v, ok
	default:
		return nil, false
	}
}

func TestSetScalar_ShapeInvariant(t *testing.T) {
	const probe = "PROBEVALUE"

	cases := []struct {
		name string
		doc  string
		path []string
	}{
		{"an anchor on the block header", "block: &anc\n    k: 'v'\nother:\n    m: 'n'\n", []string{"block", "new"}},
		{"an alias as a sibling's value", "base:\n    k: &a 'v'\nblock:\n    m: *a\n", []string{"block", "new"}},
		{"a merge key inside the block", "base: &b\n    x: '1'\nblock:\n    <<: *b\n    k: 'v'\n", []string{"block", "new"}},
		{"a flow mapping as the block", "block: {a: 1, b: 2}\n", []string{"block", "new"}},
		{"a flow mapping as a sibling", "block:\n    inner: {a: 1}\n    k: 'v'\n", []string{"block", "new"}},
		{"a double-quoted key", "block:\n    \"quoted key\": 'v'\n", []string{"block", "new"}},
		{"a single-quoted key", "block:\n    'sq': 'v'\n", []string{"block", "new"}},
		{"a key with the same name as its block", "block:\n    block: 'v'\n", []string{"block", "new"}},
		{"a leaf that is a prefix of a sibling", "block:\n    newer: 'v'\n", []string{"block", "new"}},
		{"a sops block that is not last", "sops:\n    mac: 'x'\nblock:\n    k: 'v'\n", []string{"block", "new"}},
		{"a literal block scalar sibling", "block:\n    text: |\n        line one\n        line two\n    k: 'v'\n", []string{"block", "new"}},
		{"a folded block scalar sibling", "block:\n    text: >\n        folded\n    k: 'v'\n", []string{"block", "new"}},
		{"a nested block of the same name", "block:\n    block:\n        k: 'v'\n    k: 'w'\n", []string{"block", "new"}},
		{"a deeper sibling before a shallower one", "block:\n    a:\n        deep: '1'\n    k: 'v'\n", []string{"block", "new"}},
		{"a sibling with an empty value", "block:\n    empty:\n    k: 'v'\n", []string{"block", "new"}},
		{"CRLF with a trailing comment", "block:\r\n    k: 'v'  # note\r\n", []string{"block", "k"}},
		{"a document-end marker", "block:\n    k: 'v'\n...\n", []string{"block", "new"}},
		{"a tab inside a value rather than the indent", "block:\n    k: \"has\ttab\"\n", []string{"block", "new"}},
		{"a three-space indent", "block:\n   k: 'v'\n", []string{"block", "new"}},
		{"a one-space indent", "block:\n k: 'v'\n", []string{"block", "new"}},
		{"a ten-space indent", "block:\n          k: 'v'\n", []string{"block", "new"}},
		{"a blank line between siblings", "block:\n    a: '1'\n\n    b: '2'\n", []string{"block", "new"}},
		{"a value that looks like a nested key", "block:\n    k: 'a: b'\n", []string{"block", "new"}},
		{"a numeric-looking key", "block:\n    123: 'v'\n", []string{"block", "new"}},
		{"a null value sibling", "block:\n    k: null\n", []string{"block", "new"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var before map[string]any
			inputParses := yaml.Unmarshal([]byte(c.doc), &before) == nil

			got, _, err := SetScalar([]byte(c.doc), c.path, probe)
			if err != nil {
				// A refusal is always an acceptable answer. The named-shape
				// refusals are pinned by TestSetScalar_Refusals; here the only
				// requirement is that refusing leaves nothing behind.
				if got != nil {
					t.Errorf("refused but returned a document: %q", got)
				}
				return
			}

			var after map[string]any
			if perr := yaml.Unmarshal(got, &after); perr != nil {
				if !inputParses {
					t.Skip("the input was not valid YAML either; not a regression")
				}
				t.Fatalf("accepted a valid document and emitted an unparseable one: %v\n%s", perr, got)
			}

			for key := range before {
				if _, ok := after[key]; !ok {
					t.Errorf("top-level key %q disappeared — the key landed in the wrong block", key)
				}
			}

			cur := any(after)
			for _, segment := range c.path {
				next, ok := mapLookup(cur, segment)
				if !ok {
					t.Fatalf("path %v is not reachable: found %T at %q", c.path, cur, segment)
				}
				cur = next
			}
			s, ok := cur.(string)
			if !ok || !strings.Contains(s, probe) {
				t.Errorf("value at %v decoded as %#v, want %q", c.path, cur, probe)
			}
		})
	}
}
