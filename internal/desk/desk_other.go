//go:build !unix

package desk

import (
	"fmt"
	"os"
)

// RunSupervisor is unavailable off Unix: the desk is built on openat,
// O_NOFOLLOW, process groups and sessions.
func RunSupervisor(string, string) int {
	fmt.Fprintln(os.Stderr, "desk _supervise:", ErrUnsupported)
	return 2
}
