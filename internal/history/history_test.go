package history

// Test plan for history.go
//
// Parse (Classification: parser over untrusted input)
//   [x] Happy: extended format yields command, timestamp, elapsed, oldest first
//   [x] Happy: plain format yields the command with a zero timestamp
//   [x] Happy: a backslash continuation folds into one entry with a real newline
//   [x] Happy: metafied high bytes decode back to UTF-8
//   [x] Happy: control and bidi runes survive parsing verbatim (the CLI sanitizes)
//   [x] Fail-closed: empty and whitespace-only files refuse
//   [x] Fail-closed: unterminated continuation refuses as truncated
//   [x] Fail-closed: trailing lone metafy marker refuses as truncated
//   [x] Fail-closed: non-UTF-8 after unmetafying refuses
//   [x] Fail-closed: every malformed extended header shape refuses
//   [x] Fail-closed: an elapsed field that would overflow a Duration refuses
//   [x] Fail-closed: a start time outside a plausible epoch window refuses
//   [x] Fail-closed: a file past MaxEntries refuses
//   [x] Format: the first record decides the format and every record is held to it
//   [x] Format: a command ending in a backslash does not swallow the next record
//
// LastN
//   [x] Happy: returns the tail, oldest first, capped at what exists
//   [x] Fail-closed: non-positive n and an empty slice refuse
//
// Read
//   [x] Happy: reads and parses a file
//   [x] Fail-closed: absent path, directory, and oversized file all refuse
//
// ResolvePath
//   [x] Happy: $HISTFILE wins; otherwise ~/.zsh_history
//   [x] Fail-closed: no $HISTFILE and no home directory refuses

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ctrl builds a control or bidi character from its code point, so no test
// source file ever contains one literally (a literal RLO reorders the source
// that asserts it is neutralized).
func ctrl(cp rune) string { return string(cp) }

func TestParse_ExtendedFormat(t *testing.T) {
	data := []byte(": 1690000000:0;echo one\n: 1690000060:12;echo two\n")

	entries, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if got, want := entries[0].Command, "echo one"; got != want {
		t.Errorf("entries[0].Command = %q, want %q", got, want)
	}
	if got, want := entries[1].Command, "echo two"; got != want {
		t.Errorf("entries[1].Command = %q, want %q", got, want)
	}
	if got, want := entries[0].Timestamp.Unix(), int64(1690000000); got != want {
		t.Errorf("entries[0].Timestamp.Unix() = %d, want %d", got, want)
	}
	if got, want := entries[1].Elapsed, 12*time.Second; got != want {
		t.Errorf("entries[1].Elapsed = %v, want %v", got, want)
	}
}

func TestParse_PlainFormat(t *testing.T) {
	entries, err := Parse([]byte("ls -la\ngit status\n"))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if got, want := entries[1].Command, "git status"; got != want {
		t.Errorf("entries[1].Command = %q, want %q", got, want)
	}
	if !entries[0].Timestamp.IsZero() {
		t.Errorf("entries[0].Timestamp = %v, want zero for plain format", entries[0].Timestamp)
	}
}

func TestParse_ContinuationFoldsIntoOneEntry(t *testing.T) {
	data := []byte(": 1690000000:0;for f in a b\\\ndo echo $f\\\ndone\n: 1690000100:0;echo after\n")

	entries, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	want := "for f in a b\ndo echo $f\ndone"
	if entries[0].Command != want {
		t.Errorf("entries[0].Command = %q, want %q", entries[0].Command, want)
	}
	if entries[1].Command != "echo after" {
		t.Errorf("entries[1].Command = %q, want %q", entries[1].Command, "echo after")
	}
}

func TestParse_UnmetafiesHighBytes(t *testing.T) {
	// zsh metafies any byte >= 0x80 as 0x83 followed by byte^0x20. "é" is
	// 0xC3 0xA9 in UTF-8, so it lands on disk as 0x83 0xE3 0x83 0x89.
	data := append([]byte(": 1690000000:0;echo "), 0x83, 0xE3, 0x83, 0x89, '\n')

	entries, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if got, want := entries[0].Command, "echo é"; got != want {
		t.Errorf("entries[0].Command = %q, want %q", got, want)
	}
}

func TestParse_PreservesHostileRunesForTheCallerToSanitize(t *testing.T) {
	payload := "echo " + ctrl(0x1B) + "[2J" + ctrl(0x202E) + "gpj.exe"
	entries, err := Parse([]byte(": 1690000000:0;" + payload + "\n"))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Command != payload {
		t.Errorf("Parse rewrote the command; the parser must not sanitize (that is termsafe's job at the sink)")
	}
}

