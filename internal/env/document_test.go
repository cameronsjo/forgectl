package env

// Test plan for document.go
//
// Parse / Bytes (Classification: pure parse/render, byte-verbatim round trip)
//   [x] Happy: round-trip verbatim — comments, blanks, order, export,
//       bare/single/double quoting, inline comment on a quoted value
//   [x] Happy: CRLF line endings preserved
//   [x] Happy: a missing trailing newline is preserved
//   [x] Happy: multiline quoted (PEM) value parses to ONE KindPair; Redacted
//       shows no body line; Get returns the full decoded value; Set
//       re-encodes it to escaped single-line form (documented normalization)
//
// Keys / Get (Classification: pure query)
//   [x] Keys dedupes (first-seen order) and excludes malformed lines
//   [x] Get decodes bare/single/double, last assignment wins, missing key
//       reports ok=false
//
// Set (Classification: pure mutation)
//   [x] In-place Set preserves every other line byte-for-byte
//   [x] Append adds exactly one pair line — no spurious blank line, even
//       when the source had no trailing newline
//   [x] A key appearing more than once is refused, naming every line it
//       appears on — and never echoes the rejected value
//   [x] encode() picks bare vs single- vs double-quote correctly
//   [x] Double-quote escape order is pinned (backslash, then '"', then '$',
//       then newline) — round-trips through a real Parse, not just the
//       encoder in isolation
//
// Redacted (Classification: pure render, value-blind)
//   [x] Constant 4-char mask, no length hint regardless of value length
//   [x] A quoted value's inline trailing comment is kept
//   [x] An unquoted value's trailing "#…" is masked WITH the value (no
//       comment boundary exists for a bare assignment)
//   [x] A malformed line is masked in its ENTIRETY, not just after its
//       first '=' — including a truncated multiline quoted value whose
//       base64 body lines contain '=' padding
//   [x] A missing trailing newline is reproduced
//
// ValidKey (Classification: pure predicate, adversarial input)
//   [x] Accepts ordinary identifiers; rejects shell metacharacters, spaces,
//       backticks, unicode, a leading digit, an embedded '=', and empty
//
// Diff (Classification: pure comparison)
//   [x] Reports missing (in example, not file) and extra (in file, not
//       example) key sets, sorted

import (
	"bytes"
	"strings"
	"testing"
)

func TestDocument_RoundTrip_Verbatim(t *testing.T) {
	src := "# a comment\n" +
		"FOO=bar\n" +
		"\n" +
		"export BAZ=\"qux\"   # trailing comment\n" +
		"QUX='single quoted'\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(doc.Bytes()); got != src {
		t.Errorf("Bytes() = %q, want %q", got, src)
	}

	if v, ok := doc.Get("FOO"); !ok || v != "bar" {
		t.Errorf("Get(FOO) = %q, %v, want bar, true", v, ok)
	}
	if v, ok := doc.Get("BAZ"); !ok || v != "qux" {
		t.Errorf("Get(BAZ) = %q, %v, want qux, true", v, ok)
	}
	if v, ok := doc.Get("QUX"); !ok || v != "single quoted" {
		t.Errorf("Get(QUX) = %q, %v, want %q, true", v, ok, "single quoted")
	}

	baz := doc.Lines[3]
	if !baz.Export || baz.Quote != '"' || baz.Inline != "   # trailing comment" {
		t.Errorf("BAZ line = %+v, want Export=true Quote='\"' Inline=%q", baz, "   # trailing comment")
	}
}

func TestDocument_RoundTrip_CRLF(t *testing.T) {
	src := "FOO=bar\r\n" + "BAZ=qux\r\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !doc.crlf {
		t.Error("crlf = false, want true")
	}
	if got := string(doc.Bytes()); got != src {
		t.Errorf("Bytes() = %q, want %q", got, src)
	}
}

func TestDocument_RoundTrip_NoTrailingNewline(t *testing.T) {
	src := "FOO=bar\nBAZ=qux"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.finalNL {
		t.Error("finalNL = true, want false")
	}
	if got := string(doc.Bytes()); got != src {
		t.Errorf("Bytes() = %q, want %q", got, src)
	}
}

