//go:build !unix

package gitenv

import "context"

// interruptible has nothing to pass on here: off unix a Runner starts git
// in no process group of its own (internal/exec's WithProcessGroup changes
// nothing), so a console's Ctrl-C reaches it as before.
func interruptible(ctx context.Context) (context.Context, func()) {
	return ctx, func() {}
}
