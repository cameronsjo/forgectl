package termsafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// No unsafe character appears in this file, in any spelling. Both the raw rune
// and its expected JSON escape are BUILT from a code point number, because a
// source file carrying a raw RIGHT-TO-LEFT OVERRIDE reorders its own diff,
// review, and editor rendering — the exact hazard this package exists to
// prevent — and even the escape spelling is easy to turn into the literal
// character by accident on the way into the file. Deriving both from one
// integer removes the chance entirely and keeps the two in step.
const (
	rlo  = 0x202E // RIGHT-TO-LEFT OVERRIDE — the issue's named character
	lri  = 0x2066 // LEFT-TO-RIGHT ISOLATE — three-byte, different lead pair
	alm  = 0x061C // ARABIC LETTER MARK — the only two-byte bidi member
	csi  = 0x009B // C1 CONTROL SEQUENCE INTRODUCER — cursor ops, one byte pair
	osc  = 0x009D // C1 OPERATING SYSTEM COMMAND — window title, clipboard
	del  = 0x007F // DELETE — Cc, and above encoding/json's 0x20 escape cutoff
	bel  = 0x0007 // BELL — C0, which encoding/json escapes before we see it
	emDa = 0x2014 // non-bidi, and shares RLO's E2-80 lead pair exactly, so
	//               the carry must hold it and then release it unescaped
	zwsp = 0x200B // ZERO WIDTH SPACE — invisible, but neither Cc nor bidi
)

// Safe multi-byte runes, chosen by encoding length and lead byte rather than by
// appearance: each is a width the split-write carry must reassemble, and none
// may ever be escaped or repaired.
const (
	eAcute = 0x00E9  // 2-byte, C3 A9 — lead byte no escapable rune shares
	euro   = 0x20AC  // 3-byte, E2 82 AC — shares only RLO's first byte
	enDash = 0x2013  // 3-byte, E2 80 93 — shares RLO's first TWO bytes
	grin   = 0x1F600 // 4-byte, F0 9F 98 80 — widest sequence UTF-8 allows
)

// raw renders a code point as the literal UTF-8 character.
func raw(cp rune) string { return string(cp) }

// escaped renders a code point as the JSON escape the filter must emit.
func escaped(cp rune) string { return fmt.Sprintf(`\u%04x`, cp) }

// escapableRunes enumerates every rune the filter escapes, by sweeping the rune
// space through the predicate. It lives in the test rather than in the package
// because that is the only place the exhaustive form is worth its cost: the
// filter itself applies the predicate directly, so nothing at run time needs
// the list, and paying a full-rune-space sweep at init would tax every forgectl
// invocation for a guarantee the tests already provide.
func escapableRunes() []rune {
	var runes []rune
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogate halves are not characters
		}
		if needsJSONEscape(r) {
			runes = append(runes, r)
		}
	}
	return runes
}

// TestJSONEncoder_EscapesEveryEscapableRune walks every rune the shared predicate
// accepts rather than a chosen sample, so a character added to Cc or
// Bidi_Control by a future Go release cannot slip through untested. Both halves
// of the contract are asserted per rune: nothing literal survives, and the
// document still decodes to the exact input.
func TestJSONEncoder_EscapesEveryEscapableRune(t *testing.T) {
	runes := escapableRunes()
	if len(runes) == 0 {
		t.Fatal("escapableRunes enumerated nothing; the whole table walk is vacuous")
	}
	for _, r := range runes {
		value := "head" + raw(r) + "tail"

		var buf bytes.Buffer
		if err := JSONEncoder(&buf).Encode(value); err != nil {
			t.Fatalf("encode U+%04X: %v", r, err)
		}
		out := buf.String()

		if strings.ContainsRune(out, r) {
			t.Errorf("U+%04X survived literally in %q", r, out)
		}
		var decoded string
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatalf("decode U+%04X: %v (%q)", r, err, out)
		}
		if decoded != value {
			t.Errorf("U+%04X round trip = %q, want %q", r, decoded, value)
		}
	}
}

