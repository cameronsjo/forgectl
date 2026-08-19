// The non-unix half of the stat seam. It exists so the `//go:build unix`
// constraint on stat_unix.go is a real exclusion rather than a compile error
// deferred to the next symbol: without this file the package would fail to
// build on any other platform with "undefined: fillStat", which is exactly the
// confusing failure the constraint was added to prevent.
//
// Both stubs fail CLOSED, and that is the whole design. fillStat leaving Inode
// zero makes backend.Fingerprint refuse, so no reference can be built at all —
// rather than one whose digest would match across a server restart. ownerUID
// reporting ok=false makes the socket-directory check decline to assert
// ownership rather than quietly pass it.
//go:build !unix

package tmuxadapter

import (
	"os"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func ownerUID(os.FileInfo) (int, bool) { return 0, false }

func fillStat(*backend.IncarnationInput, os.FileInfo) {}
