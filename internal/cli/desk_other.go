// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// The desk is built on openat, O_NOFOLLOW, process sessions and groups, so
// off Unix every verb refuses before touching anything (`layout` through
// deskSupported, the rest here).

func runDeskDashboard(*cobra.Command, module.Deps, string, bool) error {
	return errDeskUnsupported()
}

func runDeskAdd(*cobra.Command, module.Deps, string, string, deskAddOpts) error {
	return errDeskUnsupported()
}

func runDeskPlan(*cobra.Command, module.Deps, string, string, bool) error {
	return errDeskUnsupported()
}

func runDeskStatus(*cobra.Command, module.Deps, string, string, bool) error {
	return errDeskUnsupported()
}

func runDeskWatch(*cobra.Command, module.Deps, string, string, int, int) error {
	return errDeskUnsupported()
}

func runDeskSkip(*cobra.Command, module.Deps, string, string, string, bool) error {
	return errDeskUnsupported()
}

func runDeskPrune(*cobra.Command, module.Deps, string, int, bool, bool) error {
	return errDeskUnsupported()
}

func runDeskSupervise(string, string, string, string) error { return errDeskUnsupported() }

// deskSupported: the desk core does not run here.
const deskSupported = false
