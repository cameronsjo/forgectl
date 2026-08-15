// Package history parses a zsh history file into commands. It is the
// shell-history half of issue #26, and it is read-only by design: forgectl
// stays an exec-and-replace tool, so there is no shell shim, no
// `forgectl y shell-init`, and no postexec hook. Commands are recovered by
// re-reading $HISTFILE from disk on each call.
//
// A history file is untrusted input — it holds whatever the operator ever
// typed, including text pasted from elsewhere. Parsing therefore fails closed:
// a malformed, truncated, oversized, or absent file is a refusal, never an
// empty-but-confident result. The parser deliberately does not sanitize the
// commands it recovers; rendering them is the caller's job and goes through
// internal/termsafe, so exactly one notion of terminal-safe exists.
//
// Two zsh encodings are handled explicitly. The extended history format
// (`: <begintime>:<elapsed>;<command>`, written when EXTENDED_HISTORY is set)
// is parsed for its times; a file without those headers is read as plain
// commands with zero timestamps. Metafication — zsh stores any byte >= 0x80 as
// 0x83 followed by that byte XOR 0x20 — is decoded before the text is checked
// for UTF-8 validity.
package history

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// MaxFileBytes caps how large a history file may be before Read refuses it.
// A history file is an append-only log of one operator's typing; anything past
// this is either not a history file or has been tampered with, and either way
// reading it into memory is not something to do quietly.
const MaxFileBytes = 32 << 20

// zshMeta is zsh's metafication marker. The byte after it carries its real
// value XOR 0x20.
const zshMeta = 0x83

// histFileEnv is the environment variable zsh exports for its history file.
const histFileEnv = "HISTFILE"

// defaultHistoryName is the file zsh writes when $HISTFILE is unset.
const defaultHistoryName = ".zsh_history"

// extendedPrefix opens an EXTENDED_HISTORY record.
const extendedPrefix = ": "

// maxElapsedSeconds is the largest elapsed field that still fits a
// time.Duration once multiplied by a second.
const maxElapsedSeconds = int64(math.MaxInt64) / int64(time.Second)

var (
	// ErrNoHistory reports that the file parsed cleanly but held no commands.
	// It is a refusal rather than an empty slice so an absent or emptied
	// history can never read as "you have run nothing".
	ErrNoHistory = errors.New("history: no entries")
	// ErrMalformed reports a record this parser will not guess at.
	ErrMalformed = errors.New("history: malformed entry")
	// ErrTruncated reports a file that ends mid-record.
	ErrTruncated = errors.New("history: truncated file")
	// ErrEncoding reports bytes that are not UTF-8 once unmetafied.
	ErrEncoding = errors.New("history: unsupported encoding")
	// ErrTooLarge reports a file past MaxFileBytes.
	ErrTooLarge = errors.New("history: file too large")
	// ErrRange reports a non-positive entry count.
	ErrRange = errors.New("history: count out of range")
	// ErrPath reports a history path carrying terminal control characters.
	ErrPath = errors.New("history: unsafe path")
)

// Entry is one recovered history record.
type Entry struct {
	// Timestamp is the command's start time. Zero when the file is in zsh's
	// plain (non-extended) format, which carries no times.
	Timestamp time.Time
	// Elapsed is the command's recorded wall-clock duration, zero in plain
	// format.
	Elapsed time.Duration
	// Command is the command text, with zsh's escaped newlines restored. It is
	// untrusted: sanitize at the sink with termsafe before rendering it.
	Command string
}