func TestParse_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", []byte(""), ErrNoHistory},
		{"whitespace only", []byte("\n\n"), ErrNoHistory},
		{"unterminated continuation", []byte(": 1690000000:0;echo one\\\n"), ErrTruncated},
		{"trailing metafy marker", append([]byte(": 1690000000:0;echo one\n"), 0x83), ErrTruncated},
		{"invalid utf-8", []byte(": 1690000000:0;echo \xc3\n"), ErrEncoding},
		{"header missing semicolon", []byte(": 1690000000:0;echo one\n: 1690000000:0 echo two\n"), ErrMalformed},
		{"header missing colon", []byte(": 1690000000:0;echo one\n: 1690000000;echo two\n"), ErrMalformed},
		{"non-numeric timestamp", []byte(": 1690000000:0;echo one\n: notatime:0;echo two\n"), ErrMalformed},
		{"non-numeric elapsed", []byte(": 1690000000:0;echo one\n: 1690000000:soon;echo two\n"), ErrMalformed},
		{"negative timestamp", []byte(": 1690000000:0;echo one\n: -1690000000:0;echo two\n"), ErrMalformed},
		{"negative elapsed", []byte(": 1690000000:0;echo one\n: 1690000000:-5;echo two\n"), ErrMalformed},
		{"empty command", []byte(": 1690000000:0;\n"), ErrMalformed},
		{"blank line between entries", []byte("echo one\n\necho two\n"), ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := Parse(tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse error = %v, want %v", err, tc.want)
			}
			if entries != nil {
				t.Errorf("Parse returned %d entries alongside a refusal; a refusal must yield none", len(entries))
			}
		})
	}
}

