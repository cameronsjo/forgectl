// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Command forgectl-tmux is the standalone plugin binary for `forgectl tmux`
// (dispatched via forgectl's Rung B PATH extension lookup).
package main

import (
	"context"
	"os"

	"github.com/cameronsjo/forgectl/internal/tmuxcli"
)

func main() {
	if err := tmuxcli.Execute(context.Background()); err != nil {
		os.Exit(tmuxcli.ExitCode(err))
	}
}
