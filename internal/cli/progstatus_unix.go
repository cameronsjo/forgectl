// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/progstatus"
)

// reportOnSignal installs a scoped SIGINT/SIGTERM handler that writes an error
// report for name, then restores the default disposition and re-raises the
// signal, so forgectl still dies by it exactly as before. It deliberately does
// not touch any context: forgectl#788 declined a root signal context, and
// children still get the terminal's Ctrl-C from the process group. The returned
// stop func removes the handler on the normal return path. With status off it
// installs nothing.
func reportOnSignal(r *progstatus.Reporter, name string) (stop func()) {
	if !r.Enabled() {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			r.Error(name + " interrupted")
			signal.Reset(sig)
			_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
