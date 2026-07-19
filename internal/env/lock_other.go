//go:build !unix

package env

// withFileLock has no portable implementation off Unix, so it is a
// deliberate no-op pass-through rather than a fail-closed refusal: unlike
// bless's ownership check, this is a concurrency SERIALIZATION, not a
// security control, and goreleaser ships linux+darwin only (no Windows
// build), so fail-open here just keeps `go build`/`go test` usable on a
// contributor's non-unix machine rather than leaving a real gap in a
// shipped binary.
func withFileLock(_ string, fn func() error) error {
	return fn()
}
