//go:build !unix

package sweep

import (
	"errors"

	"github.com/cameronsjo/forgectl/internal/attention"
)

// Write is not supported off Unix: the ledger is written through openat.
func Write(string, []byte) error {
	return errors.New("sweep: writing the ledger needs a Unix system")
}

// Path is where Write puts the ledger, for messages.
func Path(stateBase string) string { return attention.SweepPath(stateBase) }