// TestMayContainUnsafe_NeverMissesAnEscapableRune is the anti-drift check on
// the prefilter, swept over the whole rune space rather than a sample. A fast
// path answering "nothing here" for a rune the escape loop would rewrite is a
// silent bypass of the entire control, so the sweep is the assertion the
// package deliberately does not pay for at init.
func TestMayContainUnsafe_NeverMissesAnEscapableRune(t *testing.T) {
	var checked int
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if !needsJSONEscape(r) {
			continue
		}
		checked++
		if !mayContainUnsafe([]byte(string(r))) {
			t.Fatalf("prefilter rejected U+%04X, which the escape loop rewrites", r)
		}
	}
	if checked == 0 {
		t.Fatal("swept the rune space and found nothing escapable; the predicate is broken, not the filter clean")
	}
}

// TestEncoderEscapesEverythingBelowTheFloor is the other half of that
// agreement, and the one that keeps the sub-floor delegation honest. This
// filter deliberately ignores runes below jsonEscapeFloor because tab, newline
// and carriage return are Cc *and* JSON's structural whitespace — escaping them
// would rewrite SetIndent's layout and the document terminator. That is only
// safe while encoding/json escapes every one of them inside a string, so assert
// it rather than trusting it: silent delegation to the encoder's narrower rule
// is precisely how the C1 gap opened.
func TestEncoderEscapesEverythingBelowTheFloor(t *testing.T) {
	var checked int
	for r := rune(0); r < jsonEscapeFloor; r++ {
		if !IsUnsafeTerminalRune(r) {
			continue
		}
		checked++

		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode("head" + string(r) + "tail"); err != nil {
			t.Fatalf("encode U+%04X: %v", r, err)
		}
		// The encoder's own trailing newline is structure, not content.
		body := strings.TrimRight(buf.String(), "\n")
		if strings.ContainsRune(body, r) {
			t.Errorf("encoding/json left U+%04X raw in %q — the sub-floor delegation is no longer safe", r, body)
		}
	}
	if checked == 0 {
		t.Fatal("no sub-floor unsafe runes were checked; the delegation is unasserted")
	}
}

// TestJSONEncoder_EscapesC1AndDelete is the #279 review regression. encoding/json
// escapes bytes below 0x20 and nothing else above them, so DEL and the C1
// controls reached a terminal literally even after bidi was handled — and C1
// CSI and OSC drive the same cursor and window-title operations as their C0
// forms. BELL is asserted alongside them to pin the division of labour: the
// encoder escapes it before this filter ever sees it, and the result is the
// same escape either way.
func TestJSONEncoder_EscapesC1AndDelete(t *testing.T) {
	value := "opus" + raw(osc) + "0;title" + raw(csi) + "31m" + raw(del) + raw(bel) + "-tail"

	var buf bytes.Buffer
	if err := JSONEncoder(&buf).Encode(value); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := buf.String()

	for _, r := range []rune{osc, csi, del, bel} {
		if strings.ContainsRune(out, r) {
			t.Errorf("U+%04X survived literally: %q", r, out)
		}
		if !strings.Contains(out, escaped(r)) {
			t.Errorf("U+%04X is missing its escape %s: %q", r, escaped(r), out)
		}
	}

	var decoded string
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v (%q)", err, out)
	}
	if decoded != value {
		t.Errorf("round trip = %q, want %q", decoded, value)
	}
}

// TestJSONEncoder_EscapeSpelling pins the exact wire form for the character the
// issue names, so a future refactor cannot satisfy the "no literal rune" half
// by deleting or substituting it.
func TestJSONEncoder_EscapeSpelling(t *testing.T) {
	var buf bytes.Buffer
	if err := JSONEncoder(&buf).Encode("opus" + raw(rlo) + "-tail"); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := strings.TrimRight(buf.String(), "\n")
	want := `"opus` + escaped(rlo) + `-tail"`
	if got != want {
		t.Errorf("encoded = %s, want %s", got, want)
	}
}

// TestJSONEncoder_LeavesOrdinaryDocumentsByteIdentical pins the filter as
// value-preserving rather than merely round-trip-safe: a document with nothing
// to escape must come out exactly as encoding/json wrote it, so adopting the
// encoder cannot churn any existing --json contract. ZERO WIDTH SPACE is in the
// fixture on purpose — it is invisible but neither Cc nor bidi, and rewriting
// it would mean the escape set had quietly widened past IsUnsafeTerminalRune.
func TestJSONEncoder_LeavesOrdinaryDocumentsByteIdentical(t *testing.T) {
	value := map[string]any{
		"path":      "/tmp/forgectl/config.toml",
		"model":     "opus",
		"unicode":   "café — naïve — " + raw(emDa),
		"invisible": raw(zwsp),
		"html":      "<a>&</a>",
		"count":     7,
	}

	var filtered, reference bytes.Buffer
	enc := JSONEncoder(&filtered)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		t.Fatalf("filtered encode: %v", err)
	}
	ref := json.NewEncoder(&reference)
	ref.SetIndent("", "  ")
	if err := ref.Encode(value); err != nil {
		t.Fatalf("reference encode: %v", err)
	}

	if filtered.String() != reference.String() {
		t.Errorf("filter rewrote a document with nothing to escape:\n got %q\nwant %q", filtered.String(), reference.String())
	}
}

