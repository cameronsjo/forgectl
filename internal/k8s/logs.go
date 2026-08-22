// Package k8s implements focused Kubernetes helpers without inventing a
// deployment-manifest abstraction. The caller supplies ordinary kubectl argv.
package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Level is an ordered log severity. Only recognized top-level JSON level or
// severity fields participate in filtering; banners and plain-text lines pass
// through so a floor never hides operational context.
type Level uint8

const (
	LevelTrace Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// ParseLevel parses the user-facing severity vocabulary.
func ParseLevel(value string) (Level, error) {
	switch strings.ToLower(value) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error", "err":
		return LevelError, nil
	case "fatal", "panic":
		return LevelFatal, nil
	default:
		return 0, fmt.Errorf("unknown log level %s (want trace, debug, info, warn, error, or fatal)", termsafe.QuoteText(value))
	}
}

// LogsOptions controls the bounded line transformer.
type LogsOptions struct {
	MinLevel Level
	Color    bool
}

// Client streams kubectl logs through terminal-safe line transforms.
type Client struct {
	runner forgexec.StreamingRunner
}

func New(runner forgexec.StreamingRunner) *Client { return &Client{runner: runner} }

// Logs invokes exactly one `kubectl logs` process. args are appended
// structurally, never joined into a shell command.
func (c *Client) Logs(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string, opts LogsOptions) error {
	if c.runner == nil {
		return errors.New("k8s streaming runner is unavailable")
	}
	if len(args) == 0 {
		return errors.New("kubectl logs requires a resource or selector argument")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	logOut := newBoundedLineWriter(stdout, logLineTransform(stdout, opts))
	safeErr := newBoundedLineWriter(stderr, safeLineTransform(stderr))
	kubectlArgs := append([]string{"logs"}, args...)
	runErr := c.runner.RunStreaming(ctx, stdin, logOut, safeErr, "kubectl", kubectlArgs...)
	stdoutErr := logOut.Close()
	stderrErr := safeErr.Close()
	if stdoutErr != nil {
		return fmt.Errorf("write kubectl logs: %w", stdoutErr)
	}
	if stderrErr != nil {
		return fmt.Errorf("write kubectl diagnostics: %w", stderrErr)
	}
	return runErr
}

// maxBufferedLine bounds memory while retaining normal JSON lines for parsing.
// A larger line is treated as unrecognized and streamed safely in chunks.
const maxBufferedLine = 256 << 10

type lineTransform func(line []byte, newline bool) error

type boundedLineWriter struct {
	out       io.Writer
	transform lineTransform
	line      []byte
	oversized *safeFragmentWriter
	err       error
}

func newBoundedLineWriter(out io.Writer, transform lineTransform) *boundedLineWriter {
	return &boundedLineWriter{out: out, transform: transform, line: make([]byte, 0, 4096)}
}

func (w *boundedLineWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	written := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		fragment := p
		newline := false
		if i >= 0 {
			fragment = p[:i]
			p = p[i+1:]
			newline = true
		} else {
			p = nil
		}
		if err := w.append(fragment); err != nil {
			w.err = err
			return 0, err
		}
		if newline {
			if err := w.finishLine(true); err != nil {
				w.err = err
				return 0, err
			}
		}
	}
	return written, nil
}

func (w *boundedLineWriter) append(fragment []byte) error {
	if w.oversized != nil {
		return w.oversized.WriteFragment(fragment, false)
	}
	if len(w.line)+len(fragment) <= maxBufferedLine {
		w.line = append(w.line, fragment...)
		return nil
	}

	// The line is too large to retain for JSON parsing. From here to its
	// newline it is an unrecognized line and therefore passes through, but via
	// a rune-aware sanitizer so chunks cannot inject terminal controls.
	w.oversized = &safeFragmentWriter{out: w.out}
	if err := w.oversized.WriteFragment(w.line, false); err != nil {
		return err
	}
	w.line = w.line[:0]
	return w.oversized.WriteFragment(fragment, false)
}

func (w *boundedLineWriter) finishLine(newline bool) error {
	if w.oversized != nil {
		if err := w.oversized.Finish(); err != nil {
			return err
		}
		w.oversized = nil
		if newline {
			_, err := io.WriteString(w.out, "\n")
			return err
		}
		return nil
	}
	err := w.transform(w.line, newline)
	w.line = w.line[:0]
	return err
}

func (w *boundedLineWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.oversized == nil && len(w.line) == 0 {
		return nil
	}
	return w.finishLine(false)
}

// safeFragmentWriter preserves valid UTF-8 across arbitrary process-write
// boundaries while making each emitted fragment terminal-inert.
type safeFragmentWriter struct {
	out   io.Writer
	carry []byte
}

func (w *safeFragmentWriter) WriteFragment(p []byte, final bool) error {
	data := append(append(make([]byte, 0, len(w.carry)+len(p)), w.carry...), p...)
	w.carry = w.carry[:0]
	if !final {
		if held := incompleteUTF8Suffix(data); held > 0 {
			w.carry = append(w.carry, data[len(data)-held:]...)
			data = data[:len(data)-held]
		}
	}
	if len(data) == 0 {
		return nil
	}
	_, err := io.WriteString(w.out, termsafe.SafeLine(string(data)))
	return err
}

func (w *safeFragmentWriter) Finish() error { return w.WriteFragment(nil, true) }

func incompleteUTF8Suffix(data []byte) int {
	start := len(data) - 1
	limit := len(data) - utf8.UTFMax
	if limit < 0 {
		limit = 0
	}
	for start >= limit {
		if utf8.RuneStart(data[start]) {
			if !utf8.FullRune(data[start:]) {
				return len(data) - start
			}
			return 0
		}
		start--
	}
	return 0
}

func safeLineTransform(out io.Writer) lineTransform {
	return func(line []byte, newline bool) error {
		if _, err := io.WriteString(out, termsafe.SafeLine(string(line))); err != nil {
			return err
		}
		if newline {
			_, err := io.WriteString(out, "\n")
			return err
		}
		return nil
	}
}

func logLineTransform(out io.Writer, opts LogsOptions) lineTransform {
	return func(line []byte, newline bool) error {
		level, recognized := jsonSeverity(line)
		if recognized && level < opts.MinLevel {
			return nil
		}
		safe := termsafe.SafeLine(string(line))
		if opts.Color && recognized {
			safe = severityANSI(level) + safe + "\x1b[0m"
		}
		if _, err := io.WriteString(out, safe); err != nil {
			return err
		}
		if newline {
			_, err := io.WriteString(out, "\n")
			return err
		}
		return nil
	}
}

func jsonSeverity(line []byte) (Level, bool) {
	var fields struct {
		Level    json.RawMessage `json:"level"`
		Severity json.RawMessage `json:"severity"`
	}
	if err := json.Unmarshal(line, &fields); err != nil {
		return 0, false
	}
	raw := fields.Level
	if len(raw) == 0 {
		raw = fields.Severity
	}
	if len(raw) == 0 {
		return 0, false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	level, err := ParseLevel(value)
	return level, err == nil
}

func severityANSI(level Level) string {
	switch level {
	case LevelTrace:
		return "\x1b[90m"
	case LevelDebug:
		return "\x1b[36m"
	case LevelInfo:
		return "\x1b[32m"
	case LevelWarn:
		return "\x1b[33m"
	case LevelError:
		return "\x1b[31m"
	case LevelFatal:
		return "\x1b[1;31m"
	default:
		return ""
	}
}
