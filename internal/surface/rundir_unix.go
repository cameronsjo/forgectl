//go:build unix

package surface

import "golang.org/x/sys/unix"

// closeFD releases a descriptor privdir handed us. It is split out per
// platform because privdir returns a raw descriptor rather than an *os.File —
// the pin only means anything as a descriptor, and wrapping it in an os.File
// would invite a finalizer to close it out from under a caller.
func closeFD(fd int) error { return unix.Close(fd) }
