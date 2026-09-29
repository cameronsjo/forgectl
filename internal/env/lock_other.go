//go:build !unix

package env

// withFileLock has no portable implementation off Unix, so it is a
// deliberate no-op pass-through rather than a fail-closed refusal: unlike
// bless's ownership check, this is a concurrency SERIALIZATION, not a
// security control, and goreleaser ships linux+darwin only (no Windows
// build), so fail-open here just keeps `go build`/`go test` usable on a
// contributor's non-unix machine rather than leaving a real gap in a
// shipped binary.
//
// The leftover scan still runs, so the refusal behaves the same everywhere.
// With no lock it cannot rule out a concurrent run's live scratch, but it only
// ever refuses, so the worst case is a false refusal.
func withFileLock(t Target, fn func() error) error {
	if err := scanLeftovers(t); err != nil {
		return err
	}
	return fn()
}
