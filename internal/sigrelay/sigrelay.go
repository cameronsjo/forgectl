// Package sigrelay runs a non-interactive child in a process group of its own
// without losing the terminal's Ctrl-C.
//
// A child in its own group can be killed whole when its deadline passes, with
// every helper it forked (internal/exec's WithProcessGroup). But the terminal
// sends Ctrl-C to the foreground group only, which is forgectl's, so the
// child no longer gets it: forgectl dies and the child runs on. Group passes
// the signal on.
//
// It is a package of its own, not part of internal/gitenv, so a caller that
// runs no git (audit secrets' gitleaks) can use it without importing gitenv
// (#1020).
package sigrelay

import (
	"context"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// Group returns ctx bounded by d, under which each child a Runner starts
// runs in a process group of its own that the deadline kills whole. A
// terminating signal that reaches forgectl while the context is live
// cancels it, which kills the child's group, and cancel then re-raises the
// signal, so forgectl stops as it did when the child shared its group rather
// than carrying on to the next one. The caller defers cancel.
func Group(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithTimeout(ctx, d)
	ictx, stop := interruptible(dctx)
	return fexec.WithProcessGroup(ictx), func() {
		stop()
		cancel()
	}
}
