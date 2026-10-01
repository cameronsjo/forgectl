package yamlsafe

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestParseRefusesInputOverItsCap: input of exactly maxBytes parses, and one
// byte more is refused, never cut, with an error naming the size and the cap.
//
// Mutations that turn it red: compare with >= (the at-cap input is refused);
// drop the size check (the over-cap input parses).
func TestParseRefusesInputOverItsCap(t *testing.T) {
	const capBytes = 64
	at := "a: " + strings.Repeat("x", capBytes-4) + "\n"
	if len(at) != capBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(at), capBytes)
	}
	doc, err := Parse([]byte(at), capBytes)
	if err != nil {
		t.Fatalf("Parse at the cap: %v", err)
	}
	if root := Root(doc); root == nil || root.Kind != yaml.MappingNode {
		t.Fatalf("Parse at the cap: root = %v, want a mapping", root)
	}
	_, err = Parse([]byte(at+"#"), capBytes)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Parse one byte over: err = %v, want ErrTooLarge", err)
	}
	if !strings.Contains(err.Error(), "65 bytes") || !strings.Contains(err.Error(), "64-byte limit") {
		t.Errorf("Parse one byte over: %q, want the size and the limit", err)
	}
}

// TestRootOfEmptyInput: empty input and a comment-only document have no
// root, which callers read as "no keys", not as an error.
func TestRootOfEmptyInput(t *testing.T) {
	for _, in := range []string{"", "# only a comment\n"} {
		doc, err := Parse([]byte(in), 1024)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if root := Root(doc); root != nil {
			t.Errorf("Root(Parse(%q)) = %v, want nil", in, root)
		}
	}
}

// TestCheckTree pins the refusals: each shape a map decode refuses, a merge
// key, and a mapping over the key cap; and the acceptances: aliases, nested
// plain data, and a mapping at the cap. Docs' frontmatter tests
// (internal/docs/frontmatter_check_test.go) cover the same walk through
// yamlFrontmatterRoot.
//
// Mutations that turn it red: drop the maxKeys check (the wide mapping
// passes); compare len(n.Content) rather than its half against maxKeys (the
// at-cap mapping is refused); drop the CheckKeys call (the duplicate, merge
// and non-scalar keys pass).
func TestCheckTree(t *testing.T) {
	wide := func(n int) string {
		var b strings.Builder
		b.WriteString("top:\n")
		for i := range n {
			b.WriteString("  k")
			b.WriteString(strings.Repeat("x", i))
			b.WriteString(": 1\n")
		}
		return b.String()
	}
	for _, tc := range []struct {
		name    string
		doc     string
		maxKeys int
		want    string // "" means accepted
	}{
		{"plain nested", "a:\n  b: [1, 2]\n  c: {d: e}\n", 0, ""},
		{"alias to a mapping", "a: &x {b: 1}\nc: *x\n", 0, ""},
		{"duplicate key, nested", "a:\n  b: 1\n  b: 2\n", 0, "line 3: a mapping key is repeated"},
		{"merge key", "a: &x {b: 1}\nc:\n  <<: *x\n", 0, "line 3: a merge key"},
		{"mapping as a key", "? {a: 1}\n: 2\n", 0, "a key is a sequence or a mapping"},
		{"alias key to a mapping", "a: &x {b: 1}\n*x : 2\n", 0, "a key is an alias to a sequence or a mapping"},
		{"self-containing anchor", "a: &x [*x]\n", 0, "an alias refers to a node that contains it"},
		{"tag that does not fit", "a: !!int abc\n", 0, "does not fit its explicit tag"},
		{"at the key cap", wide(8), 8, ""},
		{"over the key cap", wide(9), 8, "line 2: a mapping has 9 keys, over the limit of 8"},
		{"no key cap", wide(200), 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.doc), 1<<20)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = CheckTree(Root(doc), tc.maxKeys)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("CheckTree = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("CheckTree = nil, want %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("CheckTree = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCheckTreeErrorsDoNotEchoTheDocument: a refusal names a line, never the
// key or value, since the document is untrusted text.
//
// Mutation that turns it red: put k.Value in the repeated-key error.
func TestCheckTreeErrorsDoNotEchoTheDocument(t *testing.T) {
	const secret = "hunter2\x1b[31m"
	doc, err := Parse([]byte("\""+strings.ReplaceAll(secret, "\x1b", "\\e")+"\": 1\n\""+strings.ReplaceAll(secret, "\x1b", "\\e")+"\": 2\n"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckTree(Root(doc), 0)
	if err == nil {
		t.Fatal("CheckTree accepted a repeated key")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("CheckTree's error %q echoes the key", err)
	}
}
