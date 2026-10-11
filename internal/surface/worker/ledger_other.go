//go:build !unix

package worker

import "github.com/cameronsjo/forgectl/internal/privdir"

// The ledger's safety rests on openat and O_NOFOLLOW, which this platform
// does not offer, so it refuses rather than writing without them.
type unsupportedStore struct{}

func newFileStore(string, string) store { return unsupportedStore{} }

func (unsupportedStore) read() ([]byte, error) { return nil, privdir.ErrUnsupported }

func (unsupportedStore) update(func([]byte) ([]byte, error)) error { return privdir.ErrUnsupported }

func listLedgersAt(string) ([]LedgerID, []string, error) { return nil, nil, privdir.ErrUnsupported }
