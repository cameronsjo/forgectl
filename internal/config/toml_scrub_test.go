package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// secretish is the leading bare word BurntSushi/toml quotes back when an
// unquoted value fails to lex: `found "ghp"` for a GitHub token, up to twelve
// characters for an AWS-style key (#687).
const secretish = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // G101: AWS's documented example key, a fixture the test asserts never echoes

// digitRun finds five or more digits in a row: an out-of-range number is
// printed bare in toml's own message, and line/column numbers stay short.
var digitRun = regexp.MustCompile(`[0-9]{5,}`)

// TestScrubTOMLError_AllowlistOverRealDecoder drives the real decoder through
// each message family and pins the fixed hint it renders. The rendering is an
// allowlist, so no part of toml's message — a quoted bare word, an
// out-of-range number printed in full — can reach it (#687).
func TestScrubTOMLError_AllowlistOverRealDecoder(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unquoted token", "token = ghp_SECRETabcdefghij\n", `toml: line 1, column 9 (last key "token"): expected a value`},
		{"unquoted aws key", "token = " + secretish + "\n", `toml: line 1, column 9 (last key "token"): expected a value`},
		{"int out of range", "log_level = 98765432109876543210987\n", `toml: line 1, column 13 (last key "log_level"): number out of range`},
		{"5000-digit int", "x = " + strings.Repeat("7", 5000) + "\n", `toml: line 1, column 5 (last key "x"): number out of range`},
		{"float out of range", "x = 1e99999\n", `toml: line 1, column 5 (last key "x"): number out of range`},
		{"unterminated string", "x = \"abc\n", `toml: line 1, column 9 (last key "x"): unterminated string`},
		{"invalid escape", "x = \"\\q\"\n", `toml: line 1, column 6 (last key "x"): invalid escape`},
		{"hex escape", "x = \"\\xZZsecret\"\n", `toml: line 1, column 6 (last key "x"): invalid escape`},
		{"table header", "[ghp_SECRET\n", `toml: line 2, column 12: invalid table header`},
		{"duplicate key", "a = 1\na = 2\n", `toml: line 2, column 7 (last key "a"): duplicate or conflicting key`},
		{"datetime", "x = 1979-05-27T12345ghp\n", `toml: line 1, column 5 (last key "x"): invalid datetime`},
		{"hex number", "x = 0xZZ\n", `toml: line 1, column 5 (last key "x"): invalid number`},
		{"leading zeroes", "x = 0123456789\n", `toml: line 1, column 5 (last key "x"): invalid number`},
		{"array", "x = [1 2]\n", `toml: line 1, column 8 (last key "x"): invalid array`},
		{"trailing text", "x = 12ghp\n", `toml: line 1, column 7: unexpected text after a value`},
		{"bare word line", "ghp_SECRETabcdef\n", `toml: line 1, column 17: expected '=' after a key`},
		{"colour table", "[theme.colors]\naccent = { dark = \"#000000\", ligth = \"#ffffff\" }\n",
			`toml: line 2, column 11 (last key "theme.colors.accent"): invalid [theme.colors] entry: a colour takes a hex string or a {dark, light} table`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeStrict([]byte(c.body))
			if err == nil {
				t.Fatal("DecodeStrict accepted the body")
			}
			if err.Error() != c.want {
				t.Errorf("error = %q, want %q", err, c.want)
			}
			if m := digitRun.FindString(err.Error()); m != "" {
				t.Errorf("error carries a digit run %q from the value", m)
			}
			for _, leak := range []string{"ghp", "SECRET", "AKIA", "secret", "ligth"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error carries %q from the file", leak)
				}
			}
			var pe toml.ParseError
			if !errors.As(err, &pe) {
				t.Errorf("toml.ParseError is no longer on the chain: %v", err)
			}
		})
	}
}

// TestScrubTOMLError_EverySurface pins #687 across every surface a parse
// error reaches: DecodeStrict (the loader's WARN, the parse gate via
// DecodeError, doctor via ValidatePath), Describe (`forgectl config`), and
// the two legacy claunch.conf decoders.
func TestScrubTOMLError_EverySurface(t *testing.T) {
	body := "log_level = \"info\"\ntoken = " + secretish + "\n"

	check := func(t *testing.T, surface string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: err = nil for a file that does not parse", surface)
		}
		msg := err.Error()
		if strings.Contains(msg, "AKIA") {
			t.Errorf("%s: error echoes the value: %q", surface, msg)
		}
		if !strings.Contains(msg, `toml: line 2, column 9 (last key "token"): expected a value`) {
			t.Errorf("%s: error = %q, want the position, key and hint", surface, msg)
		}
	}

	_, err := DecodeStrict([]byte(body))
	check(t, "DecodeStrict", err)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	check(t, "LoadPath.DecodeError", LoadPath(path).DecodeError())
	check(t, "ValidatePath", ValidatePath(path))
	_, rep := describeFile(path)
	check(t, "Describe", rep.DecodeErr)

	_, _, err = decodeLegacyLaunch([]byte("[defaults]\nmodel = " + secretish + "\n"))
	if err == nil || strings.Contains(err.Error(), "AKIA") {
		t.Errorf("decodeLegacyLaunch: err = %v, want a parse error without the value", err)
	}
	if !errors.Is(err, ErrLegacyMalformed) {
		t.Errorf("decodeLegacyLaunch: err = %v, want ErrLegacyMalformed kept", err)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "claunch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "claunch", "claunch.conf"), []byte("[defaults]\nmodel = "+secretish+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = LoadLegacyLaunch()
	if err == nil || strings.Contains(err.Error(), "AKIA") {
		t.Errorf("LoadLegacyLaunch: err = %v, want a parse error without the value", err)
	}
}

// TestScrubTOMLError_TypeMismatchKeyIsCapped: a decoder type mismatch names
// the full key path. In a user-keyed map a 400-rune key echoed twice before
// #738; DecodeStrict now renders it from the fixed template, capped.
func TestScrubTOMLError_TypeMismatchKeyIsCapped(t *testing.T) {
	key := strings.Repeat("K", 400)
	_, err := DecodeStrict([]byte("[launch.defaults.env]\n" + key + " = 98765\n"))
	if err == nil {
		t.Fatal("DecodeStrict accepted an int for a string env value")
	}
	msg := err.Error()
	if strings.Contains(msg, strings.Repeat("K", 81)) || !strings.Contains(msg, "…") {
		t.Errorf("error = %q, want the key capped", msg)
	}
	if !strings.HasSuffix(msg, ": wrong value type: found integer, want string") {
		t.Errorf("error = %q, want the fixed type-mismatch hint", msg)
	}
	if strings.Contains(msg, "98765") {
		t.Errorf("error echoes the value: %q", msg)
	}
}

// TestScrubTOMLError_KeyIsCapped: the last key is the operator's own text,
// echoed quoted and capped.
func TestScrubTOMLError_KeyIsCapped(t *testing.T) {
	_, err := DecodeStrict([]byte(strings.Repeat("K", 300) + " = ghp_SECRET\n"))
	if err == nil {
		t.Fatal("DecodeStrict accepted the body")
	}
	if msg := err.Error(); strings.Contains(msg, strings.Repeat("K", 81)) || !strings.Contains(msg, "…") {
		t.Errorf("error = %q, want the key capped", msg)
	}
}
