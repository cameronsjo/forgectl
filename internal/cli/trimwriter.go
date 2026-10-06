package cli

import (
	"io"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// trailingTrimWriter drops the spaces and tabs that end a line. fang renders
// help and error frames with a fixed-width style (120 columns off a terminal,
// whatever COLUMNS says), so every line of a redirected frame ends in padding
// (forgectl#1086). That padding is up to 46% of a verb's help page (`resume
// restart --help`: 12,275 to 6,646 bytes) but only 4% of the root page.
//
// It is for fang's own frames only. A command's data goes through the stream
// untouched: trailing whitespace can be data there (a TSV row that ends in an
// empty column). Install it for the length of one frame and call flush after.
//
// A run of whitespace is held until the next byte shows whether the line ends
// there. A color sequence (`ESC [ ... final`) after the padding is kept and
// the padding before it is dropped, so a styled frame trims too. flush
// writes out anything still held, so nothing is lost at the end of the stream.
type trailingTrimWriter struct {
	w io.Writer
	// held is every byte since the last visible one, in order: whitespace and
	// whole color sequences. keep is the color sequences alone.
	held, keep []byte
	// esc collects a color sequence that is still arriving; escOpen says it
	// has seen ESC, csi that it has seen the `[`.
	esc          []byte
	escOpen, csi bool
}

// Write reports len(p) on success, as io.Writer requires, not the count of
// bytes forwarded.
func (t *trailingTrimWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		out = t.step(out, b)
	}
	if len(out) > 0 {
		if _, err := t.w.Write(out); err != nil {
			return 0, err //nolint:wrapcheck // a writer must return the sink's error as is
		}
	}
	return len(p), nil
}

// step feeds one byte through the state machine and returns out with any
// bytes it releases appended.
func (t *trailingTrimWriter) step(out []byte, b byte) []byte {
	switch {
	case t.csi:
		t.esc = append(t.esc, b)
		if b >= 0x40 && b <= 0x7e { // the final byte ends the sequence
			t.held = append(t.held, t.esc...)
			t.keep = append(t.keep, t.esc...)
			t.esc, t.escOpen, t.csi = t.esc[:0], false, false
		}
		return out
	case t.escOpen:
		if b == '[' {
			t.esc = append(t.esc, b)
			t.csi = true
			return out
		}
		// Not a color sequence: the ESC is ordinary content.
		out = t.release(out)
		out = append(out, t.esc...)
		t.esc, t.escOpen = t.esc[:0], false
		return t.step(out, b)
	}
	switch b {
	case 0x1b:
		t.esc = append(t.esc[:0], b)
		t.escOpen = true
	case ' ', '\t':
		t.held = append(t.held, b)
	case '\n', '\r':
		out = append(out, t.keep...)
		t.held, t.keep = t.held[:0], t.keep[:0]
		out = append(out, b)
	default:
		out = t.release(out)
		out = append(out, b)
	}
	return out
}

// release appends everything held to out and clears it.
func (t *trailingTrimWriter) release(out []byte) []byte {
	out = append(out, t.held...)
	t.held, t.keep = t.held[:0], t.keep[:0]
	return out
}

// flush writes out what is still held, so a frame that ends without a newline
// loses nothing.
func (t *trailingTrimWriter) flush() error {
	out := t.release(nil)
	out = append(out, t.esc...)
	t.esc, t.escOpen, t.csi = t.esc[:0], false, false
	if len(out) == 0 {
		return nil
	}
	_, err := t.w.Write(out)
	return err //nolint:wrapcheck // a writer must return the sink's error as is
}

// isTerminalFd is a seam so a test can pin either answer without a pty.
var isTerminalFd = term.IsTerminal

// trimIfNotTerminal returns w wrapped in a trailingTrimWriter, or w itself and
// a nil writer when w is a terminal. A writer that is not a file (a buffer, a
// pipe wrapper) counts as not a terminal.
func trimIfNotTerminal(w io.Writer) (io.Writer, *trailingTrimWriter) {
	if f, ok := w.(term.File); ok && isTerminalFd(f.Fd()) {
		return w, nil
	}
	t := &trailingTrimWriter{w: w}
	return t, t
}

// trimHelpFrames makes every help page a command renders strip its padding off
// a terminal. fang installs its help function on the root alone, so each other
// command gets a wrapper that calls it with that command's stream swapped for
// a trimming one for the length of the call. The stream is the command's own,
// so a command's data output never passes through the trim, and each stream is
// judged on its own. The root's page is covered by trimRootHelp.
func trimHelpFrames(root *cobra.Command) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			sub.SetHelpFunc(func(c *cobra.Command, args []string) {
				// The root's function is fang's, installed once Execute runs.
				withTrimmedOut(c, func() { c.Root().HelpFunc()(c, args) })
			})
			walk(sub)
		}
	}
	walk(root)
}

// withTrimmedOut runs fn with c's stdout replaced by a trimming writer when
// stdout is not a terminal, then flushes and restores it.
func withTrimmedOut(c *cobra.Command, fn func()) {
	out := c.OutOrStdout()
	trimmed, t := trimIfNotTerminal(out)
	if t == nil {
		fn()
		return
	}
	c.SetOut(trimmed)
	defer func() {
		_ = t.flush()
		c.SetOut(out)
	}()
	fn()
}

// trimErrorFrame makes a fang error frame strip its padding when it is
// written to a stream that is not a terminal. fang hands the handler a
// colorprofile.Writer; its NoTTY profile is the not-a-terminal answer, and the
// trim sits below it so it sees the text after the color codes are stripped.
// The returned func flushes and must run after the frame is rendered.
func trimErrorFrame(w io.Writer) (done func()) {
	cw, ok := w.(*colorprofile.Writer)
	if !ok || cw.Profile != colorprofile.NoTTY {
		return func() {}
	}
	t := &trailingTrimWriter{w: cw.Forward}
	cw.Forward = t
	return func() { _ = t.flush() }
}
