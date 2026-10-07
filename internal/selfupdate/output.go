package selfupdate

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	// captureLimit bounds what a step's capture keeps. Past it the oldest
	// bytes go, because the tail is what explains a failure.
	captureLimit = 1 << 20
	// maxLineBytes bounds one pending line in a safeWriter, so a child that
	// never prints a newline cannot grow it without limit.
	maxLineBytes = 8 << 10
	// maxLineRunes caps one rendered line.
	maxLineRunes = 400
)

// captureBuffer keeps the last captureLimit bytes written to it. Safe for
// concurrent use.
type captureBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (c *captureBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if over := len(c.buf) - captureLimit; over > 0 {
		c.buf = append(c.buf[:0], c.buf[over:]...)
	}
	return len(p), nil
}

func (c *captureBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// safeWriter shows untrusted output on a terminal: it splits it into lines,
// redacts each (a credential-shaped word withholds the line), and escapes
// control and bidi characters, so brew's text cannot move the cursor, retitle
// the window, or forge a prompt. A carriage return ends a line like a newline,
// so a progress bar cannot overwrite what is already on screen.
type safeWriter struct {
	mu  sync.Mutex
	out io.Writer
	buf []byte
}

func newSafeWriter(out io.Writer) *safeWriter { return &safeWriter{out: out} }

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexAny(s.buf, "\r\n")
		if i < 0 {
			break
		}
		line, blankOK := s.buf[:i], s.buf[i] == '\n'
		rest := s.buf[i+1:]
		if s.buf[i] == '\r' && len(rest) > 0 && rest[0] == '\n' {
			rest, blankOK = rest[1:], true
		}
		s.emit(string(line), blankOK)
		s.buf = append(s.buf[:0], rest...)
	}
	if len(s.buf) > maxLineBytes {
		s.emit(string(s.buf), false)
		s.buf = s.buf[:0]
	}
	return len(p), nil
}

// Flush writes any pending partial line.
func (s *safeWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		s.emit(string(s.buf), false)
		s.buf = s.buf[:0]
	}
}

func (s *safeWriter) emit(line string, blankOK bool) {
	if line == "" && !blankOK {
		return
	}
	_, _ = io.WriteString(s.out, safeText(line)+"\n")
}

// safeText is the one rendering of an untrusted brew line: redact first, then
// escape, then cap (a cap that cut first could split a credential shape).
func safeText(line string) string {
	return termsafe.SafeLineMax(redact.Text(line), maxLineRunes)
}

// Tail returns the last n non-empty lines of brew output, each redacted and
// escaped, joined by newlines; "" when there are none. brew's text is
// untrusted (it relays what the tap's server and git transport send), so this
// is the only way it reaches a terminal or a JSON field from here.
func Tail(output string, n int) string {
	var lines []string
	for _, l := range strings.FieldsFunc(output, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = safeText(l)
	}
	return strings.Join(lines, "\n")
}