const pemFixture = "-----BEGIN CERTIFICATE-----\n" +
	"MIIBoXsomeBase64ContentHereThatSpansALineOfItsOwn\n" +
	"-----END CERTIFICATE-----"

func TestDocument_MultilinePEM_OnePair(t *testing.T) {
	src := "KEY_ONE=plain\n" +
		"PEM_CERT=\"" + pemFixture + "\"\n" +
		"KEY_TWO=after\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// One KindPair for the whole multiline value — not one Line per source
	// line inside the quotes.
	var pairs int
	var pemLine Line
	for _, l := range doc.Lines {
		if l.Kind == KindPair {
			pairs++
			if l.Key == "PEM_CERT" {
				pemLine = l
			}
		}
	}
	if pairs != 3 {
		t.Fatalf("KindPair count = %d, want 3", pairs)
	}
	if len(pemLine.Raw) != 3 {
		t.Errorf("PEM_CERT.Raw has %d physical lines, want 3", len(pemLine.Raw))
	}
	if pemLine.Value != pemFixture {
		t.Errorf("PEM_CERT.Value = %q, want %q", pemLine.Value, pemFixture)
	}

	if got := string(doc.Bytes()); got != src {
		t.Errorf("Bytes() = %q, want %q", got, src)
	}

	v, ok := doc.Get("PEM_CERT")
	if !ok || v != pemFixture {
		t.Errorf("Get(PEM_CERT) = %q, %v, want full fixture, true", v, ok)
	}
}

func TestDocument_MultilinePEM_RedactedShowsNoBodyLine(t *testing.T) {
	src := "PEM_CERT=\"" + pemFixture + "\"\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	redacted := string(doc.Redacted())
	want := "PEM_CERT=****\n"
	if redacted != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
	assertNoSecretInOutput(t, "MIIBoXsomeBase64ContentHereThatSpansALineOfItsOwn", "", redacted)
	assertNoSecretInOutput(t, "BEGIN CERTIFICATE", "", redacted)
}

func TestDocument_MultilinePEM_SetReencodesEscapedSingleLine(t *testing.T) {
	src := "PEM_CERT=\"" + pemFixture + "\"\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := doc.Set("PEM_CERT", pemFixture); err != nil {
		t.Fatalf("Set: %v", err)
	}

	out := doc.Bytes()
	if bytes.Count(out, []byte("\n")) != 1 {
		t.Errorf("re-encoded output has %d newlines, want 1 (single physical line + trailing newline)", bytes.Count(out, []byte("\n")))
	}

	reparsed, err := Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if v, ok := reparsed.Get("PEM_CERT"); !ok || v != pemFixture {
		t.Errorf("round trip Get(PEM_CERT) = %q, %v, want full fixture, true", v, ok)
	}
}

