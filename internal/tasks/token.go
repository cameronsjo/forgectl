// Package tasks is a read-only client for a Vikunja instance (tasks.sjo.lol),
// a local cache of what it returns, and the pure `ready` ranking logic layered
// on top. It is a plain library per internal/module's doc comment — only
// internal/cli wires it to a command surface.
//
// Every exported type that could carry the bearer token renders "[redacted]"
// from String, GoString, Format, LogValue, and MarshalJSON, mirroring
// internal/exec's SecretArg. The token is read once from the macOS login
// keychain, never appears in an argv, and is never written to the cache.
package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Redacted is the fixed rendering every token-carrying type in this package
// produces under every formatting and marshaling path. Never the payload.
const Redacted = "[redacted]"

// DefaultKeychainService is the login-keychain service name `tasks token`
// reads when no override is given. Configurable per the credential-handling
// contract: a caller may name a different service (e.g. for a second
// instance) without any code change.
const DefaultKeychainService = "vikunja-readonly"

// keychainTimeout bounds the `security find-generic-password` call. It is
// generous relative to a keychain read that succeeds instantly, because the
// case it exists for is one that never returns: a locked keychain or an ACL
// prompt with nobody at the console.
const keychainTimeout = 15 * time.Second

// tokenShape is a Vikunja API token: "tk_" followed by hex. Asserted before
// the value is ever sent anywhere, so a truncated keychain read produces a
// loud local error instead of a bare header and a misleading 401/403 that
// reads as a verdict about the server.
//
// Hex is matched case-insensitively: the digits mean the same thing in
// either case, and rejecting an uppercase copy would report "malformed
// token" — which reads as data corruption — for a value that is simply
// spelled differently.
var tokenShape = regexp.MustCompile(`^tk_[0-9a-fA-F]{40,}$`)

// Token is an opaque bearer credential. The payload lives behind a closure —
// not a plain string field — for the same reason internal/exec.SecretArg's
// does: fmt, slog's TextHandler, and encoding/json all reach a value through
// reflection only when it is NOT held behind an unexported field of a struct
// with no redacting method set of its own, and a func value has nothing for
// reflection to print but an address. Holding this type in an EXPORTED field
// of another struct with no Format/MarshalJSON of its own would still print
// verbatim under %+v — callers must hold a Token privately or route it
// through Header(), never expose it on a public struct field.
type Token struct {
	reveal func() string
}

// newToken wraps v. Unexported: the only way to mint a Token from outside
// this package is ReadToken, so a caller can never construct one from a
// string literal that skips shape validation.
func newToken(v string) Token { return Token{reveal: func() string { return v }} }

// Present reports whether a token was actually read (as opposed to the zero
// Token, which a caller might hold before ReadToken ever runs).
func (t Token) Present() bool { return t.reveal != nil && t.reveal() != "" }

// Header returns the literal Authorization header value ("Bearer tk_..."),
// the one sanctioned reveal point. Every caller sets this directly on an
// *http.Request; it must never be formatted into a log line, an error
// string, or a struct field that itself lacks these redacting methods.
func (t Token) Header() string {
	if t.reveal == nil {
		return ""
	}
	return "Bearer " + t.reveal()
}

func (Token) String() string                { return Redacted }
func (Token) GoString() string              { return Redacted }
func (Token) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (Token) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (Token) MarshalJSON() ([]byte, error)  { return []byte(strconv.Quote(Redacted)), nil }
func (Token) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

func writeRedacted(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprint(f, strconv.Quote(Redacted))
		return
	}
	_, _ = fmt.Fprint(f, Redacted)
}

// ErrTokenNotFound reports that the named keychain service has no entry.
var ErrTokenNotFound = fmt.Errorf("tasks: no token found in the login keychain")

// ErrTokenMalformed reports a keychain entry that does not look like a
// Vikunja API token (tk_<hex>). Refuse to send it anywhere rather than
// discover the truncation from a confusing 401.
var ErrTokenMalformed = fmt.Errorf("tasks: keychain entry does not look like a Vikunja API token")

// ErrTokenFileMode reports a token file readable by anyone but its owner.
// Refused rather than warned about: a group- or world-readable credential
// file works perfectly, so nothing downstream would ever surface it, and the
// container transport exists precisely to keep the token out of the two
// runtime readers (`docker inspect` and /proc/1/environ) that an env var
// exposes. A file every process on the host can read gives that back.
var ErrTokenFileMode = fmt.Errorf("tasks: token file is readable by more than its owner")

// maxTokenFileSize bounds the read. A token is ~50 bytes; anything near this
// ceiling is a wrong path (a log, a mounted directory's contents), and the
// bound is what stops the wrong path from being read into memory at all.
const maxTokenFileSize = 4096

