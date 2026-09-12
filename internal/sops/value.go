package sops

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// maxValueBytes bounds one scalar. SOPS documents hold credentials, not blobs;
// a value past this is a wrong-input mistake (a whole file piped in by
// accident), and bounding it keeps a multi-megabyte paste out of the editor
// round-trip.
const maxValueBytes = 64 << 10

// NormalizeValue applies the input rules to a raw value and returns the scalar
// that will be written.
//
// Every refusal names the rule and never the value — the same discipline
// internal/env's errInvalidKey follows, for the same reason: this function's
// whole input is a secret, so a message that quoted it would write the secret
// into stderr and the session transcript, which is the one outcome the feature
// exists to prevent.
//
// The control-character and UTF-8 refusals are not hygiene. YAML forbids C0
// control bytes outside tab and newline even inside a single-quoted scalar, so
// a value carrying one produces a document sops cannot parse — and a document
// sops cannot parse is the trigger for its unbounded editor re-invocation loop
// (measured on 3.13.3: 36,851 editor calls and 8.4 MB of stderr in three
// minutes, still going when it was killed). Refusing the byte here is the
// cheapest of the three brakes on that loop; __sops-edit's own YAML validation
// and its once-only counter are the other two.
func NormalizeValue(raw string) (string, error) {
	if len(raw) > maxValueBytes {
		return "", errors.New("value exceeds the 64KiB scalar ceiling")
	}

	value := StripOneTrailingNewline(raw)

	switch {
	case value == "":
		return "", errors.New("empty value; refusing to set an empty value — edit the file directly if intended")
	case !utf8.ValidString(value):
		return "", errors.New("value is not valid UTF-8")
	case strings.ContainsAny(value, "\n\r"):
		return "", errors.New("value must be a single line; a scalar cannot hold a newline")
	}

	for _, r := range value {
		// Tab is the one C0 byte YAML permits in a scalar, and a real
		// credential never carries it; DEL is not C0 but is equally
		// unrenderable, so it refuses alongside them.
		if (r < 0x20 && r != '\t') || r == 0x7F {
			return "", errors.New("value contains a control character")
		}
	}

	return value, nil
}

// StripOneTrailingNewline removes exactly one trailing "\n" or "\r\n" — what a
// piped value or a clipboard paste carries from the producing command's own
// line ending. Interior whitespace is never touched, and a value with no
// trailing newline (the interactive no-echo prompt) passes through unchanged.
//
// Exactly one, never a trim: a secret whose real last character is a newline is
// unusual but legal, and a greedy strip would silently corrupt it. This mirrors
// internal/env.StripTrailingNewline, which the .env path uses — the same rule,
// stated once per package rather than shared, because the packages are
// otherwise independent.
func StripOneTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

// encodeScalar renders value as a single-quoted YAML scalar.
//
// Single quotes are the right container because YAML performs NO escape
// processing inside them: a backslash is a backslash, a `#` is not a comment, a
// leading `*` is not an alias, and `: ` is not a mapping. The one character
// that needs handling is the quote itself, which YAML escapes by doubling.
// That makes this function total over every input NormalizeValue admits.
//
// Verified by round-trip rather than by inspection: the tests re-parse the
// emitted document with yaml.v3 and compare the decoded scalar, so they assert
// the value survives rather than restating this function's own output shape.
func encodeScalar(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
