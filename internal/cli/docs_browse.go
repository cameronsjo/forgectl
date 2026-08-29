package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/docstui"
	"github.com/cameronsjo/forgectl/internal/module"
)

var docsStreamIsTerminal = func(stream any) bool {
	fd, ok := stream.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fd.Fd()))
}

func newDocsBrowseCmd(deps module.Deps) *cobra.Command {
	var graphics string
	cmd := &cobra.Command{
		Use:   "browse [dir|file ...]",
		Short: "Browse rendered docs in the terminal",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDocsBrowse(cmd, deps, args, graphics)
		},
	}
	cmd.Flags().StringVar(&graphics, "graphics", "auto", "image mode: auto, kitty, or off")
	return cmd
}

func runDocsBrowse(cmd *cobra.Command, deps module.Deps, args []string, graphics string) error {
	if !docsStreamIsTerminal(cmd.InOrStdin()) || !docsStreamIsTerminal(cmd.OutOrStdout()) {
		return fmt.Errorf("docs browse requires an interactive terminal; use `forgectl docs list` for text output or `forgectl docs serve` for the web reader")
	}
	mode, err := docs.ParseGraphicsMode(graphics)
	if err != nil {
		return err
	}
	roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
	if err != nil {
		return err
	}
	idx, err := docs.NewIndex(roots)
	if err != nil {
		return err
	}
	return docstui.Run(cmd.Context(), idx, deps.Runner, mode, cmd.InOrStdin(), cmd.OutOrStdout())
}

func docsHelpForNonTTY(cmd *cobra.Command, args []string) (bool, error) {
	if docsStreamIsTerminal(cmd.InOrStdin()) && docsStreamIsTerminal(cmd.OutOrStdout()) {
		return false, nil
	}
	if len(args) == 0 {
		return true, cmd.Help()
	}
	return true, fmt.Errorf("docs browsing requires an interactive terminal; use `forgectl docs list` for text output or `forgectl docs serve` for the web reader")
}
