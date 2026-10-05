package sops

// Test plan for the #959 bounds on file.go and driver.go
//
//   [x] Happy: isSOPSFile and readPlaintextRules read every document in the
//       corpus exactly as a decode of the whole document did
//   [x] Sad: a merge key at the top level or in the sops: block is refused
//       rather than expanded
//   [x] Sad: a document one byte over MaxDocumentBytes is refused by name;
//       one at the cap is read
//   [x] Sad: a re-read over MaxRewrittenBytes is refused by name
//   [x] Unhappy: many keys at the top level or in the sops: block cost
//       about what the same keys nested out of reach cost
//   [x] sopsBlockKeys names every sopsBlock field

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cameronsjo/forgectl/internal/perftest"
)

// isSOPSFileWhole is isSOPSFile before #959: a decode of the whole document.
func isSOPSFileWhole(data []byte) bool {
	var probe map[string]yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	node, ok := probe["sops"]
	return ok && node.Kind == yaml.MappingNode
}

// readMetadataWhole is readPlaintextRules' decode before #959.
func readMetadataWhole(data []byte) (metadata, error) {
	var meta metadata
	err := yaml.Unmarshal(data, &meta)
	return meta, err
}

// boundCorpus is the documents the pruned readers must read as the whole
// decode did: every top-level and sops: block shape the struct and map
// decodes treat specially (aliases, tags, nulls, duplicates, wrong kinds).
// c29wcw== is base64 for "sops".
var boundCorpus = []string{
	"",
	"~\n",
	"just a scalar\n",
	"- a\n- b\n",
	"key: ENC[AES256_GCM,data:abc]\nsops:\n    mac: ENC[x]\n    version: 3.13.3\n    unencrypted_suffix: _u\n",
	"key: value\nother: thing\n",
	"sops: ~\n",
	"sops: scalar\n",
	"sops: [a, b]\n",
	"sops: {}\n",
	"anchor: &s {mac: m, encrypted_regex: '^data$', extra: 1}\nsops: *s\n",
	"v: &v 3.13.3\nsops:\n  version: *v\n  mac: x\n",
	"sops:\n  version: 3\n  mac: ~\n  unencrypted_regex: '^x'\n",
	"!!binary c29wcw==: {mac: m, encrypted_suffix: _e}\n",
	"!!binary c29wcw==: {mac: a}\nsops: {mac: b}\n",
	"sops:\n  !!str unencrypted_suffix: _plain\n  mac: m\n",
	"a: 1\na: 2\nsops: {mac: m}\n",
	"sops: {mac: a}\nsops: {mac: b}\n",
	"sops: {mac: a, mac: b}\n",
	"data: {a: 1, a: 2}\nsops: {mac: m}\n",
	"sops:\n  mac: {nested: map}\n",
	"sops:\n  version: [3]\n",
	"1: x\nsops: {mac: m}\n",
	"!!binary not-base64: x\nsops: {mac: m}\n",
	"sops:\n  !!binary not-base64: x\n  mac: m\n",
	"? [a]\n: x\nsops: {mac: m}\n",
	"sops:\n  ? {a: 1}\n  : x\n",
	"sops: {mac: m, extra: {a: 1, a: 2}}\n",
	"key: [unclosed\n",
}

// TestBoundedReadersMatchTheWholeDecode is #959's equivalence: pruning the
// document to the keys the checks read changes nothing about what they
// return, refusals included.
//
// Mutations that turn it red: match keepKeys' keys by Value rather than by
// the decoded name (the !!binary rows diverge); skip the CheckKeys call in
// keepKeys (the row with a repeated top-level key that is not sops: is
// read, where the whole decode refused it).
func TestBoundedReadersMatchTheWholeDecode(t *testing.T) {
	for _, doc := range boundCorpus {
		data := []byte(doc)
		if got, want := isSOPSFile(data), isSOPSFileWhole(data); got != want {
			t.Errorf("isSOPSFile(%q) = %v, the whole decode said %v", doc, got, want)
		}
		got, gotErr := readMetadata(data)
		want, wantErr := readMetadataWhole(data)
		if (gotErr != nil) != (wantErr != nil) {
			t.Errorf("readMetadata(%q) err = %v, the whole decode's = %v", doc, gotErr, wantErr)
			continue
		}
		// On an error the whole decode returns what it filled before
		// failing, and nothing reads that; only a success's value counts.
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("readMetadata(%q) = %+v, the whole decode's = %+v", doc, got, want)
		}
	}
}

