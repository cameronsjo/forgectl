// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Command forgectl is a personal dev-experience CLI for the headless
// workbench. Bare invocation opens a TUI menu (thumb mode); typed verbs drive
// it directly (power mode). See internal/cli and internal/tui.
package main

import (
	"context"
	"os"

	"github.com/cameronsjo/forgectl/internal/cli"
)

// main passes context.Background(), and fang gets no WithNotifySignal, on
// purpose (forgectl#788): SIGINT and SIGTERM keep Go's default disposition
// and end forgectl. That does not end its child. A terminal Ctrl-C reaches the
// whole foreground process group, so a child in it gets its own SIGINT. A
// SIGTERM aimed at forgectl reaches forgectl only, and a child outlives it
// (internal/sops/signal.go relies on this). A root signal context was weighed
// and declined. It would cancel a running sops child while internal/sops's own
// plaintext guard is still cleaning up for that same signal. It would SIGKILL
// an interactive child (editor, docker run -it) through exec.CommandContext
// instead of letting it take the terminal's Ctrl-C itself. And a verb that
// never reads ctx would swallow a second Ctrl-C. The verbs that need a
// graceful stop install their own scoped handler: docs serve, tasks mcp,
// sops's guard, and the surface trampoline. The trampoline catches SIGINT and
// SIGQUIT with signal.Notify and drains them, so its child still takes
// Ctrl-C; it does not ignore them, because an ignored disposition would be
// inherited across exec.
func main() {
	if err := cli.Execute(context.Background()); err != nil {
		// cli.ExitCode reads a command's opted-in typed exit code (see
		// internal/cli/exitcode.go); everything else still exits 1.
		os.Exit(cli.ExitCode(err))
	}
}
