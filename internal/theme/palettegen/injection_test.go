package palettegen

import (
	"fmt"
	"strings"
	"testing"
)

// withVersion returns fixturePalette with its $version replaced by v, JSON-
// encoded so a newline in v is a real newline in the parsed string — which is
// the whole point: JSON permits it, and that is what makes the injection below
// expressible in a well-formed palette file.
func withVersion(t *testing.T, v string) []byte {
	t.Helper()
	quoted := fmt.Sprintf("%q", v)
	out := strings.Replace(fixturePalette, `"9.9.9"`, quoted, 1)
	if out == fixturePalette {
		t.Fatal("fixture substitution did not fire; the test would prove nothing")
	}
	return []byte(out)
}

// TestRender_RejectsCodeInjectionViaVersion is the regression test for the
// sharpest finding on this branch.
//
// $version is the one palette value interpolated into generated Go as bare
// text, and it lands inside the doc comment above func Artificer() — after the
// package clause, where injected Go still compiles. A crafted version can
// close that comment and append an import and an init function. go/format
// accepts the result and reports success, and the freshness test goes GREEN,
// because it re-runs this same function and byte-compares its own output
// against the committed file. The landing site is a "DO NOT EDIT" file, which
// is the file a reviewer skims.
//
// Assertions are on the error AND on nothing being emitted, so this cannot
// pass merely because rendering failed for an unrelated reason.
func TestRender_RejectsCodeInjectionViaVersion(t *testing.T) {
	hostile := map[string]string{
		"comment break then init": "0.25.0\nimport \"os/exec\"\nfunc init(){ _ = exec.Command(\"sh\") }\n// (",
		"block comment escape":    "0.25.0*/\nfunc init(){}\n/*",
		"bare newline":            "0.25.0\n",
		"leading punctuation":     "*/0.25.0",
		"absurdly long":           strings.Repeat("9", 200),
	}
	for name, v := range hostile {
		t.Run(name, func(t *testing.T) {
			out, err := Render(withVersion(t, v))
			if err == nil {
				t.Fatalf("Render accepted $version %q and produced:\n%s", v, out)
			}
			if len(out) != 0 {
				t.Errorf("Render returned %d bytes alongside its error; it must emit nothing", len(out))
			}
		})
	}

	// Positive control: the fixture's own version still renders. Without this
	// the table above would pass against a Render that rejected everything.
	if _, err := Render([]byte(fixturePalette)); err != nil {
		t.Fatalf("Render rejected the legitimate fixture version: %v", err)
	}
}

// TestRender_RejectsNonHexColour pins that a palette value which is not a
// colour never reaches the generated file. Theme.Hex hands its result to a
// terminal, so "not a colour" and "a control sequence" are the same problem —
// the fixture below carries a real ESC.
func TestRender_RejectsNonHexColour(t *testing.T) {
	for name, bad := range map[string]string{
		"escape sequence": "\x1b[2Kpwned",
		"not a colour":    "rebeccapurple",
		"shorthand hex":   "#fff",
		"empty":           "#",
	} {
		t.Run(name, func(t *testing.T) {
			payload := strings.Replace(fixturePalette, `"#111111"`, fmt.Sprintf("%q", bad), 1)
			if payload == fixturePalette {
				t.Fatal("fixture substitution did not fire; the test would prove nothing")
			}
			out, err := Render([]byte(payload))
			if err == nil {
				t.Fatalf("Render accepted colour %q and produced:\n%s", bad, out)
			}
			if len(out) != 0 {
				t.Errorf("Render returned %d bytes alongside its error", len(out))
			}
		})
	}
}
