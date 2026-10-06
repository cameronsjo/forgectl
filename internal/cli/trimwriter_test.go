// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// rootHelpMaxBytes is the size ceiling for `forgectl --help` off a terminal.
// Stripping the padding takes root help from 5,809 to 5,575 bytes. It does not
// reach the 4 KB the checklist suggests (forgectl#1086): the rest is the column
// fang aligns to the widest command usage. The bound pins today's size so the
// page does not grow back.
const rootHelpMaxBytes = 5700

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
		{"padding then a color reset", []string{"a   \x1b[0m\n"}, "a\x1b[0m\n"},
		{"color sequence split across writes", []string{"a  \x1b[3", "8;5;1m\nb"}, "a\x1b[38;5;1m\nb"},
		{"padding inside colored text kept", []string{"\x1b[1ma  b\x1b[0m\n"}, "\x1b[1ma  b\x1b[0m\n"},
		{"end of stream keeps held whitespace", []string{"a  "}, "a  "},
		{"lone escape is content", []string{"a \x1bX \n"}, "a \x1bX\n"},
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
			if err := w.flush(); err != nil {
				t.Fatalf("flush() = %v", err)
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
	if got, tw := trimIfNotTerminal(os.Stdout); got != os.Stdout || tw != nil {
		t.Errorf("terminal file wrapped: got (%T, %v), want the file untouched", got, tw)
	}

	isTerminalFd = func(uintptr) bool { return false }
	if _, tw := trimIfNotTerminal(os.Stdout); tw == nil {
		t.Error("non-terminal file: want a trimming writer")
	}
	if _, tw := trimIfNotTerminal(new(bytes.Buffer)); tw == nil {
		t.Error("a non-file writer must count as not a terminal")
	}
}

func TestTrimErrorFrame(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile colorprofile.Profile
		want    string
	}{
		{"not a terminal", colorprofile.NoTTY, "a\nb\n"},
		{"a terminal profile keeps the frame", colorprofile.TrueColor, "a   \nb  \n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var sink bytes.Buffer
			w := &colorprofile.Writer{Forward: &sink, Profile: tt.profile}
			done := trimErrorFrame(w)
			_, _ = w.Write([]byte("a   \nb  \n"))
			done()
			if got := sink.String(); got != tt.want {
				t.Errorf("sink = %q, want %q", got, tt.want)
			}
		})
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

// dataVerbOutput is data whose trailing whitespace is content: a TSV row
// ending in an empty column, a line of padding, and a last line with no
// newline. dataVerbRoot adds a verb that prints it.
const dataVerbOutput = "id\tname\t\nrow \t\n   \nlast "

func dataVerbRoot(t *testing.T) (*cobra.Command, module.Deps) {
	t.Helper()
	t.Setenv(skipLegacyMigrateEnv, "1")
	deps := module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()}
	root := newRoot(deps)
	root.AddCommand(&cobra.Command{
		Use:   "dataprobe",
		Short: "Test verb that prints data with trailing whitespace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = cmd.OutOrStdout().Write([]byte(dataVerbOutput))
			_, _ = cmd.ErrOrStderr().Write([]byte("warn \t\nend "))
			return nil
		},
	})
	return root, deps
}

// TestTrim_VerbDataIsByteIdentical pins the review finding on forgectl#1127:
// the trim is for fang's frames only. A verb's data off a terminal must reach
// the stream byte for byte, including whitespace that ends a line and the
// stream's last bytes.
func TestTrim_VerbDataIsByteIdentical(t *testing.T) {
	root, deps := dataVerbRoot(t)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	if err := execCommand(context.Background(), root, []string{"dataprobe"}, deps.Theme); err != nil {
		t.Fatalf("execCommand() = %v", err)
	}
	if out.String() != dataVerbOutput {
		t.Errorf("stdout = %q, want %q", out.String(), dataVerbOutput)
	}
	if errBuf.String() != "warn \t\nend " {
		t.Errorf("stderr = %q, want it unchanged", errBuf.String())
	}
}

// TestTrim_HelpOfDataVerbStillTrimmed holds the other side: the same verb's own
// help page is trimmed, so scoping did not turn the trim off.
func TestTrim_HelpOfDataVerbStillTrimmed(t *testing.T) {
	root, deps := dataVerbRoot(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(new(bytes.Buffer))
	_ = execCommand(context.Background(), root, []string{"dataprobe", "--help"}, deps.Theme)
	assertNoTrailingSpace(t, "dataprobe --help", out.String())
}

// TestTrim_HelpKeepsInnerAlignment guards the Long text's code blocks: only
// whitespace that ends a line goes.
func TestTrim_HelpKeepsInnerAlignment(t *testing.T) {
	stdout, _ := runFramed(t, "review", "--help")
	for _, want := range []string{
		"forgectl review --json                machine-readable output",
		"forgectl review mark owner/repo#42    mark an item reviewed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("review --help lost inner spacing; want a line containing %q", want)
		}
	}
}

// TestWithTrimmedOut_FlushesAtTheEnd: a frame that stops without a newline
// loses nothing, and the command's stream is restored afterwards.
func TestWithTrimmedOut_FlushesAtTheEnd(t *testing.T) {
	var sink bytes.Buffer
	cmd := &cobra.Command{Use: "x"}
	cmd.SetOut(&sink)
	withTrimmedOut(cmd, func() {
		_, _ = cmd.OutOrStdout().Write([]byte("head  \ntail  "))
	})
	if got := sink.String(); got != "head\ntail  " {
		t.Errorf("sink = %q, want %q", got, "head\ntail  ")
	}
	if cmd.OutOrStdout() != &sink {
		t.Error("withTrimmedOut did not restore the command's stdout")
	}
}