// Parse decodes a zsh history file's bytes into entries, oldest first. Every
// failure path returns a nil slice, so a caller cannot accidentally act on a
// partial parse.
func Parse(data []byte) ([]Entry, error) {
	decoded, err := unmetafy(data)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(decoded) {
		return nil, fmt.Errorf("%w: not valid UTF-8 after unmetafying", ErrEncoding)
	}

	text := strings.TrimSuffix(string(decoded), "\n")
	if strings.TrimSpace(text) == "" {
		return nil, ErrNoHistory
	}

	var (
		entries []Entry
		folded  []string
		startAt int
	)
	for i, line := range strings.Split(text, "\n") {
		if len(folded) == 0 {
			startAt = i + 1
		}
		// zsh escapes a command's embedded newlines by ending the physical
		// line with a backslash, so a trailing backslash continues the record.
		if strings.HasSuffix(line, `\`) {
			folded = append(folded, strings.TrimSuffix(line, `\`))
			continue
		}
		folded = append(folded, line)
		entry, err := parseRecord(strings.Join(folded, "\n"), startAt)
		if err != nil {
			return nil, err
		}
		folded = nil
		entries = append(entries, entry)
	}
	if len(folded) > 0 {
		return nil, fmt.Errorf("%w: unterminated continuation at line %d", ErrTruncated, startAt)
	}
	if len(entries) == 0 {
		return nil, ErrNoHistory
	}
	return entries, nil
}

// parseRecord turns one logical record into an Entry. line is the record's
// first physical line number, reported so a refusal points at the damage.
func parseRecord(record string, line int) (Entry, error) {
	if record == "" {
		return Entry{}, fmt.Errorf("%w: blank record at line %d", ErrMalformed, line)
	}
	if !strings.HasPrefix(record, extendedPrefix) {
		return Entry{Command: record}, nil
	}

	header := record[len(extendedPrefix):]
	colon := strings.IndexByte(header, ':')
	if colon < 0 {
		return Entry{}, fmt.Errorf("%w: extended header has no elapsed field at line %d", ErrMalformed, line)
	}
	semicolon := strings.IndexByte(header[colon+1:], ';')
	if semicolon < 0 {
		return Entry{}, fmt.Errorf("%w: extended header has no command separator at line %d", ErrMalformed, line)
	}

	begin, err := parseUnsigned(header[:colon])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: unreadable start time at line %d", ErrMalformed, line)
	}
	elapsed, err := parseUnsigned(header[colon+1 : colon+1+semicolon])
	if err != nil {
		return Entry{}, fmt.Errorf("%w: unreadable elapsed time at line %d", ErrMalformed, line)
	}
	// Guard the seconds→Duration multiply: an absurd elapsed would otherwise
	// wrap into a negative duration rather than being rejected.
	if elapsed > maxElapsedSeconds {
		return Entry{}, fmt.Errorf("%w: elapsed time out of range at line %d", ErrMalformed, line)
	}

	command := header[colon+1+semicolon+1:]
	if command == "" {
		return Entry{}, fmt.Errorf("%w: entry has no command at line %d", ErrMalformed, line)
	}
	return Entry{
		Timestamp: time.Unix(begin, 0).UTC(),
		Elapsed:   time.Duration(elapsed) * time.Second,
		Command:   command,
	}, nil
}

// parseUnsigned reads a non-negative decimal field. Negatives are rejected
// rather than clamped: zsh never writes one, so its presence means the record
// was not written by zsh.
func parseUnsigned(field string) (int64, error) {
	n, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative value %d", n)
	}
	return n, nil
}

// unmetafy reverses zsh's metafication: 0x83 followed by b means b^0x20. A
// file ending on a lone marker was cut mid-byte and is refused.
func unmetafy(data []byte) ([]byte, error) {
	if bytes.IndexByte(data, zshMeta) < 0 {
		return data, nil
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] != zshMeta {
			out = append(out, data[i])
			continue
		}
		if i+1 >= len(data) {
			return nil, fmt.Errorf("%w: ends on an incomplete metafied byte", ErrTruncated)
		}
		i++
		out = append(out, data[i]^0x20)
	}
	return out, nil
}

// LastN returns the n most recent entries, oldest first, capped at what the
// slice holds. An empty slice refuses rather than returning nothing quietly.
func LastN(entries []Entry, n int) ([]Entry, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: count must be positive, got %d", ErrRange, n)
	}
	if len(entries) == 0 {
		return nil, ErrNoHistory
	}
	if n > len(entries) {
		n = len(entries)
	}
	tail := make([]Entry, n)
	copy(tail, entries[len(entries)-n:])
	return tail, nil
}

// Read loads and parses the history file at path. Every failure — absent,
// unreadable, a directory, oversized, or unparseable — is an error, and the
// path is quoted so a hostile filename cannot drive the reader's terminal.
func Read(path string) ([]Entry, error) {
	// Stat before opening: a $HISTFILE pointing at a fifo would block forever
	// in os.Open, so the kind check has to happen while nothing is open.
	if err := checkRegular(path, nil); err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read shell history: %w", termsafe.Error(err))
	}
	defer func() { _ = file.Close() }()

	// Re-check against the open handle: the path could have been swapped
	// between the stat above and this open.
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read shell history: %w", termsafe.Error(err))
	}
	if err := checkRegular(path, info); err != nil {
		return nil, err
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit", ErrTooLarge, termsafe.QuotePath(path), info.Size(), MaxFileBytes)
	}

	// Bounded by the same limit rather than trusting the size just read: the
	// file can grow between the stat and the read.
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read shell history: %w", termsafe.Error(err))
	}
	if int64(len(data)) > MaxFileBytes {
		return nil, fmt.Errorf("%w: %s grew past the %d-byte limit while being read", ErrTooLarge, termsafe.QuotePath(path), MaxFileBytes)
	}
	entries, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("read shell history %s: %w", termsafe.QuotePath(path), err)
	}
	return entries, nil
}

// ResolvePath returns $HISTFILE when set, else <home>/.zsh_history. Both
// sources are attacker-influenced in the general case — an environment
// variable and a home directory name — so a path carrying terminal control or
// bidi characters is refused rather than carried into an error message.
func ResolvePath(getenv func(string) string, home func() (string, error)) (string, error) {
	if getenv == nil || home == nil {
		return "", errors.New("resolve shell history path: getenv and home are both required")
	}
	if fromEnv := getenv(histFileEnv); fromEnv != "" {
		return checkedPath(fromEnv, "$"+histFileEnv)
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("resolve shell history path: $%s is unset and the home directory is unknown: %w", histFileEnv, termsafe.Error(err))
	}
	if dir == "" {
		return "", fmt.Errorf("resolve shell history path: $%s is unset and the home directory is empty", histFileEnv)
	}
	return checkedPath(filepath.Join(dir, defaultHistoryName), "the home directory")
}

// checkRegular refuses anything that is not a regular file — a directory, a
// fifo, a device node. info is stat'd from path when nil, so the same rule
// serves both the pre-open check and the re-check against the open handle.
func checkRegular(path string, info os.FileInfo) error {
	if info == nil {
		var err error
		if info, err = os.Stat(path); err != nil {
			return fmt.Errorf("read shell history: %w", termsafe.Error(err))
		}
	}
	if info.IsDir() {
		return fmt.Errorf("read shell history: %s is a directory", termsafe.QuotePath(path))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("read shell history: %s is not a regular file", termsafe.QuotePath(path))
	}
	return nil
}

// checkedPath refuses a path carrying terminal controls, naming only the
// source so the refusal itself cannot render the hostile bytes.
func checkedPath(path, source string) (string, error) {
	for _, r := range path {
		if termsafe.IsUnsafeTerminalRune(r) {
			return "", fmt.Errorf("%w: %s holds a terminal control character", ErrPath, source)
		}
	}
	return path, nil
}
