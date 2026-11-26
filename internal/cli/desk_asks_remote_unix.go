//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// runDeskAsksRemote runs "forgectl desk <words...>" on r.Host, streaming
// stdout and stderr unchanged and returning the remote exit code. lost says
// what did not happen when ssh fails ("the ask was NOT recorded").
func runDeskAsksRemote(cmd *cobra.Command, r *deskAsksRemote, verb string, words []string, stdin io.Reader, lost string) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := deskRemoteRun(ctx, deskRemoteArgv(*r, words), stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if ctx.Err() != nil && parent.Err() == nil {
		return WithExitCode(fmt.Errorf("%s: interrupted while ssh to %s ran; %s", verb, r.Host, lost), deskExitInterrupted)
	}
	return deskRemoteResult(verb, *r, code, err, lost)
}
