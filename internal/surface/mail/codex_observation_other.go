//go:build !aix && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package mail

import "os"

func codexOwnedByCurrentUser(os.FileInfo) bool {
	return false
}