// TestBoundedReadersRefuseMergeKeys: a merge key in the top level or the
// sops: block is refused, where the whole decode applied it. Refusing fails
// closed: a rule merged in from elsewhere cannot go unread.
//
// Mutation that turns it red: drop the merge-key refusal in
// yamlsafe.CheckKeys.
func TestBoundedReadersRefuseMergeKeys(t *testing.T) {
	for _, doc := range []string{
		"base: &b {sops: {mac: m}}\n<<: *b\n",
		"rules: &r {unencrypted_regex: '^x'}\nsops:\n  <<: *r\n  mac: m\n",
	} {
		if _, err := readPlaintextRules([]byte(doc)); err == nil || !strings.Contains(err.Error(), "merge key") {
			t.Errorf("readPlaintextRules(%q) err = %v, want a merge-key refusal", doc, err)
		}
	}
	if isSOPSFile([]byte("base: &b {sops: {mac: m}}\n<<: *b\n")) {
		t.Error("isSOPSFile accepted a sops: block merged into the top level")
	}
}

// padTo returns a SOPS document padded with a trailing comment to n bytes.
func padTo(t *testing.T, n int) []byte {
	t.Helper()
	head := "key: ENC[AES256_GCM,data:abc]\nsops:\n    mac: ENC[x]\n    version: 3.13.3\n#"
	if n < len(head)+1 {
		t.Fatalf("cannot pad to %d bytes", n)
	}
	return []byte(head + strings.Repeat("x", n-len(head)-1) + "\n")
}

// TestReadersRefuseADocumentOverTheCap: at MaxDocumentBytes the document
// is read; one byte more and CheckSize and readPlaintextRules name the
// limit, and isSOPSFile says no. The file is never cut short.
//
// Mutations that turn it red: drop CheckSize from parseTop (the over-cap
// document is read); compare with >= in CheckSize (the at-cap one is
// refused).
func TestReadersRefuseADocumentOverTheCap(t *testing.T) {
	at := padTo(t, MaxDocumentBytes)
	if err := CheckSize(at); err != nil {
		t.Fatalf("CheckSize at the cap: %v", err)
	}
	if !isSOPSFile(at) {
		t.Fatal("isSOPSFile refused a document at the cap")
	}
	if _, err := readPlaintextRules(at); err != nil {
		t.Fatalf("readPlaintextRules at the cap: %v", err)
	}

	over := padTo(t, MaxDocumentBytes+1)
	if err := CheckSize(over); err == nil || !strings.Contains(err.Error(), "larger than 4 MiB") {
		t.Errorf("CheckSize one byte over: %v, want the limit named", err)
	}
	if isSOPSFile(over) {
		t.Error("isSOPSFile accepted a document over the cap")
	}
	if _, err := readPlaintextRules(over); err == nil || !strings.Contains(err.Error(), "larger than 4 MiB") {
		t.Errorf("readPlaintextRules one byte over: %v, want the limit named", err)
	}
}

// TestAssertEncryptedAtPathRefusesAnOversizedReread: the re-read is held to
// MaxRewrittenBytes and refused by name past it, not parsed.
//
// Mutation that turns it red: pass a limit of 1<<62 to yamlsafe.Parse in
// assertEncryptedAtPath.
func TestAssertEncryptedAtPathRefusesAnOversizedReread(t *testing.T) {
	doc := "a:\n    b: ENC[AES256_GCM,data:x]\n#"
	at := doc + strings.Repeat("x", MaxRewrittenBytes-len(doc))
	if err := assertEncryptedAtPath([]byte(at), []string{"a", "b"}); err != nil {
		t.Fatalf("assertEncryptedAtPath at the cap: %v", err)
	}
	err := assertEncryptedAtPath([]byte(at+"x"), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "larger than 5 MiB") {
		t.Fatalf("assertEncryptedAtPath one byte over: %v, want the limit named", err)
	}
}

// keyLines returns lines of distinct keys at indent, about n bytes in all.
func keyLines(n int, indent string) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		_, _ = fmt.Fprintf(&b, "%sk%d: v\n", indent, i)
	}
	return b.String()
}

