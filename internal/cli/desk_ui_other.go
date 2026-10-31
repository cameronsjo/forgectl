// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// newDeskUICmd off Unix: the asks store needs O_NOFOLLOW and flock.
func newDeskUICmd(_ *string, _ module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:          "ui",
		Short:        "Answer the asks agents left you, from the keyboard",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         func(*cobra.Command, []string) error { return errDeskUnsupported() },
	}
}
