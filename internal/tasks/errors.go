package tasks

import "errors"

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
