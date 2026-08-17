//go:build unix

package launch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// errUsageAbsent distinguishes "the namespace has never been created" from
// "the namespace exists and forgectl refused it". The reader and doctor treat
// the first as an empty store and the second as a problem worth reporting.
var errUsageAbsent = errors.New("launch usage namespace is absent")

// unsafeStore wraps a refusal reason under ErrUsageUnsafeStore. The reason is
// terminal-safe because doctor prints it.
func unsafeStore(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsageUnsafeStore, termsafe.SafeLine(fmt.Sprintf(format, args...)))
}

// pinUsageLeaf returns a descriptor on the fixed `forgectl` state leaf, with
// every check that descriptor's later use depends on already performed.
//
// The pin is the whole point: after this returns, lock and data are opened
// with openat against the returned fd, never by rebuilding a path string. An
// attacker who swaps the visible leaf for a symlink after this call still
// cannot redirect a single later operation, because no later operation
// consults the name again.
//
// The mechanism lives in internal/privdir, which this package used to own. It
// moved out when the surface coordinator needed the same guarantee for its
// private socket directory (forgectl#331) — a second hand-rolled copy of a
// filesystem race guard is how the copies drift apart. What stays here is the
// translation: privdir's vocabulary back into the two error identities the
// reader and doctor already branch on.
func pinUsageLeaf(base string, create bool) (int, error) {
	fd, err := privdir.Pin(privdir.Spec{
		Base:         base,
		Leaf:         usageLeafDir,
		Mode:         usageLeafMode,
		AncestorMode: usageAncestorMode,
		Create:       create,
	})
	switch {
	case errors.Is(err, privdir.ErrAbsent):
		return -1, errUsageAbsent
	case errors.Is(err, privdir.ErrUnsafe), errors.Is(err, privdir.ErrUnsupported):
		// Re-wrap rather than pass through: doctor and the reader branch on
		// ErrUsageUnsafeStore, and privdir deliberately does not know that
		// this store exists.
		return -1, unsafeStore("%s", strings.TrimPrefix(err.Error(), "privdir: "))
	case err != nil:
		return -1, unsafeStore("pin state leaf: %s", err)
	}
	return fd, nil
}
