// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// rootHelpMaxBytes is the size ceiling for `forgectl --help` off a terminal.
// The checklist's suggestion for a top-level help is 4 KB (forgectl#1086); the
// page cannot reach it while every command's own usage tokens set the column,
// so the bound pins today's size with a little headroom.
const rootHelpMaxBytes = 4800

func TestTrailingTrimWriter(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{"padding before a newline", []string{"a   \nb\t \n"}, "a\nb\n"},
		{"inner spaces kept", []string{"a  b   c\n"}, "a  b   c\n"},
		{"leading spaces kept", []string{"   a\n"}, "   a\n"},
		{"padding split across writes", []string{"a  ", "  \nb"}, "a\nb"},
		{"spaces split across writes then text", []string{"a ", " b\n"}, "a  b\n"},
		{"crlf", []string{"a  \r\nb\n"}, "a\r\nb\n"},
		{"blank padded line", []string{"   \n"}, "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sink bytes.Buffer
			w := &trailingTrimWriter{w: &sink}
			for _, s := range tt.writes {
				n, err := w.Write([]byte(s))
				if err != nil || n != len(s) {
					t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", s, n, err, len(s))
				}
			}
			if got := sink.String(); got != tt.want {
				t.Errorf("sink = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrimIfNotTerminal(t *testing.T) {
	prev := isTerminalFd
	t.Cleanup(func() { isTerminalFd = prev })

	isTerminalFd = func(uintptr) bool { return true }
	if got := trimIfNotTerminal(os.Stdout); got != os.Stdout {
		t.Errorf("terminal file wrapped: got %T, want the file untouched", got)
	}

	isTerminalFd = func(uintptr) bool { return false }
	wrapped := trimIfNotTerminal(os.Stdout)
	if _, ok := wrapped.(*trailingTrimWriter); !ok {
		t.Errorf("non-terminal file: got %T, want *trailingTrimWriter", wrapped)
	}
	if again := trimIfNotTerminal(wrapped); again != wrapped {
		t.Error("wrapping twice must return the first wrapper")
	}
	if _, ok := trimIfNotTerminal(new(bytes.Buffer)).(*trailingTrimWriter); !ok {
		t.Error("a non-file writer must count as not a terminal")
	}
}

// runFramed runs args through execCommand with both streams captured, which is
// the same path main takes through fang.
func runFramed(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	t.Setenv(skipLegacyMigrateEnv, "1")
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	root := newRoot(deps)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	_ = execCommand(context.Background(), root, args, deps.Theme)
	return out.String(), errBuf.String()
}

func assertNoTrailingSpace(t *testing.T, label, text string) {
	t.Helper()
	if text == "" {
		t.Fatalf("%s: no output captured, the check could not fail", label)
	}
	for i, line := range strings.Split(text, "\n") {
		if strings.TrimRight(line, " \t") != line {
			t.Errorf("%s line %d ends in whitespace: %q", label, i+1, line)
			return
		}
	}
}

// TestFrames_NoTrailingPaddingOffTerminal pins forgectl#1086: fang pads every
// help and error line to 120 columns when the stream is not a terminal.
func TestFrames_NoTrailingPaddingOffTerminal(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"desk", "--help"},
		{"review", "--help"},
		{"resume", "restart", "--help"},
		{"tasks", "mcp", "--help"},
	} {
		stdout, _ := runFramed(t, args...)
		assertNoTrailingSpace(t, "help "+strings.Join(args, " "), stdout)
	}
	for _, args := range [][]string{
		{"desk", "bogus"},
		{"nosuchverb"},
		{"desk", "add"},
	} {
		_, stderr := runFramed(t, args...)
		assertNoTrailingSpace(t, "error "+strings.Join(args, " "), stderr)
	}
}

func TestRootHelp_SizeBound(t *testing.T) {
	stdout, _ := runFramed(t, "--help")
	if len(stdout) > rootHelpMaxBytes {
		t.Errorf("forgectl --help is %d bytes off a terminal, want <= %d (forgectl#1086)", len(stdout), rootHelpMaxBytes)
	}
}

// TestFrames_TerminalStreamKeepsItsFrame holds the other half of the contract:
// a stream that is a terminal gets fang's frame as before. The test hands
// execCommand an os.File that the seam reports as a terminal, so fang's width
// padding must still be there.
func TestFrames_TerminalStreamKeepsItsFrame(t *testing.T) {
	t.Setenv(skipLegacyMigrateEnv, "1")
	prev := isTerminalFd
	t.Cleanup(func() { isTerminalFd = prev })
	isTerminalFd = func(uintptr) bool { return true }

	f, err := os.CreateTemp(t.TempDir(), "help")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	root := newRoot(deps)
	root.SetOut(f)
	root.SetErr(new(bytes.Buffer))
	_ = execCommand(context.Background(), root, []string{"review", "--help"}, deps.Theme)

	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("  \n")) {
		t.Error("a stream reported as a terminal lost fang's width padding; the trim must apply only off a terminal")
	}
}
