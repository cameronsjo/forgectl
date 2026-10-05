package desk

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Output normalization and the private-step redaction table, ported from
// run-and-watch. This is NOT internal/redact: that package masks URL and
// credential shapes, while this one replaces the exact values a `private`
// step wrote to STEP_OUT. Redaction reduces exposure; it is not a guarantee —
// a value printed split across lines, encoded, or transformed gets through.

var (
	oscRe = regexp.MustCompile(`(?:\x1b\]|\x{9d})[^\x07\x1b\x{9c}]*(?:\x07|\x1b\\|\x{9c})?`)
	csiRe = regexp.MustCompile(`(?:\x1b\[|\x{9b})[0-?]*[ -/]*[@-~]`)
	escRe = regexp.MustCompile(`\x1b[@-Z\\\]^_]?`)
	// C0 and C1 controls (keeping tab; lines arrive without "\n"), plus the
	// invisible format characters that split a printed value without changing
	// how it looks.
	ctrlRe = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f-\x{9f}\x{ad}\x{34f}\x{180e}\x{200b}-\x{200f}\x{2028}-\x{202e}` +
		`\x{2060}-\x{2064}\x{2066}-\x{2069}\x{3164}\x{fe00}-\x{fe0f}\x{feff}\x{e0000}-\x{e007f}]`)
	markRe = regexp.MustCompile(`<redacted:OUT_[a-z][a-z0-9]*_[A-Za-z][A-Za-z0-9_]*>`)
)

// isEscapeStart reports the characters the first stage of Clean keeps (ESC,
// CSI, OSC), so the later stages can still recognize the sequence they begin.
func isEscapeStart(s string) bool {
	const esc, csi, osc = 0x1b, 0x9b, 0x9d
	r, size := utf8.DecodeRuneInString(s)
	return size == len(s) && (r == esc || r == csi || r == osc)
}

// redactMin is the shortest value the table replaces. Shorter values (a "1",
// a "yes") would shred ordinary output.
const redactMin = 6

// Normalize strips OSC/CSI/other escapes, then control and invisible
// characters. It runs before redaction, so a value split by color codes is
// rejoined and still caught.
func Normalize(text string) string {
	text = oscRe.ReplaceAllString(text, "")
	text = csiRe.ReplaceAllString(text, "")
	text = escRe.ReplaceAllString(text, "")
	return ctrlRe.ReplaceAllString(text, "")
}

// Redactor replaces private-step output values of six or more characters
// with `<redacted:OUT_<step>_<key>>`. The zero value is an empty table.
type Redactor struct {
	table []redaction
}

type redaction struct {
	value, mark string
}

// Add records a private step's outputs. Values are normalized first because
// output lines are: a value carrying "\r" or an escape would otherwise never
// match its printed form.
func (r *Redactor) Add(stepID string, outputs map[string]string) {
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if nv := Normalize(outputs[k]); len([]rune(nv)) >= redactMin {
			r.table = append(r.table, redaction{value: nv, mark: fmt.Sprintf("<redacted:OUT_%s_%s>", stepID, k)})
		}
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.SliceStable(r.table, func(i, j int) bool {
		return len([]rune(r.table[i].value)) > len([]rune(r.table[j].value))
	})
}

// Apply replaces every table value in text.
func (r *Redactor) Apply(text string) string {
	for _, e := range r.table {
		text = strings.ReplaceAll(text, e.value, e.mark)
	}
	return text
}

// Clean normalizes and redacts one output line. Stripping an escape can join a
// split value or swallow its first character, depending on the order of the
// strips, so redaction runs after each stage: plain controls, then OSC/CSI
// sequences, then the rest. Marks placed by an earlier stage are never touched
// by a later one.
func Clean(r *Redactor, text string) string {
	stages := []func(string) string{
		func(t string) string {
			return ctrlRe.ReplaceAllStringFunc(t, func(m string) string {
				if isEscapeStart(m) {
					return m
				}
				return ""
			})
		},
		func(t string) string { return csiRe.ReplaceAllString(oscRe.ReplaceAllString(t, ""), "") },
		Normalize,
	}
	parts := []string{text} // odd indexes are marks
	for _, stage := range stages {
		var b strings.Builder
		for i, p := range parts {
			if i%2 == 1 {
				b.WriteString(p)
			} else {
				b.WriteString(r.Apply(stage(p)))
			}
		}
		parts = splitKeepMarks(b.String())
	}
	return strings.Join(parts, "")
}

// splitKeepMarks splits s around redaction marks, keeping each mark at an odd
// index (re.split with a capturing group).
func splitKeepMarks(s string) []string {
	locs := markRe.FindAllStringIndex(s, -1)
	out := make([]string, 0, 2*len(locs)+1)
	prev := 0
	for _, l := range locs {
		out = append(out, s[prev:l[0]], s[l[0]:l[1]])
		prev = l[1]
	}
	return append(out, s[prev:])
}

// ParseOutputs reads a STEP_OUT file: key=value lines, the last value for a
// key winning. Warnings name the line number, never its content, because the
// content may be a secret.
func ParseOutputs(data []byte) (outs map[string]string, keys []string, warns []string) {
	outs = map[string]string{}
	for i, raw := range splitLines(strings.ToValidUTF8(string(data), "\uFFFD")) {
		n := i + 1
		if strings.TrimSpace(raw) == "" {
			continue
		}
		key, value, found := strings.Cut(raw, "=")
		switch {
		case !found:
			warns = append(warns, fmt.Sprintf("STEP_OUT line %d is not key=value; dropped", n))
		case !outKeyRe.MatchString(key):
			warns = append(warns, fmt.Sprintf("STEP_OUT line %d has a bad key; dropped (keys match %s)", n, outKeyRe))
		case strings.ContainsRune(value, 0):
			warns = append(warns, fmt.Sprintf("STEP_OUT line %d (%s) contains NUL; dropped", n, key))
		default:
			if _, seen := outs[key]; !seen {
				keys = append(keys, key)
			}
			outs[key] = value
		}
	}
	return outs, keys, warns
}

// splitLines splits on the line boundaries Python's str.splitlines uses, so a
// "\r\n" file yields clean values. A trailing boundary makes no empty line.
func splitLines(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			out = append(out, string(rs[start:i]))
			start = i + 1
		case '\r':
			out = append(out, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}
