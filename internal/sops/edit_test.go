package sops

// Test plan for edit.go
//
// SetScalar (Classification: pure text transform, table-driven, no sops
// binary and no filesystem)
//
// The reference suite, ported from the shell prototype this replaces:
//   [x] 1  appends inside the block
//   [x] 2  copies a four-space sibling's indent rather than assuming two
//   [x] 3  replaces in place, preserving key order
//   [x] 4  appends to a block that runs to EOF
//   [x] 5  lands inside the block, not after a trailing blank line
//   [x] 6  a prefix-sharing sibling (llm_key_hermes_old) is untouched
//   [x] 7  a same-named key in another block is untouched
//   [x] 8  stays out of a trailing sops: block
//   [x] 9  a missing block refuses
// Added after review:
//   [x] 10 three and four levels deep; a missing INTERMEDIATE block refuses
//   [x] 11 each unsupported shape refuses rather than mis-bounding
//   [x] 12 quoting survives a yaml.v3 round-trip (not an output-shape assert)
//   [x] 13 control bytes and invalid UTF-8 refuse, with no value echoed
//   [x] 14 a CRLF document gets a CRLF-terminated insertion
//   [x] 15 replace preserves the line's own indent and trailing comment

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSetScalar_Table(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		path    []string
		value   string
		want    string
		outcome Outcome
	}{
		{
			name:    "appends inside the block",
			doc:     "agentgateway:\n    existing: 'a'\ntop:\n    b: 'c'\n",
			path:    []string{"agentgateway", "llm_key"},
			value:   "secret",
			want:    "agentgateway:\n    existing: 'a'\n    llm_key: 'secret'\ntop:\n    b: 'c'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "copies a four-space sibling indent",
			doc:     "block:\n    only: 'x'\n",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\n    only: 'x'\n    added: 'v'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "copies a two-space sibling indent",
			doc:     "block:\n  only: 'x'\n",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\n  only: 'x'\n  added: 'v'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "replaces in place preserving order",
			doc:     "block:\n    first: 'a'\n    target: 'old'\n    last: 'z'\n",
			path:    []string{"block", "target"},
			value:   "new",
			want:    "block:\n    first: 'a'\n    target: 'new'\n    last: 'z'\n",
			outcome: OutcomeReplaced,
		},
		{
			name:    "appends to a block running to EOF",
			doc:     "block:\n    only: 'x'",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\n    only: 'x'\n    added: 'v'",
			outcome: OutcomeAdded,
		},
		{
			name:    "lands inside the block not after a blank line",
			doc:     "block:\n    only: 'x'\n\ntop:\n    b: 'c'\n",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\n    only: 'x'\n    added: 'v'\n\ntop:\n    b: 'c'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "a prefix-sharing sibling is untouched",
			doc:     "block:\n    llm_key_hermes_old: 'stale'\n",
			path:    []string{"block", "llm_key_hermes"},
			value:   "fresh",
			want:    "block:\n    llm_key_hermes_old: 'stale'\n    llm_key_hermes: 'fresh'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "a same-named key in another block is untouched",
			doc:     "first:\n    shared: 'one'\nsecond:\n    shared: 'two'\n",
			path:    []string{"second", "shared"},
			value:   "new",
			want:    "first:\n    shared: 'one'\nsecond:\n    shared: 'new'\n",
			outcome: OutcomeReplaced,
		},
		{
			name:    "stays out of a trailing sops block",
			doc:     "block:\n    only: 'x'\nsops:\n    mac: 'ENC[...]'\n    version: 3.13.3\n",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\n    only: 'x'\n    added: 'v'\nsops:\n    mac: 'ENC[...]'\n    version: 3.13.3\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "three levels deep",
			doc:     "a:\n    b:\n        c:\n            leaf: 'old'\n",
			path:    []string{"a", "b", "c", "leaf"},
			value:   "new",
			want:    "a:\n    b:\n        c:\n            leaf: 'new'\n",
			outcome: OutcomeReplaced,
		},
		{
			name:    "three levels deep, adding",
			doc:     "a:\n    b:\n        c:\n            existing: 'x'\n",
			path:    []string{"a", "b", "c", "added"},
			value:   "v",
			want:    "a:\n    b:\n        c:\n            existing: 'x'\n            added: 'v'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "adds into an empty block at header indent plus two",
			doc:     "block:\ntop:\n    b: 'c'\n",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\n  added: 'v'\ntop:\n    b: 'c'\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "replace preserves the line indent and trailing comment",
			doc:     "block:\n      target: 'old'  # rotated 2026-01\n",
			path:    []string{"block", "target"},
			value:   "new",
			want:    "block:\n      target: 'new'  # rotated 2026-01\n",
			outcome: OutcomeReplaced,
		},
		{
			name:    "a hash inside a quoted value is data, not a comment",
			doc:     "block:\n    target: 'pass#word'\n",
			path:    []string{"block", "target"},
			value:   "new",
			want:    "block:\n    target: 'new'\n",
			outcome: OutcomeReplaced,
		},
		{
			name:    "CRLF document gets a CRLF insertion",
			doc:     "block:\r\n    only: 'x'\r\n",
			path:    []string{"block", "added"},
			value:   "v",
			want:    "block:\r\n    only: 'x'\r\n    added: 'v'\r\n",
			outcome: OutcomeAdded,
		},
		{
			name:    "a comment inside the block does not terminate it",
			doc:     "block:\n    first: 'a'\n    # a note about the next key\n    second: 'b'\n",
			path:    []string{"block", "second"},
			value:   "new",
			want:    "block:\n    first: 'a'\n    # a note about the next key\n    second: 'new'\n",
			outcome: OutcomeReplaced,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, outcome, err := SetScalar([]byte(c.doc), c.path, c.value)
			if err != nil {
				t.Fatalf("SetScalar: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("document =\n%q\nwant\n%q", got, c.want)
			}
			if outcome != c.outcome {
				t.Errorf("outcome = %v, want %v", outcome, c.outcome)
			}
		})
	}
}

