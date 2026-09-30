package tomlerr

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// marker is planted in every VALUE the adversarial corpus decodes. Keys are
// the operator's own text and echo capped; values never echo at all.
const marker = "SECRETMARK"

// fixture is a decode target with one field of each shape a type mismatch
// can name, plus a user-keyed map (the #738 case: the key is arbitrary text).
type fixture struct {
	S   string            `toml:"s"`
	N   int               `toml:"n"`
	F   float64           `toml:"f"`
	B   bool              `toml:"b"`
	A   []string          `toml:"a"`
	Arr [2]int            `toml:"arr"`
	M   map[string]string `toml:"m"`
	T   struct {
		X int `toml:"x"`
	} `toml:"t"`
	D time.Time `toml:"d"`
}

// hints is every text Scrub may end a message with: the tomlHints allowlist,
// the generic fallback, and the decoder-error vocabulary.
func hints() map[string]bool {
	out := map[string]bool{"syntax error": true, "decode error": true, "wrong array length": true}
	for _, h := range tomlHints {
		out[h.hint] = true
	}
	kinds := []string{"string", "integer", "float", "boolean", "datetime", "array", "table"}
	out["wrong value type"] = true
	for _, a := range kinds {
		out["wrong value type: found "+a] = true
		out["wrong value type: want "+a] = true
		for _, b := range kinds {
			out["wrong value type: found "+a+", want "+b] = true
		}
	}
	return out
}

// grammar is the whole shape a scrubbed message may take. The key is a Go
// quoted string (QuoteArgMax), optionally followed by the cap's ellipsis.
var grammar = regexp.MustCompile(`^toml:(?: line [0-9]{1,9}(?:, column [0-9]{1,9})?)?(?: \(last key "(?:[^"\\]|\\.)*"…?\))?: (.+)$`)

func decode(t *testing.T, body string) error {
	t.Helper()
	var f fixture
	_, err := toml.Decode(body, &f)
	if err == nil {
		t.Fatalf("decoder accepted %q", body)
	}
	return err
}

// TestScrub_AdversarialCorpus drives the real decoder through parse errors
// and type mismatches with a marker in every value, and holds every result
// to the grammar and the hint allowlist: nothing from a value, and nothing
// outside fixed text, a position and a capped key.
func TestScrub_AdversarialCorpus(t *testing.T) {
	corpus := []string{
		"s = " + marker + "\n",
		"s = \"" + marker + "\n",
		"s = \"\\x" + marker + "\"\n",
		"n = 9" + strings.Repeat("9", 40) + marker + "\n",
		"n = 98765432109876543210987\n",
		"f = 1e99999\n",
		"d = 1979-05-27T" + marker + "\n",
		"a = [\"" + marker + "\" \"x\"]\n",
		"m = { k = \"" + marker + "\" k2 }\n",
		"[" + marker + "\n",
		marker + "\n",
		"s = 1\ns = 2\n",
		"s = 12" + marker + "\n",
		"s = '''" + marker + "''''''\n",
		// Type mismatches: the decoder's own text carries the key path.
		"s = 98765\n",
		"n = \"" + marker + "\"\n",
		"b = \"" + marker + "\"\n",
		"f = \"" + marker + "\"\n",
		"a = 5\n",
		"arr = [1, 2, 3]\n",
		"t = 5\n",
		"s = 1979-05-27\n",
		"n = { x = \"" + marker + "\" }\n",
		"[m]\n" + strings.Repeat("K", 400) + " = 98765\n",
		"[m]\n\"k\\u001b[2J\\u202e): toml: line 1 (last key \\\"ghp\\\"): x\" = 5\n",
		"[m]\n\"q\\\"\\\\\" = 5\n",
	}
	allowed := hints()
	for _, body := range corpus {
		raw := decode(t, body)
		got := Scrub(raw)
		msg := got.Error()
		m := grammar.FindStringSubmatch(msg)
		if m == nil {
			t.Errorf("Scrub(%q) = %q, outside the grammar", body, msg)
			continue
		}
		if !allowed[m[1]] {
			t.Errorf("Scrub(%q) hint %q is not allowlisted", body, m[1])
		}
		for _, leak := range []string{marker, "98765", "999999", "\x1b", "\u202e"} {
			if strings.Contains(msg, leak) {
				t.Errorf("Scrub(%q) = %q carries %q", body, msg, leak)
			}
		}
		if strings.Contains(msg, strings.Repeat("K", 81)) {
			t.Errorf("Scrub(%q) = %q, key not capped", body, msg)
		}
		if !errors.Is(got, raw) {
			t.Errorf("Scrub(%q) dropped the original error from the chain", body)
		}
	}
}

