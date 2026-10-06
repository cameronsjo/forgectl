package cli

import (
	"io"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// trailingTrimWriter drops the spaces and tabs that end a line. fang renders
// help and error frames with a fixed-width style (120 columns off a terminal,
// whatever COLUMNS says), so every line of a redirected frame ends in padding:
// up to 46% of a help page (forgectl#1086). Trailing whitespace carries no
// meaning, so removing it at the stream leaves the text identical to a reader.
//
// A run of whitespace is held until the next byte shows whether the line ends
// there. Whitespace still held at the end of the stream is dropped.
type trailingTrimWriter struct {
	w       io.Writer
	pending []byte
}

// Write reports len(p) on success, as io.Writer requires, not the count of
// bytes forwarded.
func (t *trailingTrimWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		switch b {
		case ' ', '\t':
			t.pending = append(t.pending, b)
		case '\n', '\r':
			t.pending = t.pending[:0]
			out = append(out, b)
		default:
			out = append(out, t.pending...)
			t.pending = t.pending[:0]
			out = append(out, b)
		}
	}
	if len(out) > 0 {
		if _, err := t.w.Write(out); err != nil {
			return 0, err //nolint:wrapcheck // a writer must return the sink's error as is
		}
	}
	return len(p), nil
}

// isTerminalFd is a seam so a test can pin either answer without a pty.
var isTerminalFd = term.IsTerminal

// trimIfNotTerminal wraps w in a trailingTrimWriter unless w is a terminal.
// A writer that is not a file (a buffer, a pipe wrapper) counts as not a
// terminal. It is idempotent, so a second call on an already wrapped writer
// returns it unchanged.
func trimIfNotTerminal(w io.Writer) io.Writer {
	if _, ok := w.(*trailingTrimWriter); ok {
		return w
	}
	if f, ok := w.(term.File); ok && isTerminalFd(f.Fd()) {
		return w
	}
	return &trailingTrimWriter{w: w}
}

// trimFramesOffTerminal applies trimIfNotTerminal to each of cmd's output
// streams on its own, so redirecting stdout leaves a terminal stderr styled
// and the reverse. fang renders every help and error frame through these two
// streams. A terminal keeps its styled frame untouched.
func trimFramesOffTerminal(cmd *cobra.Command) {
	cmd.SetOut(trimIfNotTerminal(cmd.OutOrStdout()))
	cmd.SetErr(trimIfNotTerminal(cmd.ErrOrStderr()))
}
