package surface_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"

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
	"#comment",
	"café",
	"emoji-free-but-hi-bit-\xc3\xa9",
	"--flag=value",
	"-",
	"--",
	"/Users/some one/bin/forgectl",
	"/private/tmp/fc-a1b2c3d4/s",
	"beefcafe0123456789abcdef0123456789abcdef0123456789abcdef0beefcaf",
}

// posixShells returns the POSIX-family shells present on this machine. Scripts
// written for `$@` and `set --` run only here; fish is a different language and
// gets its own assertion.
func posixShells(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, name := range []string{"sh", "bash", "zsh"} {
		if path, err := exec.LookPath(name); err == nil {
			found[name] = path
		}
	}
	if _, ok := found["sh"]; !ok {
		t.Fatal("no /bin/sh on this machine; the round-trip cannot be proven")
	}
	return found
}

// allShells adds the non-POSIX dialects the refusal set exists for. They are
// opportunistic — see TestQuote_FishIsTheReasonForTheRefusalSet for what it
// means that fish is usually absent.
func allShells(t *testing.T) map[string]string {
	t.Helper()
	found := posixShells(t)
	for _, name := range []string{"fish", "tcsh"} {
		if path, err := exec.LookPath(name); err == nil {
			found[name] = path
		}
	}
	return found
}

// TestQuote_RoundTripsThroughRealShells is the assertion that matters. Testing
// the encoder against its own rules would only prove it is self-consistent;
// this proves a real shell hands the word back unchanged.
func TestQuote_RoundTripsThroughRealShells(t *testing.T) {
	available := allShells(t)
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
	// POSIX only: the scripts below use `$@` and `set --`, neither of which is
	// fish syntax — fish has no `$@`, and `set --` is its option terminator.
	// Running them under fish would go red for a reason unrelated to the
	// encoder. Fish gets its own assertion below.
	available := posixShells(t)

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

// TestQuote_FishIsTheReasonForTheRefusalSet checks word boundaries in fish's
// own syntax, and reports plainly when fish is absent.
//
// Fish is the shell the refusal set exists for, and it is not installed on most
// developer machines or in this repo's CI. That means the fish half of the
// claim is *argued* rather than *tested* most of the time. Skipping silently
// would let that read as coverage; this says so.
func TestQuote_FishIsTheReasonForTheRefusalSet(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed: the refusal set's fish rationale is untested here. " +
			"Install fish to exercise it, or read Quote's doc comment for the argument.")
	}

	words := []string{"/Users/some one/bin/forgectl", "surface", "_exec", "", "a b c", "$HOME", "*"}
	line, err := surface.QuoteCommand(words)
	if err != nil {
		t.Fatalf("QuoteCommand: %v", err)
	}

	// fish syntax: expand the line into a command that prints each argument
	// NUL-terminated, so a merged or split word is visible.
	script := "for w in " + line + "\nprintf '%s\\0' $w\nend"
	//nolint:gosec // G204: running a real shell on the encoder's output IS the test
	got, err := exec.CommandContext(t.Context(), fish, "-c", script).Output()
	if err != nil {
		t.Fatalf("fish -c %q: %v", script, err)
	}

	var want strings.Builder
	for _, w := range words {
		want.WriteString(w)
		want.WriteByte(0)
	}
	if string(got) != want.String() {
		t.Errorf("fish argv round-trip differs\n got %q\nwant %q\nline: %s", got, want.String(), line)
	}
}

// TestQuote_RefusesWhatCannotMeanOneThing pins the characters whose
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
		"a quote in a path":   "/Users/o'brien/bin/forgectl",

		// csh and tcsh expand history inside single quotes, and do it
		// non-interactively — verified: tcsh -c "printf %s 'a!b'" fails with
		// "Event not found" while sh and bash return a!b.
		"a bang":       "a!b",
		"double bang":  "!!",
		"bang in path": "/Users/x/bin/forge!ctl",

		// Control characters are refused for a different reason than the
		// others: the bootstrap is *typed*, so quoting is not the layer that
		// contains them. A newline submits the line and runs the remainder as
		// a fresh command no matter how it is quoted.
		"an embedded NUL":   "a\x00b",
		"a newline":         "a\nb",
		"a carriage return": "a\rb",
		"a tab":             "a\tb",
		"an escape":         "a\x1bb",
		"a bell":            "a\ab",
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
			// The refusal must be attributable, or a widened refusal set would
			// let this target pass while exercising almost nothing. Keep this
			// predicate in step with quotable().
			if !refusable(word) {
				t.Fatalf("Quote refused %q, which carries none of the refused characters", word)
			}
			return
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

// refusable mirrors quotable's predicate, so the fuzz target can tell a
// legitimate refusal from a refusal set that quietly grew.
func refusable(word string) bool {
	for _, r := range word {
		if r == '\'' || r == '\\' || r == '!' || termsafe.IsUnsafeTerminalRune(r) {
			return true
		}
	}
	return false
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