func TestLastN(t *testing.T) {
	entries := []Entry{{Command: "a"}, {Command: "b"}, {Command: "c"}}

	got, err := LastN(entries, 2)
	if err != nil {
		t.Fatalf("LastN: unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Command != "b" || got[1].Command != "c" {
		t.Fatalf("LastN(entries, 2) = %v, want [b c] oldest first", got)
	}

	all, err := LastN(entries, 99)
	if err != nil {
		t.Fatalf("LastN over-request: unexpected error: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("LastN(entries, 99) returned %d entries, want all 3", len(all))
	}
}

func TestLastN_FailsClosed(t *testing.T) {
	entries := []Entry{{Command: "a"}}
	if _, err := LastN(entries, 0); !errors.Is(err, ErrRange) {
		t.Errorf("LastN(entries, 0) error = %v, want ErrRange", err)
	}
	if _, err := LastN(entries, -1); !errors.Is(err, ErrRange) {
		t.Errorf("LastN(entries, -1) error = %v, want ErrRange", err)
	}
	if _, err := LastN(nil, 1); !errors.Is(err, ErrNoHistory) {
		t.Errorf("LastN(nil, 1) error = %v, want ErrNoHistory", err)
	}
}

func TestRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".zsh_history")
	if err := os.WriteFile(path, []byte(": 1690000000:0;echo one\n"), 0o600); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	entries, err := Read(path)
	if err != nil {
		t.Fatalf("Read: unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Command != "echo one" {
		t.Fatalf("Read = %v, want one entry `echo one`", entries)
	}
}

func TestRead_FailsClosed(t *testing.T) {
	dir := t.TempDir()

	t.Run("absent file", func(t *testing.T) {
		entries, err := Read(filepath.Join(dir, "nope"))
		if err == nil {
			t.Fatalf("Read of an absent history returned %d entries and no error; absent input must refuse", len(entries))
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Read error = %v, want an os.ErrNotExist chain", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		if _, err := Read(dir); err == nil {
			t.Fatal("Read of a directory returned no error")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(dir, "huge")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.Truncate(path, MaxFileBytes+1); err != nil {
			t.Fatalf("grow: %v", err)
		}
		if _, err := Read(path); !errors.Is(err, ErrTooLarge) {
			t.Errorf("Read error = %v, want ErrTooLarge", err)
		}
	})

	t.Run("malformed names the file", func(t *testing.T) {
		path := filepath.Join(dir, "bad")
		if err := os.WriteFile(path, []byte(": 1690000000:0;echo one\n: nope:0;echo two\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := Read(path); !errors.Is(err, ErrMalformed) {
			t.Errorf("Read error = %v, want ErrMalformed", err)
		}
	})
}

func TestParse_RefusesElapsedThatWouldOverflowADuration(t *testing.T) {
	entries, err := Parse([]byte(": 1690000000:9223372036854775807;echo one\n"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse error = %v, want ErrMalformed", err)
	}
	if entries != nil {
		t.Errorf("Parse returned %d entries alongside a refusal", len(entries))
	}
}

func TestParse_FormatIsDecidedOncePerFile(t *testing.T) {
	t.Run("plain file keeps a colon-builtin command", func(t *testing.T) {
		// `:` is a real zsh builtin, so a plain-format history legitimately
		// holds lines that open like an extended header but are not one.
		entries, err := Parse([]byte("echo one\n: > /tmp/truncate-me\n"))
		if err != nil {
			t.Fatalf("Parse: unexpected error: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("len(entries) = %d, want 2", len(entries))
		}
		if got, want := entries[1].Command, ": > /tmp/truncate-me"; got != want {
			t.Errorf("entries[1].Command = %q, want %q", got, want)
		}
	})

	t.Run("extended file refuses a record that is not a header", func(t *testing.T) {
		entries, err := Parse([]byte(": 1690000000:0;echo one\necho two\n"))
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("Parse error = %v, want ErrMalformed", err)
		}
		if entries != nil {
			t.Errorf("Parse returned %d entries alongside a refusal", len(entries))
		}
	})
}

func TestParse_TrailingBackslashDoesNotSwallowTheNextRecord(t *testing.T) {
	// The first command's text genuinely ends in a backslash. Folding it into
	// the next record would show the reader a command that never ran.
	entries, err := Parse([]byte(": 1690000000:0;echo 'a\\\n: 1690000060:0;echo two\n"))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 — the next header must not fold in", len(entries))
	}
	if got, want := entries[1].Command, "echo two"; got != want {
		t.Errorf("entries[1].Command = %q, want %q", got, want)
	}
}

func TestParse_RefusesStartTimeOutOfRange(t *testing.T) {
	entries, err := Parse([]byte(": 9223372036854775807:0;echo one\n"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse error = %v, want ErrMalformed", err)
	}
	if entries != nil {
		t.Errorf("Parse returned %d entries alongside a refusal", len(entries))
	}
}

func TestParse_RefusesTooManyEntries(t *testing.T) {
	data := []byte(strings.Repeat("a\n", MaxEntries+1))

	entries, err := Parse(data)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Parse error = %v, want ErrTooLarge", err)
	}
	if entries != nil {
		t.Errorf("Parse returned %d entries alongside a refusal", len(entries))
	}
}

func TestResolvePath(t *testing.T) {
	t.Run("HISTFILE wins", func(t *testing.T) {
		got, err := ResolvePath(
			func(k string) string {
				if k == "HISTFILE" {
					return "/tmp/custom_history"
				}
				return ""
			},
			func() (string, error) { return "/home/someone", nil },
		)
		if err != nil {
			t.Fatalf("ResolvePath: unexpected error: %v", err)
		}
		if got != "/tmp/custom_history" {
			t.Errorf("ResolvePath = %q, want the $HISTFILE value", got)
		}
	})

	t.Run("falls back to the home default", func(t *testing.T) {
		got, err := ResolvePath(
			func(string) string { return "" },
			func() (string, error) { return "/home/someone", nil },
		)
		if err != nil {
			t.Fatalf("ResolvePath: unexpected error: %v", err)
		}
		if want := filepath.Join("/home/someone", ".zsh_history"); got != want {
			t.Errorf("ResolvePath = %q, want %q", got, want)
		}
	})

	t.Run("no HISTFILE and no home refuses", func(t *testing.T) {
		got, err := ResolvePath(
			func(string) string { return "" },
			func() (string, error) { return "", errors.New("no home") },
		)
		if err == nil {
			t.Fatalf("ResolvePath returned %q and no error; an unknown home must refuse", got)
		}
		if got != "" {
			t.Errorf("ResolvePath returned %q alongside a refusal", got)
		}
	})

	t.Run("refuses a terminal control in either source", func(t *testing.T) {
		hostile := "/home/" + ctrl(0x1B) + "[2J" + ctrl(0x202E) + "history"

		fromEnv, err := ResolvePath(
			func(k string) string {
				if k == "HISTFILE" {
					return hostile
				}
				return ""
			},
			func() (string, error) { return "/home/someone", nil },
		)
		if err == nil {
			t.Fatalf("ResolvePath accepted a $HISTFILE carrying terminal controls, returning %q", fromEnv)
		}
		if strings.ContainsAny(err.Error(), ctrl(0x1B)+ctrl(0x202E)) {
			t.Error("ResolvePath's refusal carries the raw control characters to the terminal")
		}

		if _, err := ResolvePath(
			func(string) string { return "" },
			func() (string, error) { return hostile, nil },
		); err == nil {
			t.Error("ResolvePath accepted a home directory carrying terminal controls")
		}
	})
}
