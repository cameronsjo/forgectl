// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Command forgectl-herdr is the standalone plugin binary for `forgectl herdr`
// (dispatched via forgectl's Rung B PATH extension lookup).
package main

import (
	"context"
	"os"

	"github.com/cameronsjo/forgectl/internal/herdrcli"
)

func main() {
	if err := herdrcli.Execute(context.Background()); err != nil {
		os.Exit(herdrcli.ExitCode(err))
	}
}
