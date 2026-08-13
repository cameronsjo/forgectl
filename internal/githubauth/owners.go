package githubauth

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// MaxOwners caps how many distinct owners one operation may enumerate.
const MaxOwners = 64

// MaxOwnerBytes caps a single owner value's length.
const MaxOwnerBytes = 256

// ErrLoginUnavailable is the safe sentinel returned when the authenticated
// GitHub.com login cannot be established.
var ErrLoginUnavailable = errors.New("authenticated GitHub login unavailable")

// ValidOwner reports whether owner is usable as a `gh` argv owner component.
func ValidOwner(owner string) bool { return false }

// ResolveOwners returns the owner set an operation should enumerate.
func ResolveOwners(ctx context.Context, run exec.Runner, configured []string) ([]string, error) {
	return nil, nil
}
