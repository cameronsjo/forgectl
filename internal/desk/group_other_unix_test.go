//go:build unix && !linux

package desk

import (
	"errors"

	"golang.org/x/sys/unix"
)

// groupHasLiveMember reports whether process group pgid still exists. With no
// /proc to tell a zombie from a live member, an unreaped zombie counts as a
// member; outside a container its new parent (launchd, init) reaps it at once.
func groupHasLiveMember(pgid int) bool {
	return !errors.Is(unix.Kill(-pgid, 0), unix.ESRCH)
}
