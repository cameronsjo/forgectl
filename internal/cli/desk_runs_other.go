// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

func runDeskRuns(*cobra.Command, module.Deps, string, deskLogOpts, bool) error {
	return errDeskUnsupported()
}

func runDeskShow(*cobra.Command, module.Deps, string, string, deskLogOpts, deskShowOpts) error {
	return errDeskUnsupported()
}

func runDeskLensList(*cobra.Command, bool) error { return errDeskUnsupported() }

func runDeskLensCheck(*cobra.Command, string, string, bool) error { return errDeskUnsupported() }
