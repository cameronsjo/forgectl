//go:build !unix

package attention

import (
	"errors"
	"time"
)

var errUnsupported = errors.New("attention: not supported on this platform; it needs a Unix system")

// Spool is unavailable off Unix: it is built on openat and O_NOFOLLOW.
type Spool struct{}

// OpenSpool refuses off Unix.
func OpenSpool(string, bool) (*Spool, error) { return nil, errUnsupported }

// Path is empty off Unix.
func (*Spool) Path() string { return "" }

// Close does nothing off Unix.
func (*Spool) Close() error { return nil }

// Put refuses off Unix.
func (*Spool) Put(Record) error { return errUnsupported }

// Remove refuses off Unix.
func (*Spool) Remove(string) error { return errUnsupported }

// Get refuses off Unix.
func (*Spool) Get(string) (Record, bool, error) { return Record{}, false, errUnsupported }

// List refuses off Unix.
func (*Spool) List(time.Time, time.Duration, Liveness) ([]Record, int, error) {
	return nil, 0, errUnsupported
}

// EvictPane refuses off Unix.
func (*Spool) EvictPane(string, string, string) error { return errUnsupported }
