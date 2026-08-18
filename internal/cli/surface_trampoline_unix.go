//go:build unix

package cli

import (
	"fmt"
	"os"
	osexec "os/exec"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// checkSocketOwner refuses a bootstrap socket that is not ours.
//
// This is the check that has no counterpart on the other side. The outer
// process proves the *directory* it creates is 0700 and pins it by descriptor;
// the trampoline arrives later, holding only a path typed by a terminal
// manager, and has to establish that the thing at the end of that path is the
// socket forgectl made rather than something substituted for it.
//
// Three properties, all of them necessary:
//
// Lstat, not Stat — following a symlink would authenticate the target's
// ownership while connecting through a link an attacker controls.
//
// It must be a socket. A named pipe or a regular file at that path is not
// something to dial, and the error from trying would name the path.
//
// It must be owned by this uid. That is what makes it forgectl's socket rather
// than one another account left where forgectl's was meant to be.
//
// The window between this check and the dial is not closed by it, and cannot be
// from a path — there is no bindat, so a path is all we have. It is narrowed on
// the other side instead: the socket lives in a 0700 directory, so entering it
// to swap the entry requires already being this user, and the peer-credential
// check on the connection is what covers the rest.
func checkSocketOwner(path string) error {
	// G703 flags the stat of a caller-supplied path. That is the function: it
	// exists to interrogate a path it does not trust, and refusing to look at
	// it would remove the check rather than satisfy it. The path is cleaned
	// before it arrives here, and nothing is opened until every property below
	// holds.
	//nolint:gosec // G703: interrogating an untrusted path is this guard's purpose
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %w", errSocketUnsafe, termsafe.Error(err))
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", errSocketUnsafe, termsafe.QuotePath(path))
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// A filesystem that cannot report ownership cannot establish that this
		// socket is ours, and a guard that cannot run its check has not passed
		// it.
		return fmt.Errorf("%w: ownership is unavailable for %s",
			errSocketUnsafe, termsafe.QuotePath(path))
	}
	if int(stat.Uid) != os.Geteuid() {
		// The owning uid is not named. It is not a secret, but reporting it
		// tells whoever planted the socket that their substitution was seen.
		return fmt.Errorf("%w: %s belongs to another account",
			errSocketUnsafe, termsafe.QuotePath(path))
	}
	return nil
}

// signalExitCode renders a signal death in the shell's 128+n convention.
func signalExitCode(exitErr *osexec.ExitError) int {
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 1
	}
	return 128 + int(status.Signal())
}