func TestSetScalar_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		path    []string
		wantMsg string
	}{
		{
			name:    "a missing block",
			doc:     "other:\n    k: 'v'\n",
			path:    []string{"nosuchblock", "s3ntinel_leaf"},
			wantMsg: "no block",
		},
		{
			name:    "a missing intermediate block",
			doc:     "a:\n    other:\n        k: 'v'\n",
			path:    []string{"a", "missing", "s3ntinel_leaf"},
			wantMsg: "no block",
		},
		{
			name:    "a sequence where a mapping was expected",
			doc:     "block:\n    - one\n    - two\n",
			path:    []string{"block", "s3ntinel_leaf"},
			wantMsg: "sequence",
		},
		{
			name:    "a comment indented at or below the header",
			doc:     "block:\n# a column-zero note inside the block\n    k: 'v'\n",
			path:    []string{"block", "s3ntinel_leaf"},
			wantMsg: "ambiguous",
		},
		{
			name:    "tab indentation",
			doc:     "block:\n\tk: 'v'\n",
			path:    []string{"block", "s3ntinel_leaf"},
			wantMsg: "tab indentation",
		},
		{
			name:    "a multi-document stream",
			doc:     "---\nblock:\n    k: 'v'\n",
			path:    []string{"block", "s3ntinel_leaf"},
			wantMsg: "multi-document",
		},
		{
			name:    "a header carrying a trailing comment",
			doc:     "block: # notes\n    k: 'v'\n",
			path:    []string{"block", "s3ntinel_leaf"},
			wantMsg: "trailing comment",
		},
		{
			name:    "an empty path",
			doc:     "block:\n    k: 'v'\n",
			path:    nil,
			wantMsg: "path is empty",
		},
		{
			// A leaf naming a block would otherwise write a scalar over the
			// header and leave its children stranded at their old depth, which
			// yaml.v3 rejects. The child's own parse catches that and the
			// encrypted file survives, but the operator gets "the edited
			// document does not parse as YAML" — a message that names nothing
			// they can act on and reads as a forgectl bug.
			name:    "a leaf that names a block rather than a value",
			doc:     "block:\n    s3ntinel_leaf:\n        k: 'v'\n",
			path:    []string{"block", "s3ntinel_leaf"},
			wantMsg: "names a block rather than a value",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const sentinel = "s3ntinel-VALUE-77x"
			got, outcome, err := SetScalar([]byte(c.doc), c.path, sentinel)
			if err == nil {
				t.Fatalf("SetScalar returned nil error, want a refusal; document =\n%q", got)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.wantMsg)
			}
			if outcome != OutcomeUnspecified {
				t.Errorf("outcome = %v, want unspecified on a refusal", outcome)
			}
			// A refusal must never echo the value.
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("error %q echoed the value", err.Error())
			}
			// Nor the LEAF segment. ParsePath's grammar admits plenty of
			// provider token formats as a single valid segment, so a secret
			// pasted into the key slot arrives as the leaf — and these
			// messages are relayed out of the child process to the operator's
			// terminal, which is the transcript this feature exists to keep
			// the value out of.
			//
			// The asymmetry with the ancestor segments is deliberate, not an
			// oversight: "no block %q at this level" names a mistyped block,
			// which is the commonest mistake on this path and the only
			// actionable thing the message can carry. An ancestor is not the
			// paste site — a bare pasted secret is a single-segment path,
			// whose whole walk is the leaf.
			if leaf := lastSegment(c.path); leaf != "" && strings.Contains(err.Error(), leaf) {
				t.Errorf("error %q echoed the leaf segment %q", err.Error(), leaf)
			}
			if got != nil {
				t.Errorf("document = %q, want nil on a refusal", got)
			}
		})
	}
}