// ReadTokenFile reads a bearer token from path — the container transport's
// credential source, where there is no login keychain to read.
//
// It is deliberately NOT an environment-variable constructor. An env var is
// readable through `docker inspect`, through /proc/<pid>/environ, and in the
// rendered compose file; a mounted file is the same disclosure class on disk
// but closes both runtime readers (ADR 0009 §7).
//
// Three refusals, all before the value could reach a request: a missing file,
// a value that is not shaped like a Vikunja API token, and a file whose mode
// grants read to group or other. None of the three names the value.
func ReadTokenFile(path string) (Token, error) {
	info, err := os.Stat(path)
	if err != nil {
		// os.Stat's error text carries the PATH, never the contents, so it is
		// safe to include — and the path is what an operator needs in order
		// to fix a wrong mount.
		return Token{}, fmt.Errorf("%w: %s", ErrTokenNotFound, termsafe.QuotePath(path))
	}
	if info.IsDir() {
		// Docker creates an empty DIRECTORY at a bind-mount source that does
		// not exist on the host, so this is the shape a missing secret takes
		// in the deployment this function serves — not a hypothetical.
		return Token{}, fmt.Errorf("%w: %s is a directory, not a file — a bind mount whose source was missing on the host",
			ErrTokenNotFound, termsafe.QuotePath(path))
	}
	if !info.Mode().IsRegular() {
		// The size bound below is derived from Stat, and Stat reports size 0
		// for a FIFO, a device node, and a procfs entry — so a non-regular
		// file sails past the ceiling and os.ReadFile then reads unbounded,
		// or blocks forever on a FIFO with no writer. That last case hangs
		// startup BEFORE the listener opens, which presents as a container
		// that never becomes healthy rather than as a bad credential path.
		return Token{}, fmt.Errorf("%w: %s is not a regular file (mode %s)",
			ErrTokenNotFound, termsafe.QuotePath(path), info.Mode().Type())
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return Token{}, fmt.Errorf("%w: %s is mode %04o, want 0400 or 0600", ErrTokenFileMode, termsafe.QuotePath(path), mode)
	}
	if info.Size() > maxTokenFileSize {
		return Token{}, fmt.Errorf("%w: %s is %d bytes — a token file is under %d",
			ErrTokenMalformed, termsafe.QuotePath(path), info.Size(), maxTokenFileSize)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied flag value, stat-checked above; this IS the credential source
	if err != nil {
		return Token{}, fmt.Errorf("%w: %s", ErrTokenNotFound, termsafe.QuotePath(path))
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return Token{}, fmt.Errorf("%w: %s is empty", ErrTokenNotFound, termsafe.QuotePath(path))
	}
	if !tokenShape.MatchString(value) {
		// Names the path, never the value — the malformed value is still a
		// credential, and this error string reaches a startup log.
		return Token{}, fmt.Errorf("%w: %s", ErrTokenMalformed, termsafe.QuotePath(path))
	}
	return newToken(value), nil
}

// ReadToken reads service's value from the macOS login keychain via
// `security find-generic-password -s <service> -w`. The value travels on
// the child's stdout, never on argv — service is a fixed, non-secret
// identifier, and runner.Run's own argv logging therefore never touches the
// credential. The returned Token's payload is revealed exactly once, into
// this closure; nothing above this function ever sees the raw string.
func ReadToken(ctx context.Context, runner exec.Runner, service string) (Token, error) {
	// Bounded on purpose. A locked keychain, or an item whose ACL demands
	// interactive confirmation, makes `security` block on a GUI prompt with
	// no deadline of its own — so an unbounded call here hangs the command
	// forever while every network call in this package is capped at 10s.
	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()

	out, err := runner.Run(ctx, "security", "find-generic-password", "-s", service, "-w")
	if err != nil {
		// err is DELIBERATELY dropped rather than wrapped. exec.CommandError
		// retains the child's stdout in its exported Output field, and this
		// child's stdout is the bearer token — a nonzero exit does not mean
		// stdout was empty. Wrapping it to "improve the error context" would
		// put the credential into any error string, log line, or %+v that
		// ever renders this error. Do not add %w here.
		return Token{}, fmt.Errorf("%w: service %q", ErrTokenNotFound, service)
	}
	value := strings.TrimSpace(out)
	if value == "" {
		return Token{}, fmt.Errorf("%w: service %q", ErrTokenNotFound, service)
	}
	if !tokenShape.MatchString(value) {
		// Names the service but never the value — an operator running two
		// instances needs to know WHICH entry is bad, and the malformed
		// value itself is still a credential.
		return Token{}, fmt.Errorf("%w: service %q", ErrTokenMalformed, service)
	}
	return newToken(value), nil
}
