package surface_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface"
)

// hostileWords are the shapes a shell would otherwise interpret. Each must
// come back byte-identical through a real shell.
var hostileWords = []string{
	"plain",
	"",
	"with a space",
	"two  consecutive  spaces",
	" leading and trailing ",
	"$HOME",
	"${HOME}",
	"$(id -u)",
	"`id -u`",
	"a;rm -rf /",
	"a&&b",
	"a||b",
	"a|b",
	"a>b",
	"a<b",
	"*",
	"?",
	"[a-z]",
	"~",
	"~root",
	"=ls", // zsh: leading = is command-path expansion
	"!!",  // history expansion
	"#comment",
	"a\nb", // newline inside a word
	"a\tb",
	"a\rb",
	"café",
	"emoji-free-but-hi-bit-\xc3\xa9",
	"--flag=value",
	"-",
	"--",
	"/Users/some one/bin/forgectl",
	"/private/tmp/fc-a1b2c3d4/s",
	"beefcafe0123456789abcdef0123456789abcdef0123456789abcdef0beefcaf",
}

// shells returns the shells present on this machine. sh is required; the rest
// are opportunistic, because the point is to prove one encoding satisfies
// several dialects rather than to require any particular one be installed.
func shells(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, name := range []string{"sh", "bash", "zsh", "fish"} {
		if path, err := exec.LookPath(name); err == nil {
			found[name] = path
		}
	}
	if _, ok := found["sh"]; !ok {
		t.Fatal("no /bin/sh on this machine; the round-trip cannot be proven")
	}
	return found
}

// TestQuote_RoundTripsThroughRealShells is the assertion that matters. Testing
// the encoder against its own rules would only prove it is self-consistent;
// this proves a real shell hands the word back unchanged.
func TestQuote_RoundTripsThroughRealShells(t *testing.T) {
	available := shells(t)
	t.Logf("shells under test: %s", strings.Join(shellNames(available), ", "))

	for _, word := range hostileWords {
		t.Run(shellSafeName(word), func(t *testing.T) {
			quoted, err := surface.Quote(word)
			if err != nil {
				t.Fatalf("Quote(%q): %v", word, err)
			}

			for name, path := range available {
				// printf %s rather than echo: echo mangles backslashes and
				// leading dashes in several of these shells, which would make
				// the harness the thing under test.
				//nolint:gosec // G204: running a real shell on the encoder's output IS the test
				out, err := exec.CommandContext(t.Context(), path, "-c", "printf %s "+quoted).Output()
				if err != nil {
					t.Errorf("%s -c 'printf %%s %s': %v", name, quoted, err)
					continue
				}
				if string(out) != word {
					t.Errorf("%s round-trip of %q gave %q (quoted form: %s)", name, word, out, quoted)
				}
			}
		})
	}
}

// TestQuoteCommand_KeepsWordBoundaries proves the join does not merge
// arguments. A word count is the assertion: quoting each word correctly and
// then joining them wrongly produces a command that still runs, with the
// wrong argv.
func TestQuoteCommand_KeepsWordBoundaries(t *testing.T) {
	available := shells(t)

	words := []string{
		"/Users/some one/bin/forgectl",
		"surface",
		"_exec",
		"--protocol", "1",
		"--socket", "/private/tmp/fc-a1b2/s",
		"--nonce", "beefcafe",
		"",
		"a b c",
	}

	line, err := surface.QuoteCommand(words)
	if err != nil {
		t.Fatalf("QuoteCommand: %v", err)
	}

	for name, path := range available {
		// Print each argument NUL-terminated so a merged or split argument is
		// visible, which a space-joined echo would hide.
		script := `printf '%s\0' "$@"`
		args := append([]string{"-c", script + " ; :", "sh"}, words...)
		//nolint:gosec // G204: ditto — this is the baseline argv
		want, err := exec.CommandContext(t.Context(), path, args...).Output()
		if err != nil {
			t.Fatalf("%s baseline: %v", name, err)
		}

		//nolint:gosec // G204: ditto — this is the quoted argv under test
		got, err := exec.CommandContext(t.Context(), path, "-c",
			`set -- `+line+`; printf '%s\0' "$@"`).Output()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s: argv round-trip differs\n got %q\nwant %q\nline: %s", name, got, want, line)
		}
	}
}

