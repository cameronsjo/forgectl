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
		os.Exit(1)
	}
}