// TestJSONEncoder_HonorsEncoderOptions proves the wrapper is a plain io.Writer
// filter and not a re-implementation: SetEscapeHTML must still reach
// encoding/json, because launch_stats deliberately turns it off to keep an
// operator's model string byte-exact. Escaping must survive that setting.
func TestJSONEncoder_HonorsEncoderOptions(t *testing.T) {
	var buf bytes.Buffer
	enc := JSONEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode("<model>&" + raw(rlo)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "<model>&") {
		t.Errorf("SetEscapeHTML(false) did not reach the encoder: %q", out)
	}
	if !strings.Contains(out, escaped(rlo)) {
		t.Errorf("escaping was skipped: %q", out)
	}
}

// TestJSONEncoder_RejectsOverlongEncodings closes the one route by which bytes
// reach the encoder without its UTF-8 substitution: json.RawMessage is checked
// for JSON syntax, not for UTF-8 validity. An overlong encoding of an unsafe
// character is several meaningless bytes to a rune-level predicate and one
// character to a lenient decoder, so the filter replaces invalid sequences with
// U+FFFD instead of copying them through.
func TestJSONEncoder_RejectsOverlongEncodings(t *testing.T) {
	// A four-byte overlong spelling of RLO, valid to no correct decoder.
	overlong := []byte{0xF0, 0x82, 0x80, 0xAE}
	payload := append(append([]byte{'"'}, overlong...), '"')

	var buf bytes.Buffer
	if err := JSONEncoder(&buf).Encode(json.RawMessage(payload)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := buf.Bytes()

	if bytes.Contains(out, overlong) {
		t.Errorf("overlong encoding of U+%04X passed through: % X", rlo, out)
	}
	if !utf8.Valid(out) {
		t.Errorf("filter emitted invalid UTF-8: % X", out)
	}
}

// TestEscapingWriter_ByteAtATimeWrites is the load-bearing test for the carry.
// json.Encoder writes a whole document per call today, so a character split
// across two Writes never arises in production — which is exactly why it must
// be tested rather than assumed. One byte per Write is the harshest split
// available: every multi-byte character arrives fragmented, and so does the
// safe multi-byte character that must NOT be held back.
func TestEscapingWriter_ByteAtATimeWrites(t *testing.T) {
	source := fmt.Sprintf(`{"a":"x%sy","b":"%s%s","c":"%s%s","d":"%s%s%s"}`,
		raw(rlo), raw(emDa), raw(lri), raw(alm), raw(csi),
		raw(eAcute), raw(euro), raw(grin))
	want := fmt.Sprintf(`{"a":"x%sy","b":"%s%s","c":"%s%s","d":"%s%s%s"}`,
		escaped(rlo), raw(emDa), escaped(lri), escaped(alm), escaped(csi),
		raw(eAcute), raw(euro), raw(grin))

	var whole bytes.Buffer
	if _, err := (&escapingWriter{out: &whole}).Write([]byte(source)); err != nil {
		t.Fatalf("whole write: %v", err)
	}
	if whole.String() != want {
		t.Errorf("single write = %q, want %q", whole.String(), want)
	}

	var split bytes.Buffer
	w := &escapingWriter{out: &split}
	for i := 0; i < len(source); i++ {
		n, err := w.Write([]byte{source[i]})
		if err != nil {
			t.Fatalf("write byte %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("write byte %d reported n=%d, want 1", i, n)
		}
	}
	if len(w.held) != 0 {
		t.Errorf("writer still holding % X after a complete document", w.held)
	}
	if split.String() != want {
		t.Errorf("byte-at-a-time = %q, want %q", split.String(), want)
	}
}

// TestEscapingWriter_EverySplitPoint is the test the byte-at-a-time one only
// looked like. Its fixture used to carry a single safe multi-byte rune —
// em-dash, whose first two bytes the old carry happened to hold anyway — so a
// carry that dropped every OTHER safe rune still passed it. That is exactly the
// bug the U+FFFD repair introduced and this test would have caught: split
// across two Writes, café became two replacement characters, because the
// leading bytes were repaired on one Write and the trailing bytes on the next.
//
// Cutting at every offset, with runes of each UTF-8 width and lead byte, is the
// assertion that cannot be satisfied by luck.
func TestEscapingWriter_EverySplitPoint(t *testing.T) {
	source := fmt.Sprintf(`{"safe":"%s %s %s %s","unsafe":"%s%s"}`,
		raw(eAcute), raw(euro), raw(enDash), raw(grin), raw(rlo), raw(csi))

	var whole bytes.Buffer
	if _, err := (&escapingWriter{out: &whole}).Write([]byte(source)); err != nil {
		t.Fatalf("whole write: %v", err)
	}
	reference := whole.String()

	for cut := 0; cut <= len(source); cut++ {
		var got bytes.Buffer
		w := &escapingWriter{out: &got}
		if _, err := w.Write([]byte(source[:cut])); err != nil {
			t.Fatalf("cut %d, first write: %v", cut, err)
		}
		if _, err := w.Write([]byte(source[cut:])); err != nil {
			t.Fatalf("cut %d, second write: %v", cut, err)
		}
		if len(w.held) != 0 {
			t.Errorf("cut %d: writer still holding % X after a complete document", cut, w.held)
		}
		if got.String() != reference {
			t.Errorf("cut %d corrupted the document:\n got %q\nwant %q", cut, got.String(), reference)
		}
	}
}

// TestIncompleteSuffixLen_HoldsEveryPartialCharacter guards the carry from both
// failure directions: holding bytes that form a complete character stalls
// output that was ready, and releasing a partial one destroys it.
//
// It sweeps EVERY rune the filter can meet — escapable and safe alike — because
// the predecessor asserted only over escapable prefixes, and "safe rune, split
// in half" was precisely the case that had no coverage and no correct behavior.
func TestIncompleteSuffixLen_HoldsEveryPartialCharacter(t *testing.T) {
	subjects := append(escapableRunes(), eAcute, euro, enDash, grin, emDa, zwsp, 'a')
	for _, r := range subjects {
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], r)
		for k := 1; k < n; k++ {
			if got := incompleteSuffixLen(append([]byte("lead"), buf[:k]...)); got != k {
				t.Errorf("incompleteSuffixLen(%d of %d bytes of U+%04X) = %d, want %d", k, n, r, got, k)
			}
		}
		if got := incompleteSuffixLen(append([]byte("lead"), buf[:n]...)); got != 0 {
			t.Errorf("incompleteSuffixLen(complete U+%04X) = %d, want 0", r, got)
		}
	}

	for _, s := range []string{"", "plain ascii", `{"k":"v"}`, raw(emDa), "café", raw(grin)} {
		if got := incompleteSuffixLen([]byte(s)); got != 0 {
			t.Errorf("incompleteSuffixLen(%q) = %d, want 0", s, got)
		}
	}
}

// TestMayContainUnsafe_AlsoCatchesInvalidUTF8 covers the prefilter's second
// job. TestMayContainUnsafe_NeverMissesAnEscapableRune covers the first; this
// one covers repair, which is the half a lead-byte table could not do — and the
// reason the prefilter is a byte-range test rather than a table at all.
func TestMayContainUnsafe_AlsoCatchesInvalidUTF8(t *testing.T) {
	// Invalid bytes have no lead byte to enumerate, so each of these is a case
	// a table-derived prefilter would have handed straight to the output.
	for name, b := range map[string][]byte{
		"overlong RLO":      {0xF0, 0x82, 0x80, 0xAE},
		"truncated 3-byte":  {0xE2, 0x80},
		"lone continuation": {0xAE},
	} {
		if !mayContainUnsafe(b) {
			t.Errorf("prefilter rejected %s (% X), which the rune scan repairs", name, b)
		}
	}
	if mayContainUnsafe([]byte(`{"model":"opus","path":"/tmp/x"}`)) {
		t.Error("prefilter claimed a document with nothing to escape may contain an unsafe rune")
	}
}