// TestQuote_RefusesWhatCannotMeanOneThing pins the two characters whose
// encoding genuinely differs between POSIX shells and fish. Escaping them for
// one dialect produces shell syntax in the other, so they are refused rather
// than guessed at.
func TestQuote_RefusesWhatCannotMeanOneThing(t *testing.T) {
	refused := map[string]string{
		"a single quote":      "it's",
		"only a single quote": "'",
		"a backslash":         `a\b`,
		"only a backslash":    `\`,
		"a windows-ish path":  `C:\Users\x`,
		"an embedded NUL":     "a\x00b",
		"a quote in a path":   "/Users/o'brien/bin/forgectl",
	}

	for name, word := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := surface.Quote(word); !errors.Is(err, surface.ErrUnquotable) {
				t.Errorf("Quote(%q) err = %v, want ErrUnquotable", word, err)
			}
			if _, err := surface.QuoteCommand([]string{"ok", word}); !errors.Is(err, surface.ErrUnquotable) {
				t.Errorf("QuoteCommand with %q err = %v, want ErrUnquotable", word, err)
			}
		})
	}

	// The control: every hostile word above IS quotable, so the refusals are
	// the two-character rule firing rather than the encoder refusing broadly.
	for _, word := range hostileWords {
		if _, err := surface.Quote(word); err != nil {
			t.Errorf("Quote(%q) refused a word it should encode: %v", word, err)
		}
	}
}

// TestQuoteCommand_RefusesAnEmptyCommand keeps a caller from producing a
// bootstrap that is the empty string, which a manager would happily type.
func TestQuoteCommand_RefusesAnEmptyCommand(t *testing.T) {
	if _, err := surface.QuoteCommand(nil); !errors.Is(err, surface.ErrUnquotable) {
		t.Errorf("QuoteCommand(nil) err = %v, want ErrUnquotable", err)
	}
	if _, err := surface.QuoteCommand([]string{}); !errors.Is(err, surface.ErrUnquotable) {
		t.Errorf("QuoteCommand([]) err = %v, want ErrUnquotable", err)
	}

	// An empty *word* is legal and must survive as an empty argument.
	line, err := surface.QuoteCommand([]string{""})
	if err != nil {
		t.Fatalf("QuoteCommand with one empty word: %v", err)
	}
	if line != "''" {
		t.Errorf("QuoteCommand([\"\"]) = %q, want ''", line)
	}
}

// FuzzQuote asserts the property directly: anything Quote accepts round-trips
// through /bin/sh byte-for-byte. The refusal set is the fuzzer's escape hatch,
// so a newly-discovered unencodable input fails the run rather than being
// quietly skipped.
func FuzzQuote(f *testing.F) {
	for _, word := range hostileWords {
		f.Add(word)
	}
	f.Add("it's")
	f.Add(`back\slash`)

	sh, err := exec.LookPath("sh")
	if err != nil {
		f.Skip("no sh available")
	}

	f.Fuzz(func(t *testing.T, word string) {
		quoted, err := surface.Quote(word)
		if err != nil {
			if !errors.Is(err, surface.ErrUnquotable) {
				t.Fatalf("Quote(%q) failed with an unexpected error: %v", word, err)
			}
			return
		}

		// A NUL cannot survive an argv round-trip regardless of quoting, and
		// Quote already refuses it — so reaching here with one is the bug.
		if strings.ContainsRune(word, 0) {
			t.Fatalf("Quote accepted a word containing NUL: %q", word)
		}

		//nolint:gosec // G204: the fuzzer's whole job is feeding this to a shell
		out, err := exec.CommandContext(t.Context(), sh, "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("sh rejected the quoted form %s (input %q): %v", quoted, word, err)
		}
		if string(out) != word {
			t.Fatalf("round-trip of %q gave %q (quoted: %s)", word, out, quoted)
		}
	})
}

func shellNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}

// shellSafeName renders a word as a subtest name without the characters that
// would make the name unreadable.
func shellSafeName(word string) string {
	if word == "" {
		return "empty"
	}
	r := strings.NewReplacer("\n", "\\n", "\t", "\\t", "\r", "\\r", " ", "_", "/", "-")
	name := r.Replace(word)
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}
