package tasks

import (
	"errors"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Sentinels for errors.Is. A caller (the CLI layer) matches on these to pick
// a distinct process exit code and to decide whether a stale cache may be
// served — ErrUnreachable may fall back to cache (with its age stated);
// ErrUnauthorized never may, because a revoked token must fail loudly rather
// than silently reading data that may no longer be current.
var (
	// ErrUnreachable means the request could not even reach the server:
	// DNS failure, connection refused, or a timeout. Distinct from
	// ErrUnauthorized so a caller never confuses "the box is down" with
	// "the credential is bad".
	ErrUnreachable = errors.New("tasks: could not reach the Vikunja instance")

	// ErrUnauthorized means the server was reached and rejected the
	// credential (401/403) on a request that should have succeeded.
	ErrUnauthorized = errors.New("tasks: the Vikunja instance rejected the credential")

	// ErrUnexpectedStatus means the server responded with a status this
	// client did not expect on a read — neither success nor an auth
	// rejection. Vikunja returns a JSON object (not the expected array) on
	// this path, so decoding is refused before it can be attempted.
	ErrUnexpectedStatus = errors.New("tasks: unexpected response status")
)

// malformedJSON is the categorical refusal of a server response that does not
// decode (#761). A *json.SyntaxError quotes a character of server text and an
// *json.UnmarshalTypeError names server-chosen fields, so the message is fixed
// text plus where, which the caller builds from code-owned paths and ids only.
// The chain carries both ErrUnexpectedStatus, for the CLI's exit-code
// disposition, and the decode error, for errors.As.
func malformedJSON(where string, err error) error {
	return termsafe.Categorical(
		ErrUnexpectedStatus.Error()+": "+where+": the tasks server returned malformed JSON",
		errors.Join(ErrUnexpectedStatus, err))
}

// IsHostRefused reports whether err (or anything it wraps) is the host-pinning
// refusal. It is deliberately NOT folded into ErrUnreachable: "I declined to
// send the credential here" and "the box is down" call for different operator
// responses, and a caller that cannot tell them apart cannot alert on the
// security one.
func IsHostRefused(err error) bool { return errors.Is(err, ErrHostRefused) }