func TestDocument_Keys_DedupAndExcludesMalformed(t *testing.T) {
	src := "FOO=1\n" +
		"BAR=2\n" +
		"FOO=3\n" +
		"1BAD=x\n" +
		"=alsobad\n" +
		"BAR=4\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := doc.Keys()
	want := []string{"FOO", "BAR"}
	if len(got) != len(want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Keys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDocument_Get_DecodesAndLastWins(t *testing.T) {
	src := "A=bare\n" +
		"B='single'\n" +
		"C=\"double\"\n" +
		"D=1\n" +
		"D=2\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cases := map[string]string{"A": "bare", "B": "single", "C": "double", "D": "2"}
	for k, want := range cases {
		if v, ok := doc.Get(k); !ok || v != want {
			t.Errorf("Get(%q) = %q, %v, want %q, true", k, v, ok, want)
		}
	}
	if _, ok := doc.Get("MISSING"); ok {
		t.Error("Get(MISSING) ok = true, want false")
	}
}

func TestDocument_Set_InPlacePreservesRest(t *testing.T) {
	src := "# header\n" +
		"FOO=1\n" +
		"BAR=old\n" +
		"BAZ=3\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := doc.Set("BAR", "new"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	want := "# header\n" +
		"FOO=1\n" +
		"BAR=new\n" +
		"BAZ=3\n"
	if got := string(doc.Bytes()); got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
}

func TestDocument_Set_AppendNoSpuriousBlank_WithTrailingNewline(t *testing.T) {
	src := "FOO=1\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := doc.Set("BAR", "2"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	want := "FOO=1\nBAR=2\n"
	if got := string(doc.Bytes()); got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
}

func TestDocument_Set_AppendNoSpuriousBlank_NoTrailingNewline(t *testing.T) {
	src := "FOO=1"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := doc.Set("BAR", "2"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	want := "FOO=1\nBAR=2\n"
	if got := string(doc.Bytes()); got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
}

func TestDocument_Set_DuplicateKeyRefusedNamingLines(t *testing.T) {
	src := "# header\n" +
		"FOO=1\n" +
		"BAR=2\n" +
		"FOO=3\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	const sentinel = "s3ntinel-VALUE-77x"
	err = doc.Set("FOO", sentinel)
	if err == nil {
		t.Fatal("Set(FOO) on a duplicated key returned nil error, want a refusal")
	}
	msg := err.Error()
	for _, want := range []string{`"FOO"`, "2", "4"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	assertNoSecretInOutput(t, sentinel, "", msg)

	// Refused — the document must be untouched.
	if got := string(doc.Bytes()); got != src {
		t.Errorf("Bytes() after refused Set = %q, want unchanged %q", got, src)
	}
}

func TestEncodeValue_BareSingleDouble(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"bare", "simple-value_1.2:3+4@5%6", "K=simple-value_1.2:3+4@5%6"},
		{"single (space, no apostrophe)", "has space", "K='has space'"},
		{"double (contains apostrophe)", "can't", `K="can't"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := encode(false, "K", c.value); got != c.want {
				t.Errorf("encode() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestEncodeValue_BackslashQuoteDollarNewlineOrder(t *testing.T) {
	// Forces the double-quote path (apostrophe present) while also
	// exercising every escaped character the pinned order covers.
	value := "it's \"ok\" C:\\path\nnext $VAR"

	got := encode(false, "K", value)

	// Independently mirror the pinned order — backslash, then '"', then
	// '$', then newline — rather than hand-transcribing an escaped string
	// literal (error-prone and would just duplicate the implementation).
	want := value
	want = strings.ReplaceAll(want, `\`, `\\`)
	want = strings.ReplaceAll(want, `"`, `\"`)
	want = strings.ReplaceAll(want, `$`, `\$`)
	want = strings.ReplaceAll(want, "\n", `\n`)
	want = `K="` + want + `"`

	if got != want {
		t.Errorf("encode() = %q, want %q", got, want)
	}

	// The real proof the order is correct: it round-trips through an
	// actual Parse, not just self-consistent string math.
	doc := &Document{}
	if err := doc.Set("K", value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	reparsed, err := Parse(bytes.NewReader(doc.Bytes()))
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if gotValue, ok := reparsed.Get("K"); !ok || gotValue != value {
		t.Errorf("round trip Get(K) = %q, %v, want %q, true", gotValue, ok, value)
	}
}

func TestDocument_Redacted_ConstantMaskNoLengthHint(t *testing.T) {
	const sentinel = "s3ntinel-VALUE-77x-and-then-a-lot-more-characters-after-it-too"
	doc := &Document{}
	if err := doc.Set("KEY", sentinel); err != nil {
		t.Fatalf("Set: %v", err)
	}

	redacted := string(doc.Redacted())
	want := "KEY=****\n"
	if redacted != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
	assertNoSecretInOutput(t, sentinel, "", redacted)
}

func TestDocument_Redacted_QuotedInlineCommentKept(t *testing.T) {
	const sentinel = "s3ntinel-VALUE-77x"
	src := "export BAZ=\"" + sentinel + "\"   # trailing comment\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	redacted := string(doc.Redacted())
	want := "export BAZ=****   # trailing comment\n"
	if redacted != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
	assertNoSecretInOutput(t, sentinel, "", redacted)
}

func TestDocument_Redacted_UnquotedTrailerMasked(t *testing.T) {
	const sentinel = "s3ntinel-VALUE-77x"
	src := "KEY=" + sentinel + " # not-a-comment-boundary\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	redacted := string(doc.Redacted())
	want := "KEY=****\n"
	if redacted != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
	assertNoSecretInOutput(t, sentinel, "", redacted)
	assertNoSecretInOutput(t, "not-a-comment-boundary", "", redacted)
}

func TestDocument_Redacted_MalformedMasked(t *testing.T) {
	const sentinel = "s3ntinel-VALUE-77x"
	src := "1BAD=" + sentinel + "\n" +
		"not an assignment at all\n"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	redacted := string(doc.Redacted())
	// Whole-line masking (security hardening): the malformed key "1BAD" is
	// no longer preserved before the '=' either — there's nothing about a
	// line this package couldn't parse as an assignment that's safe to
	// treat as "the key portion".
	want := "****\n" +
		"****\n"
	if redacted != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
	assertNoSecretInOutput(t, sentinel, "", redacted)
	assertNoSecretInOutput(t, "1BAD", "", redacted)
}

func TestRedacted_TruncatedPEM_MasksAllBodyLines(t *testing.T) {
	// An unterminated double-quoted multiline value: the opening line's
	// quote never closes before EOF, so Parse returns KindMalformed for the
	// OPENING line (lenient — see findClosingQuote), and every base64-shaped
	// body line then reparses independently as its own KindMalformed line.
	// Real base64 padding ('=') and non-key characters ('/') in those body
	// lines are exactly the shape that leaked under the old "preserve
	// everything before the first '='" behavior. The header label below is
	// deliberately non-standard ("TESTHDR" rather than "PRIVATE KEY") so
	// this fixture doesn't itself read as a live PEM to secret-scanning
	// tooling — the parser's handling is identical either way, since it
	// never inspects the label text.
	const (
		header = `KEY="-----BEGIN TESTHDR-----`
		body1  = "TUlJRXZRSUJBREFOQmdrcWhraUc5dzBCQVFFRkFBU0NCS2N3Z2dTakFnRUFBb0lC"
	)
	// body2 deliberately contains a '/' (real base64's non-key alphabet)
	// before its trailing "==" padding — the concatenation just makes the
	// '/' visually unambiguous in source; ValidKey must reject the '/'-
	// bearing prefix so this line lands on the malformed path too.
	body2 := "QVFDN1ZKVFV0OVVzOGNLQi" + "/" + "jM1Z5WldsNUxYUm9hWE10YVhNdGJtOTBMV0V0Y21W=="
	src := header + "\n" + body1 + "\n" + body2

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, l := range doc.Lines {
		if l.Kind != KindMalformed {
			t.Fatalf("line %q has Kind %v, want KindMalformed (fixture must exercise the malformed path)", l.Raw, l.Kind)
		}
	}

	redacted := string(doc.Redacted())
	want := "****\n****\n****"
	if redacted != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
	for _, leak := range []string{"TESTHDR", "KEY=", body1, body2} {
		assertNoSecretInOutput(t, leak, "", redacted)
	}
}

func TestDocument_Redacted_NoTrailingNewlineReproduced(t *testing.T) {
	src := "FOO=bar\nBAZ=qux"

	doc, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	redacted := doc.Redacted()
	if len(redacted) == 0 || redacted[len(redacted)-1] == '\n' {
		t.Errorf("Redacted() = %q, want no trailing newline", redacted)
	}
	want := "FOO=****\nBAZ=****"
	if string(redacted) != want {
		t.Errorf("Redacted() = %q, want %q", redacted, want)
	}
}

func TestValidKey_Adversarial(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"FOO", true},
		{"_FOO", true},
		{"FOO_1", true},
		{"FOO;rm -rf /", false},
		{"FOO BAR", false},
		{"FOO$BAR", false},
		{"FOO`BAR", false},
		{"FÖO", false},
		{"1FOO", false},
		{"FOO=BAR", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ValidKey(c.key); got != c.want {
			t.Errorf("ValidKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestDiff_MissingAndExtra(t *testing.T) {
	file, err := Parse(strings.NewReader("A=1\nB=2\nC=3\n"))
	if err != nil {
		t.Fatalf("Parse(file): %v", err)
	}
	example, err := Parse(strings.NewReader("B=\nC=\nD=\n"))
	if err != nil {
		t.Fatalf("Parse(example): %v", err)
	}

	missing, extra := Diff(file, example)
	if len(missing) != 1 || missing[0] != "D" {
		t.Errorf("missing = %v, want [D]", missing)
	}
	if len(extra) != 1 || extra[0] != "A" {
		t.Errorf("extra = %v, want [A]", extra)
	}
}
