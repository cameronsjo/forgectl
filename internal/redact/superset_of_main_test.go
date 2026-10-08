package redact_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/redact"
)

// supersetFrags are the pieces supersetInput joins: credential keys and
// non-credential ones, every key/value separator, quotes and string
// delimiters, braces, CSI, OSC, nF, Fp and DCS/APC escapes with and without
// their terminators, declaration keywords, tokens and plain values, PEM
// armor, digest refs and line breaks. They are the Gate 2 reviewer's
// alphabet for #991, #992 and #996.
var supersetFrags = []string{
	"password", "token", "secret", "api_key", "token_type", "description", "user", "config",
	`"""`, "'''", `"`, "'", ":", ": ", " = ", "=", ":=", "{", "}", ",", ", ", " ", "\t",
	"\x1b[1m", "\x1b[0m", "\x1b]8;;http://x\x1b\\", "\x1b]8;;\a", "\x1b(B", "\x1b7", "\x1bP", "\x1b\\", "\x1b_", "\a", "\x1b[1g", "\x1b",
	"\\", `\"`, "const ", "export ", "let ", "var ", "- ", "!!str ",
	"ghp_" + strings.Repeat("A", 36), "SEKRIT", "hunter2", "abc", "null", "expired", "~/.x",
	"-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----", "|", "https://u:p@h/", "@sha256:", strings.Repeat("0f", 32), "r/a:1",
	"\n", "\n", "\n", "\n  ", "\r\n", "Bearer ", "Authorization: ", "--password ",
}

// supersetInput joins one to fourteen supersetFrags picked by r.
func supersetInput(r *rand.Rand) string {
	var b strings.Builder
	for n := 1 + r.Intn(14); n > 0; n-- {
		b.WriteString(supersetFrags[r.Intn(len(supersetFrags))])
	}
	return b.String()
}

// supersetCases are inputs TestText_WithholdsSupersetOfMain runs before the
// random ones: shapes a random join rarely builds, where a #991 string's text
// holds a line main's machines read (a YAML value opener, a PEM BEGIN).
var supersetCases = []string{
	"password = \"\"\"\npassword:\n  \"\"\"\n  SEKRIT",
	"token = '''\n-----BEGIN PRIVATE KEY-----\n'''\nMIIE\n-----END PRIVATE KEY-----",
	"password: \"a\n  token: |\n  b\"\n    SEKRIT",
}

// TestText_WithholdsSupersetOfMain: over supersetCases and 250,000 seeded
// random joins of supersetFrags, every line main's Text or Stdout withheld (the frozen copy
// in frozen_main_test.go) is still withheld by the live Text or Stdout, and
// the live Text withholds every line the live Stdout does. #991, #992 and
// #996 may only add to what is withheld, so a change that drops a view or a
// state main had fails here even when no hand-written row covers it (Gate 2
// found 55 such lines in the first cut of #996).
//
// Mutations that turn it red: make stdoutShape check the full strip in place
// of main's stripEscapes view (the lone ESC '\' and "--password ESC ( B"
// lines main withheld come through); run openString only on lines no other
// state holds, or skip the main machines on a line inside a string (a value
// main opened inside the string's text is lost).
func TestText_WithholdsSupersetOfMain(t *testing.T) {
	r := rand.New(rand.NewSource(991992996)) //nolint:gosec // G404: deterministic test fixture, not crypto
	failures := 0
	for k := range 250_000 + len(supersetCases) {
		var in string
		if k < len(supersetCases) {
			in = supersetCases[k]
		} else {
			in = supersetInput(r)
		}
		if strings.Contains(in, redact.Marker) {
			continue
		}
		lines := strings.Split(in, "\n")
		mainText, mainStdout := strings.Split(mainText(in), "\n"), strings.Split(mainStdout(in), "\n")
		text, stdout := strings.Split(redact.Text(in), "\n"), strings.Split(redact.Stdout(in), "\n")
		for i, line := range lines {
			lost := ""
			switch {
			case mainText[i] != line && text[i] == line:
				lost = "Text keeps a line main's Text withheld"
			case mainStdout[i] != line && stdout[i] == line:
				lost = "Stdout keeps a line main's Stdout withheld"
			case stdout[i] != line && text[i] == line:
				lost = "Text keeps a line Stdout withholds"
			}
			if lost != "" {
				failures++
				if failures <= 10 {
					t.Errorf("%s: line %d of %q", lost, i, in)
				}
			}
		}
	}
	if failures > 10 {
		t.Errorf("%d lines in all", failures)
	}
}