// TestScrub_TypeMismatchTemplate pins the rebuilt text for each decoder
// message family, over the real decoder.
func TestScrub_TypeMismatchTemplate(t *testing.T) {
	cases := []struct{ body, want string }{
		{"s = 98765\n", `toml: line 1 (last key "s"): wrong value type: found integer, want string`},
		{"n = \"x\"\n", `toml: line 1 (last key "n"): wrong value type: found string, want integer`},
		{"a = 5\n", `toml: line 1 (last key "a"): wrong value type: found integer, want array`},
		{"t = 5\n", `toml: line 1 (last key "t"): wrong value type: found integer, want table`},
		{"s = 1979-05-27\n", `toml: line 1 (last key "s"): wrong value type: found datetime, want string`},
		{"arr = [1, 2, 3]\n", `toml: line 1 (last key "arr"): wrong array length`},
		{"\n\n[m]\n\"a): b\" = 5\n", `toml: line 4 (last key "m.\"a): b\""): wrong value type: found integer, want string`},
	}
	for _, c := range cases {
		if got := Scrub(decode(t, c.body)).Error(); got != c.want {
			t.Errorf("Scrub(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

// TestScrub_KeyCappedInTypeMismatch is #738 item 4 exactly: a 400-rune key
// in a user-keyed map, which the decoder quotes in full.
func TestScrub_KeyCappedInTypeMismatch(t *testing.T) {
	raw := decode(t, "[m]\n"+strings.Repeat("K", 400)+" = 98765\n")
	if !strings.Contains(raw.Error(), strings.Repeat("K", 400)) {
		t.Fatalf("premise broken: the decoder no longer echoes the full key: %v", raw)
	}
	msg := Scrub(raw).Error()
	want := `toml: line 2 (last key "m.` + strings.Repeat("K", 78) + `"…): wrong value type: found integer, want string`
	if msg != want {
		t.Errorf("Scrub = %q, want %q", msg, want)
	}
}

// TestScrub_UnknownDecoderMessageIsGeneric: a decoder message family the
// template does not know reads "decode error", never its text.
func TestScrub_UnknownDecoderMessageIsGeneric(t *testing.T) {
	for _, in := range []string{
		`toml: line 3 (last key "x"): some future message quoting "` + marker + `"`,
		`toml: cannot decode to non-pointer ` + marker,
		`toml: line 3 (last key "unterminated): ` + marker,
		`toml: line 99999999999999 (last key "x"): ` + marker,
	} {
		got := Scrub(errors.New(in)).Error()
		if strings.Contains(got, marker) || !strings.HasSuffix(got, ": decode error") {
			t.Errorf("Scrub(%q) = %q, want the generic hint and no marker", in, got)
		}
	}
	if got := tomlHintFor("some future message quoting \"ghp_SECRET\" and 98765432"); got != "syntax error" {
		t.Errorf("tomlHintFor(unknown) = %q, want the generic hint", got)
	}
}

// TestScrub_PassesThroughOtherErrors: an error that is not the decoder's (an
// os error from DecodeFile) and nil pass through unchanged.
func TestScrub_PassesThroughOtherErrors(t *testing.T) {
	var f fixture
	_, err := toml.DecodeFile(filepath.Join(t.TempDir(), "absent.toml"), &f)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("premise: DecodeFile on a missing file = %v", err)
	}
	if got := Scrub(err); got != err { //nolint:errorlint // identity is the assertion
		t.Errorf("Scrub rewrote a non-decoder error: %v", got)
	}
	if Scrub(nil) != nil {
		t.Error("Scrub(nil) != nil")
	}
}

// TestKeys_QuotedCappedAndBounded: an unknown-key list is quoted per key,
// capped per key, and cut after MaxKeysShown keys.
func TestKeys_QuotedCappedAndBounded(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 20; i++ {
		body.WriteString(`"` + string(rune('a'+i)) + `\u001b` + strings.Repeat("K", 200) + "\" = 1\n")
	}
	var empty struct{}
	md, err := toml.Decode(body.String(), &empty)
	if err != nil {
		t.Fatal(err)
	}
	got := Keys(md.Undecoded())
	if strings.Contains(got, "\x1b") || strings.Contains(got, strings.Repeat("K", 81)) {
		t.Errorf("Keys = %q, echoes a key uncapped or unescaped", got)
	}
	if n := strings.Count(got, `"…`); n != MaxKeysShown {
		t.Errorf("Keys shows %d keys, want %d: %q", n, MaxKeysShown, got)
	}
	if !strings.HasSuffix(got, ", …") {
		t.Errorf("Keys = %q, want a trailing ellipsis", got)
	}
	if got := Keys([]toml.Key{{"a", "b"}}); got != `"a.b"` {
		t.Errorf("Keys(one) = %q", got)
	}
}

// TestKeyStrings_MatchesKeys pins #761: the string form renders exactly as
// Keys does, so config's unrecognized-key list reads like every other
// unknown-key error.
func TestKeyStrings_MatchesKeys(t *testing.T) {
	long := strings.Repeat("K", 200)
	var keys []toml.Key
	var dotted []string
	for i := 0; i < 7; i++ {
		k := toml.Key{"s", string(rune('a'+i)) + long}
		keys = append(keys, k)
		dotted = append(dotted, k.String())
	}
	if got, want := KeyStrings(dotted), Keys(keys); got != want {
		t.Errorf("KeyStrings = %q, want Keys' %q", got, want)
	}
	if got := KeyStrings(dotted); strings.Contains(got, long[:81]) || !strings.HasSuffix(got, ", …") {
		t.Errorf("KeyStrings = %q, want capped keys and a trailing ellipsis", got)
	}
	if got := KeyStrings(dotted[:MaxKeysShown]); strings.HasSuffix(got, ", …") {
		t.Errorf("KeyStrings(%d) = %q, want no ellipsis at exactly the cap", MaxKeysShown, got)
	}
}
