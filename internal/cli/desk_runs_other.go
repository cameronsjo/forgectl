// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import "github.com/spf13/cobra"

func runDeskRuns(*cobra.Command, string, deskLogOpts, bool) error { return errDeskUnsupported() }

func runDeskShow(*cobra.Command, string, string, deskLogOpts, deskShowOpts) error {
	return errDeskUnsupported()
}
