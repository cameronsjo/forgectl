//go:build !unix

package cli

import (
	osexec "os/exec"
)

// checkSocketOwner refuses outright where ownership cannot be established.
//
// forgectl's release targets are Darwin and Linux; this file exists so the
// package compiles elsewhere. It refuses rather than returning nil, for the
// same reason the peer-credential stub does: a guard that waves everything
// through on a platform it does not understand is worse than no guard, because
// it reads as one.
func checkSocketOwner(string) error { return errSocketUnsafe }

// signalExitCode has no wait-status detail to read on this platform.
func signalExitCode(*osexec.ExitError) int { return 1 }
