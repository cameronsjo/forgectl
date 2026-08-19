// The non-unix half of the stat seam. It exists so the `//go:build unix`
// constraint on sockstat_unix.go is a real exclusion rather than a compile
// error deferred to the next symbol: without this file the package would fail
// to build on any other platform with "undefined: fill", which is exactly the
// confusing failure the constraint was added to prevent.
//
// Both stubs fail closed; the package doc says what each failure buys.
//go:build !unix

package sockstat

import (
	"os"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func ownerUID(os.FileInfo) (int, bool) { return 0, false }

func fill(*backend.IncarnationInput, os.FileInfo) {}
