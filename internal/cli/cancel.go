// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// ExitCancelled is the exit code for a prompt the user backed out of: Esc or
// Ctrl+C in a picker, or No at a confirm. It is the shell's 128+SIGINT
// convention, so it is neither success (a script can tell "killed" from
// "declined") nor the generic failure 1 (backing out is not an error).
const ExitCancelled = 130

// userCancelled records that a prompt ended in a cancel. Execute turns it
// into ExitCancelled once the command tree has returned.
//
// It is a flag and not a returned error on purpose. Every error the command
// tree returns goes through fang, and fang queries the terminal for its
// background colour (OSC 11 + DA1) before it renders one — even an error that
// renders nothing. A terminal that never answers costs about 4 s of that
// query, which is the stall forgectl#1099 reported on every picker exit. A
// cancel that returns nil never reaches fang's error path.
var userCancelled atomic.Bool

// noteCancelled prints the plain `cancelled` line and records the cancel. It
// returns nil so a command can `return noteCancelled(out)`.
func noteCancelled(w io.Writer) error {
	_, _ = fmt.Fprintln(w, "cancelled")
	userCancelled.Store(true)
	return nil
}

const cancelWrappedKey = "forgectl.cancel-wrapped"

// withCancelHandling wraps every runnable command in the tree so a huh abort
// (Esc, Ctrl+C) ends as noteCancelled rather than as `ERROR User aborted.`
// Wrapping the tree once covers every current and future prompt call site.
// It is idempotent: a hub-selected verb dispatches through the same root again.
func withCancelHandling(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if run := c.RunE; run != nil && c.Annotations[cancelWrappedKey] == "" {
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[cancelWrappedKey] = "1"
			c.RunE = func(cmd *cobra.Command, args []string) error {
				err := run(cmd, args)
				if errors.Is(err, huh.ErrUserAborted) {
					return noteCancelled(cmd.OutOrStdout())
				}
				return err
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
