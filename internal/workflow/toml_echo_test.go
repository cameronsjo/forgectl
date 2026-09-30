package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// echoMarker is planted in a TOML value; a parse error must never carry it
// (#687, #738).
const echoMarker = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // G101: AWS's documented example key, a fixture asserted never to echo

// TestParse_DecodeErrorsNeverEchoAValue drives both Parse decodes (the
// dsl_version probe and the full decode) through a lexer error and a type
// mismatch with a long key, and requires the scrubbed text.
func TestParse_DecodeErrorsNeverEchoAValue(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"probe lex error", "dsl_version = 1\nname = " + echoMarker + "\n", `toml: line 2, column 8 (last key "name"): expected a value`},
		{"full decode type mismatch", "dsl_version = 1\nname = 98765\n", `toml: line 2 (last key "name"): wrong value type: found integer, want string`},
		{"unknown keys capped", "dsl_version = 1\n" + strings.Repeat("K", 300) + " = 1\n", `"…`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.body))
			if err == nil {
				t.Fatal("Parse accepted the body")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.want) {
				t.Errorf("Parse error = %q, want it to contain %q", msg, c.want)
			}
			for _, leak := range []string{"AKIA", "98765", strings.Repeat("K", 81)} {
				if strings.Contains(msg, leak) {
					t.Errorf("Parse error = %q carries %q", msg, leak)
				}
			}
		})
	}
}

// TestLoadState_DecodeErrorNeverEchoesAValue: a corrupt run-state sidecar is
// reported through the same scrubbed text.
func TestLoadState_DecodeErrorNeverEchoesAValue(t *testing.T) {
	dir := redirectStateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "demo.state.toml")
	if err := os.WriteFile(path, []byte("run_id = "+echoMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadState("demo")
	if err == nil {
		t.Fatal("LoadState accepted a corrupt state file")
	}
	if msg := err.Error(); strings.Contains(msg, "AKIA") || !strings.Contains(msg, "expected a value") {
		t.Errorf("LoadState error = %q, want the scrubbed hint and no value", msg)
	}
}
