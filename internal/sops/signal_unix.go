//go:build unix

package sops

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// terminateBySignal ends the process BY the caught signal rather than by an
// exit status that merely encodes it.
//
// The difference is visible to the parent. A shell running `forgectl env set`
// in a loop also receives the terminal's SIGINT, and whether it aborts the
// loop depends on whether the child DIED of SIGINT (WIFSIGNALED) or exited
// with a status of its choosing. exit(130) reads as "the child handled it",
// and the loop carries on to the next write. Re-raising after restoring the
// default disposition is the convention that keeps Ctrl-C meaning stop.
//
// The 128+N exit is the fallback, reached only if the re-raised signal did not
// take within the grace period.
func terminateBySignal(sig os.Signal) {
	if s, ok := sig.(syscall.Signal); ok {
		signal.Reset(sig)
		_ = syscall.Kill(syscall.Getpid(), s)
		time.Sleep(time.Second)
	}
	os.Exit(exitStatusFor(sig))
}
