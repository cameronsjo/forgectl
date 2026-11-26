// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Command forgectl-docs is the standalone plugin binary for `forgectl docs`
// (dispatched via forgectl's Rung B PATH extension lookup).
package main

import (
	"context"
	"os"

	"github.com/cameronsjo/forgectl/internal/docscli"
)

func main() {
	if err := docscli.Execute(context.Background()); err != nil {
		os.Exit(docscli.ExitCode(err))
	}
}