// TestReadersCostLikeKeysOutOfReach: many keys at the top level, or in the
// sops: block, cost about what the same keys nested under a key nobody
// reads cost (perftest.Within, process CPU time). The whole decode compared
// every key of each mapping it decoded with every later one, and those two
// mappings were the ones it decoded. Measured with the fix: about 1.
// Without it, at 128 KiB: well over 20.
//
// Mutation that turns it red: make parseTop return doc unpruned (the
// top-level row), or skip keepKeys on the block in readMetadata (the block
// row).
func TestReadersCostLikeKeysOutOfReach(t *testing.T) {
	const n, reps = 128 << 10, 2
	head := "sops:\n  mac: m\n  version: 3\n"
	base := []byte(head + "data:\n" + keyLines(n, "  "))
	work := func(doc []byte) func() {
		return func() {
			for range reps {
				_ = isSOPSFile(doc)
				_, _ = readPlaintextRules(doc)
			}
		}
	}
	for _, tc := range []struct{ name, doc string }{
		{"top-level keys", head + keyLines(n, "")},
		{"sops: block keys", "sops:\n  mac: m\n" + keyLines(n, "  ")},
	} {
		baseRun, shapeRun := perftest.Amortize(work(base), work([]byte(tc.doc)))
		perftest.Within(t, "sops readers with "+tc.name, 4, baseRun, shapeRun)
	}
}

// TestSOPSBlockKeysMatchTheStruct keeps sopsBlockKeys, the keys keepKeys
// keeps in the sops: block, in step with sopsBlock's fields: a field added
// without its key would always decode empty, so its rule would go unread.
//
// Mutation that turns it red: drop "mac" from sopsBlockKeys.
func TestSOPSBlockKeysMatchTheStruct(t *testing.T) {
	var tags []string
	typ := reflect.TypeFor[sopsBlock]()
	for i := range typ.NumField() {
		tags = append(tags, typ.Field(i).Tag.Get("yaml"))
	}
	if !reflect.DeepEqual(tags, sopsBlockKeys) {
		t.Fatalf("sopsBlock's yaml tags are %q, sopsBlockKeys is %q", tags, sopsBlockKeys)
	}
}

// TestReadDocumentParsesOnceAndRefusesInOrder pins the driver's single read
// (#1001 Gate 2 nit c): an oversized file is refused for its size; plain
// YAML and unparseable YAML are ErrNotSOPSDocument; a merge key in the top
// level is refused by name; a sops file whose values use `<<: *defaults`
// deep down is read, since only the top level and the sops: block are
// decoded; and the rules match readPlaintextRules'.
//
// Mutations that turn it red: drop the errRefusedShape branch (the merge
// row reads as not a SOPS document); drop the isSOPSDoc check (plain YAML
// returns default rules); skip CheckSize (the size row reads as a parse
// failure).
func TestReadDocumentParsesOnceAndRefusesInOrder(t *testing.T) {
	if _, err := ReadDocument(padTo(t, MaxDocumentBytes+1)); err == nil || !strings.Contains(err.Error(), "larger than 4 MiB") {
		t.Errorf("oversized: %v, want the size limit named", err)
	}
	for _, doc := range []string{"key: value\n", "key: [unclosed\n"} {
		if _, err := ReadDocument([]byte(doc)); !errors.Is(err, ErrNotSOPSDocument) {
			t.Errorf("ReadDocument(%q) = %v, want ErrNotSOPSDocument", doc, err)
		}
	}
	if _, err := ReadDocument([]byte("base: &b {sops: {mac: m}}\n<<: *b\n")); err == nil || !strings.Contains(err.Error(), "merge key") {
		t.Errorf("top-level merge: %v, want a merge-key refusal", err)
	}
	values := "defaults: &defaults\n    replicas: ENC[AES256_GCM,data:x]\napp:\n    web:\n        <<: *defaults\n        token: ENC[AES256_GCM,data:y]\nsops:\n    mac: ENC[x]\n    unencrypted_regex: ^public$\n"
	got, err := ReadDocument([]byte(values))
	if err != nil {
		t.Fatalf("a sops file with merge keys in its values: %v", err)
	}
	want, err := readPlaintextRules([]byte(values))
	if err != nil {
		t.Fatal(err)
	}
	if got.unencryptedRegex.String() != want.unencryptedRegex.String() || got.unencryptedRegex.String() != "^public$" {
		t.Errorf("ReadDocument's rules = %+v, readPlaintextRules' = %+v", got, want)
	}
}
