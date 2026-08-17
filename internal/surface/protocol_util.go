package surface

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
)

// newByteReader wraps a payload for the JSON decoder. It exists so decodeFrame
// reads a fixed slice rather than the connection: the length prefix has already
// bounded the read, and handing the decoder the socket would put the bound back
// in the decoder's hands.
func newByteReader(b []byte) io.Reader { return bytes.NewReader(b) }

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

// hasEqualsSign reports whether an environment entry has a name/value
// separator. An entry without one is not a variable, and exec would carry it
// into the child regardless.
func hasEqualsSign(entry string) bool { return strings.Contains(entry, "=") }
