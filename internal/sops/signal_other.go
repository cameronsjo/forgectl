//go:build !unix

package sops

import (
	"os"
	"syscall"
)

// guardedSignals off unix: os/signal delivers os.Interrupt for Ctrl-C and
// Ctrl-Break, and SIGTERM for a console close, logoff or shutdown.
var guardedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// terminateBySignal exits with the conventional 128+N status. A process
// cannot die BY a signal here the way it can on unix, so the status is the
// whole of the convention.
func terminateBySignal(sig os.Signal) {
	os.Exit(exitStatusFor(sig))
}
