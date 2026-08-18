package surface

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// lowerHexString reports whether s is non-empty and made only of 0-9a-f.
//
// Lowercase only, matching the reference decoder: forgectl encodes hex and
// encodes it lowercase, so accepting both cases would mean two spellings of one
// nonce — and two spellings is one more than a constant-time comparison can be
// written against.
func lowerHexString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isAbsPath reports whether p is absolute. It is a named wrapper rather than a
// bare filepath.IsAbs call so the protocol's requirements read as a list of
// checks rather than a mix of helpers and stdlib calls.
func isAbsPath(p string) bool { return filepath.IsAbs(p) }

// isEnvAssignment reports whether an environment entry is NAME=VALUE with a
// non-empty name.
//
// The separator alone is not the test: "=VALUE" carries one and names nothing,
// and exec passes it to the child as a variable with an empty name.
func isEnvAssignment(entry string) bool { return strings.Index(entry, "=") > 0 }

// containsNUL reports whether s carries a NUL byte.
//
// A NUL cannot survive into an argv or an environ — exec refuses it — but its
// refusal quotes the offending value, and bounded category-only errors are what
// keep the invocation out of a log.
func containsNUL(s string) bool { return strings.ContainsRune(s, 0) }

// refuseUnsafeRunes rejects a field carrying a rune that would be interpreted
// by a terminal rather than displayed.
//
// The refusal names the field and the byte offset, never the rune or the value
// — reporting what it found would print the escape sequence into the very log
// this is protecting.
func refuseUnsafeRunes(field, s string) error {
	for i, r := range s {
		if termsafe.IsUnsafeTerminalRune(r) {
			return fmt.Errorf("%w: %s carries a control character at byte %d", ErrProtocol, field, i)
		}
	}
	return nil
}

// truncate bounds an error string the peer had a hand in composing, marking the
// cut so a reader is never left believing they have the whole message.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "… (truncated)"
}
