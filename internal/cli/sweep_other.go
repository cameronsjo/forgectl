// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// The ledger joins the desk and is written through openat, so off Unix the
// verb refuses before touching anything.
func runSweep(*cobra.Command, module.Deps, sweepOpts) error {
	return WithExitCode(fmt.Errorf("forgectl sweep is not supported on this OS (%s); it needs a Unix system", runtime.GOOS), exitUsage)
}
