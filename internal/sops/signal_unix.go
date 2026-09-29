//go:build unix

package sops

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// guardedSignals are the catchable signals whose default in a Go program ends
// the process with every defer unrun, so a work directory would survive them.
//
//   - SIGINT: Ctrl-C.
//   - SIGTERM: the polite kill.
//   - SIGHUP: a closed terminal tab or a dropped SSH session.
//   - SIGQUIT: Ctrl-\. Go's default dumps goroutines and exits 2.
//   - SIGABRT: an ASYNCHRONOUS abort only — `kill -ABRT`, a watchdog. Go's
//     default is the same dump and exit 2 as SIGQUIT (measured on go1.26:
//     caught, Reset, re-raised, exit 2). signal.Notify never sees a
//     synchronous abort raised by a fault in this process, so a real crash
//     behaves exactly as it did before this entry existed.
//
// SIGTRAP, SIGSYS, SIGSEGV, SIGBUS and SIGFPE are not here, and neither are
// SIGKILL and SIGSTOP, which cannot be caught at all. A fault that kills the
// process runs no handler, so the work directory survives it. That residual is
// what the stale-leftover scan in internal/env covers: the next write to the
// same target refuses and names what was left behind.
//
// SIGUSR1, SIGUSR2, SIGALRM, SIGXCPU and SIGXFSZ are deliberately absent,
// though their kernel default terminates. The Go runtime installs its own
// handler for them and, with no Notify, DISCARDS them — measured on go1.26: a
// program that sends itself each one runs on, so the deferred cleanup still
// runs. Catching them here would turn a signal forgectl survives today into
// one that kills it.
var guardedSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGABRT}

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
// After signal.Reset the signal meets the Go runtime's own default, which is
// the kernel's for SIGINT, SIGTERM and SIGHUP (death by that signal) and, for
// SIGQUIT and SIGABRT, a goroutine dump and exit 2 (or a core-dumping SIGABRT
// under GOTRACEBACK=crash) —
// the same thing an unguarded forgectl does. The dump prints stack words, not
// string contents.
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
