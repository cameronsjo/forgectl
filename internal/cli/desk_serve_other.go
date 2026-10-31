//go:build !unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

func newDeskServeCmd(*string, module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "A local web page to answer the desk's asks (Unix only)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return errDeskUnsupported() },
	}
}