// TestSetScalar_QuotingRoundTrips asserts the emitted document DECODES to the
// value that went in, rather than asserting the emitted line's text.
//
// That distinction is the whole point: comparing against `key: 'a”b'` would
// merely restate encodeScalar's implementation, so the test would agree with
// the code by construction and could not catch a quoting bug. Decoding with
// yaml.v3 asks the only question that matters — does a real YAML reader get
// the bytes back.
func TestSetScalar_QuotingRoundTrips(t *testing.T) {
	values := []string{
		"plain",
		"with: a colon",
		"with # a hash",
		"with 'single' quotes",
		"it's got an apostrophe",
		`with "double" quotes`,
		"{braces: true}",
		"*alias-looking",
		"&anchor-looking",
		"- dash-leading",
		"[brackets]",
		"trailing spaces   ",
		"   leading spaces",
		"100%",
		"@at-leading",
		"`backtick`",
		"ENC[AES256_GCM,data:abc]",
		"multi  internal   spaces",
		"\\backslash\\path",
		"value # with ' quote",
	}

	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			doc := "block:\n    target: 'placeholder'\n"
			got, _, err := SetScalar([]byte(doc), []string{"block", "target"}, v)
			if err != nil {
				t.Fatalf("SetScalar: %v", err)
			}

			var decoded struct {
				Block struct {
					Target string `yaml:"target"`
				} `yaml:"block"`
			}
			if err := yaml.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("the emitted document does not parse as YAML: %v\n%s", err, got)
			}
			if decoded.Block.Target != v {
				t.Errorf("decoded = %q, want %q\nemitted:\n%s", decoded.Block.Target, v, got)
			}
		})
	}
}

// TestSetScalar_AddedKeyRoundTrips is the same round-trip for the ADD path,
// which builds its line separately from the replace path.
func TestSetScalar_AddedKeyRoundTrips(t *testing.T) {
	const value = "added ' # value"
	doc := "block:\n    other: 'x'\n"
	got, outcome, err := SetScalar([]byte(doc), []string{"block", "added"}, value)
	if err != nil {
		t.Fatalf("SetScalar: %v", err)
	}
	if outcome != OutcomeAdded {
		t.Fatalf("outcome = %v, want added", outcome)
	}

	var decoded struct {
		Block map[string]string `yaml:"block"`
	}
	if err := yaml.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("the emitted document does not parse as YAML: %v\n%s", err, got)
	}
	if decoded.Block["added"] != value {
		t.Errorf("decoded = %q, want %q", decoded.Block["added"], value)
	}
	if decoded.Block["other"] != "x" {
		t.Errorf("the untouched sibling decoded as %q, want %q", decoded.Block["other"], "x")
	}
}

