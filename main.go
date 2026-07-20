// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Command forgectl is a personal dev-experience CLI for the headless
// workbench. Bare invocation opens a TUI menu (thumb mode); typed verbs drive
// it directly (power mode). See internal/cli and internal/tui.
package main

import (
	"context"
	"os"

	"github.com/cameronsjo/forgectl/internal/cli"
)

func main() {
	if err := cli.Execute(context.Background()); err != nil {
		// cli.ExitCode reads a command's opted-in typed exit code (see
		// internal/cli/exitcode.go); everything else still exits 1.
		os.Exit(cli.ExitCode(err))
	}
}
