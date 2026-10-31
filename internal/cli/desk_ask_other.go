// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

func runDeskAsk(*cobra.Command, module.Deps, string, deskAskOpts) error {
	return errDeskUnsupported()
}

func runDeskAsks(*cobra.Command, module.Deps, string, string, bool, bool) error {
	return errDeskUnsupported()
}

func runDeskRespond(*cobra.Command, module.Deps, string, []string, bool) error {
	return errDeskUnsupported()
}

func runDeskWatchResponses(*cobra.Command, module.Deps, string, string, int, int64) error {
	return errDeskUnsupported()
}
