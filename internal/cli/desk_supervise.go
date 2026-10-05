// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// newDeskPlumbingCmd is the hidden `desk` parent that carries `_supervise`
// until the desk module (the dashboard and its visible verbs) registers
// `desk` through the module registry; that change moves `_supervise` under
// the module's command and deletes this one.
func newDeskPlumbingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "desk",
		Short:        "Internal: desk plumbing; not for direct use",
		Hidden:       true,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("the desk dashboard is not available in this build")
		},
	}
	cmd.AddCommand(newDeskSuperviseCmd())
	return cmd
}

// newDeskSuperviseCmd builds `desk _supervise --sha SHA NAME`: the detached
// process that owns one claimed item's run. The desk starts it in a session
// of its own (desk.Launch), so it outlives the desk's pane. It runs only an
// item already in running/ whose bytes hash to --sha, the hash the operator
// approved, and to the hash fixed at queue time.
//
// Invoking it by hand is not harmless: the run it records in done/ looks
// exactly like an approved one. done/ records what a supervisor ran, not
// proof of approval; the agent deny rules on this command are the control.
// Hidden is presentation, not a control.
func newDeskSuperviseCmd() *cobra.Command {
	var dir, sha string
	cmd := &cobra.Command{
		Use:          "_supervise --sha SHA NAME",
		Short:        "Internal: run one claimed desk item to completion",
		Hidden:       true,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if dir == "" {
				// A missing home only matters when no env var names the dir,
				// and then ResolveDir refuses; report both causes together.
				home, homeErr := os.UserHomeDir()
				resolved, err := desk.ResolveDir(os.Getenv, home)
				if err != nil {
					return errors.Join(err, homeErr)
				}
				dir = resolved
			}
			if rc := desk.RunSupervisor(dir, args[0], sha); rc != 0 {
				return WithExitCode(fmt.Errorf("desk item finished with exit %d", rc), rc)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "desk directory (default: $DESK_DIR, $CLAUDE_DESK_DIR, then the XDG state dir)")
	cmd.Flags().StringVar(&sha, "sha", "", "the approved item's full sha256; the item runs only if its bytes still match")
	_ = cmd.MarkFlagRequired("sha") // fails only for a flag that does not exist, and it is defined above
	return cmd
}