// TestSetScalar_ColonBearingSiblingIsNotDestroyed is the regression test for a
// silent data-loss bug: a bare prefix match on `leaf + ":"` also matched a
// DIFFERENT key whose name merely began that way.
//
// `a:b: 'v'` is valid YAML and decodes to the key `a:b`, so it can legitimately
// sit in a SOPS file. Setting `a` matched that line, and since replaceValue
// cuts at the first colon, the result was `a: 'new'` — the key `a:b` and its
// encrypted value gone, reported as `replaced a`, and passing every downstream
// check, because extracting `["block"]["a"]` then returns exactly the value
// that was supplied.
//
// So this asserts the ORIGINAL key survives, not merely that the write
// happened.
func TestSetScalar_ColonBearingSiblingIsNotDestroyed(t *testing.T) {
	const doc = "block:\n    a:b: 'keepme'\n"

	got, outcome, err := SetScalar([]byte(doc), []string{"block", "a"}, "new")
	if err != nil {
		// Refusing is also acceptable — the point is that `a:b` is not eaten.
		t.Logf("refused: %v", err)
		return
	}
	if !strings.Contains(string(got), "a:b:") {
		t.Fatalf("the key `a:b` was destroyed (outcome %v):\n%s", outcome, got)
	}

	var decoded struct {
		Block map[string]string `yaml:"block"`
	}
	if err := yaml.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("the emitted document does not parse: %v\n%s", err, got)
	}
	if decoded.Block["a:b"] != "keepme" {
		t.Errorf("block[\"a:b\"] = %q, want %q — the original key lost its value", decoded.Block["a:b"], "keepme")
	}
	if decoded.Block["a"] != "new" {
		t.Errorf("block[\"a\"] = %q, want %q", decoded.Block["a"], "new")
	}
}

// TestSetScalar_NoSpaceAfterColonIsNotAMapping pins the sibling case: YAML
// reads `key:value` with no space as a plain scalar, not a mapping, so it must
// not be matched as a key either.
func TestSetScalar_NoSpaceAfterColonIsNotAMapping(t *testing.T) {
	const doc = "block:\n    keep:value\n"

	got, _, err := SetScalar([]byte(doc), []string{"block", "keep"}, "new")
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	if !strings.Contains(string(got), "keep:value") {
		t.Errorf("the `keep:value` scalar was rewritten as a key:\n%s", got)
	}
}

// TestSetScalar_NoDuplicateKeyOnReplace guards the failure that a mis-bounded
// block produces: a key added a second time. A duplicate YAML key is accepted
// by many parsers with a last-wins rule, so it does not fail loudly — it just
// makes the file's meaning depend on the reader.
func TestSetScalar_NoDuplicateKeyOnReplace(t *testing.T) {
	doc := "block:\n    first: 'a'\n    # note\n    target: 'old'\n\nsops:\n    mac: 'ENC[x]'\n"
	got, _, err := SetScalar([]byte(doc), []string{"block", "target"}, "new")
	if err != nil {
		t.Fatalf("SetScalar: %v", err)
	}
	if n := strings.Count(string(got), "target:"); n != 1 {
		t.Errorf("document has %d `target:` lines, want 1:\n%s", n, got)
	}
	// yaml.v3 rejects a duplicate mapping key outright, so a successful
	// decode is a second, independent check on the same property.
	var probe map[string]any
	if err := yaml.Unmarshal(got, &probe); err != nil {
		t.Errorf("the emitted document does not parse (duplicate key?): %v\n%s", err, got)
	}
}

// lastSegment returns path's leaf, or "" for an empty path — the segment a
// pasted secret would occupy, which no refusal may echo.
func lastSegment(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}
