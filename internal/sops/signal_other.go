//go:build !unix

package sops

import "os"

// terminateBySignal exits with the conventional 128+N status. A process
// cannot die BY a signal here the way it can on unix, so the status is the
// whole of the convention.
func terminateBySignal(sig os.Signal) {
	os.Exit(exitStatusFor(sig))
}
