package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// secretish is the leading bare word BurntSushi/toml quotes back when an
// unquoted value fails to lex: `found "ghp"` for a GitHub token, up to twelve
// characters for an AWS-style key (#687).
const secretish = "AKIAIOSFODNN7EXAMPLE"

// TestScrubTOMLError_DropsQuotedValueKeepsPosition pins #687 across every
// surface a parse error reaches: DecodeStrict (the loader's WARN, the parse
// gate via DecodeError, doctor via ValidatePath), Describe (`forgectl
// config`), and the legacy claunch.conf decoder.
func TestScrubTOMLError_DropsQuotedValueKeepsPosition(t *testing.T) {
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
		for _, want := range []string{"line 2", "column 9", `last key "token"`, `found "…"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: error = %q, want it to keep %s", surface, msg, want)
			}
		}
		var pe toml.ParseError
		if !errors.As(err, &pe) {
			t.Errorf("%s: toml.ParseError is no longer on the chain: %v", surface, err)
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

// TestScrubTOMLError_PassesThroughNonParseErrors: a type mismatch names the
// key and the two types, never the value, and is left as the decoder wrote it.
func TestScrubTOMLError_PassesThroughNonParseErrors(t *testing.T) {
	plain := errors.New("toml: line 1 (last key \"log_level\"): incompatible types")
	if got := scrubTOMLError(plain); got != plain {
		t.Errorf("scrubTOMLError rewrote a non-ParseError: %v", got)
	}
	if scrubTOMLError(nil) != nil {
		t.Error("scrubTOMLError(nil) != nil")
	}
}
