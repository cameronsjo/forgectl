//go:build !unix

package ready

import (
	"fmt"
	"os"
)

// openOverride refuses: the override's safety rests on O_NOFOLLOW and an
// owner check, which this platform does not offer. The built-in table still
// loads when no override file exists.
func openOverride(path string) (*os.File, os.FileInfo, error) {
	return nil, nil, fmt.Errorf("%w: %s: a predicate override is not supported on this platform", ErrTable, path)
}
