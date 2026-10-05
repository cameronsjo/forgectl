//go:build !unix

package config

// WithFileLock has no portable implementation off Unix, so it is a
// deliberate no-op pass-through rather than a fail-closed refusal — see
// internal/env's lock_other.go for the same reasoning: this is a concurrency
// SERIALIZATION, not a security control, and goreleaser ships linux+darwin
// only (no Windows build), so fail-open here just keeps `go build`/`go test`
// usable on a contributor's non-unix machine rather than leaving a real gap
// in a shipped binary.
//
// That reasoning holds for callers that only want writes serialized. It does
// not hold for `herdr organize --apply`, where two unlocked runs against one
// session would fight; so that command checks cli's herdrLockSupported, which
// is false wherever this file builds, and refuses off Unix rather than
// relying on this lock (#732, #956).
func WithFileLock(_ string, fn func() error) error {
	return fn()
}

// WithFileLockNotify has no waiting to announce off Unix; it runs fn.
func WithFileLockNotify(_ string, _ func(), fn func() error) error {
	return fn()
}
