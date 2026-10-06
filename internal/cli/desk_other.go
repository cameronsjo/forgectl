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

func runDeskAdd(*cobra.Command, string, string, deskAddOpts) error { return errDeskUnsupported() }

func runDeskPlan(*cobra.Command, string, string, bool) error { return errDeskUnsupported() }

func runDeskStatus(*cobra.Command, string, string, bool) error { return errDeskUnsupported() }

func runDeskWatch(*cobra.Command, string, string, int, int) error { return errDeskUnsupported() }

func runDeskSkip(*cobra.Command, string, string, string) error { return errDeskUnsupported() }

func runDeskPrune(*cobra.Command, string, int, bool) error { return errDeskUnsupported() }

func runDeskSupervise(string, string, string, string) error { return errDeskUnsupported() }

// deskSupported: the desk core does not run here.
const deskSupported = false
