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
// purpose (forgectl#788): SIGINT and SIGTERM keep Go's default disposition and
// end the process with its child. A root signal context was weighed and
// declined. It would cancel a running sops child while internal/sops's own
// plaintext guard is still cleaning up for that same signal. It would SIGKILL
// an interactive child (editor, docker run -it) through exec.CommandContext
// instead of letting it take the terminal's Ctrl-C itself. And a verb that
// never reads ctx would swallow a second Ctrl-C. The verbs that need a
// graceful stop install their own scoped handler: docs serve, tasks mcp,
// sops's guard, and the surface trampoline, which ignores SIGINT.
func main() {
	if err := cli.Execute(context.Background()); err != nil {
		// cli.ExitCode reads a command's opted-in typed exit code (see
		// internal/cli/exitcode.go); everything else still exits 1.
		os.Exit(cli.ExitCode(err))
	}
}
