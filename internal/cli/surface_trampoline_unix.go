//go:build unix

package cli

import (
	"os"
	osexec "os/exec"
	"syscall"
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
//
// Note what the uid comparison is and is not worth. Against the in-scope
// adversary — a hostile terminal manager, running as this same uid — it
// excludes nothing the 0700 directory does not already exclude. It is the
// second barrier for the case where that directory was created wrong, which is
// the only case where it does any work.
//
// Both operands are indirected so a test can drive them, for the same reason
// VerifyPeer's are: the refusal cannot be provoked honestly from a unit test —
// planting a foreign-owned socket needs a second account — so without a seam
// the suite is indifferent to whether the comparison exists at all.
var (
	lstatFn  = os.Lstat
	selfEUID = os.Geteuid
)

// Every refusal is the bare sentinel: this error reaches the pane's stderr, and
// which property failed is a fact about the socket the planter would otherwise
// learn for free.
func checkSocketOwner(path string) error {
	// G703 flags the stat of a caller-supplied path. That is the function: it
	// exists to interrogate a path it does not trust, and refusing to look at
	// it would remove the check rather than satisfy it. The path is cleaned
	// before it arrives here, and nothing is opened until every property below
	// holds.
	//nolint:gosec // G703: interrogating an untrusted path is this guard's purpose
	info, err := lstatFn(path)
	if err != nil {
		return errSocketUnsafe
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errSocketUnsafe
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// A filesystem that cannot report ownership cannot establish that this
		// socket is ours, and a guard that cannot run its check has not passed
		// it.
		return errSocketUnsafe
	}
	if int(stat.Uid) != selfEUID() {
		return errSocketUnsafe
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
