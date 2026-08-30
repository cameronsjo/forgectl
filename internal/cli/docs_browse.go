package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
)

var docsStreamIsTerminal = func(stream any) bool {
	fd, ok := stream.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fd.Fd()))
}

func runDocsPreview(cmd *cobra.Command, deps module.Deps, args []string) error {
	if !docsStreamIsTerminal(cmd.InOrStdin()) || !docsStreamIsTerminal(cmd.OutOrStdout()) {
		return fmt.Errorf("docs preview requires an interactive terminal; use `forgectl docs list` for text output or `forgectl docs serve` for a server-only process")
	}
	roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
	if err != nil {
		return err
	}
	idx, err := docspkg.NewIndex(roots)
	if err != nil {
		return err
	}
	return runDocsPreviewServer(cmd, deps, idx)
}

func docsHelpForNonTTY(cmd *cobra.Command, args []string) (bool, error) {
	if docsStreamIsTerminal(cmd.InOrStdin()) && docsStreamIsTerminal(cmd.OutOrStdout()) {
		return false, nil
	}
	if len(args) == 0 {
		return true, cmd.Help()
	}
	return true, fmt.Errorf("docs preview requires an interactive terminal; use `forgectl docs list` for text output or `forgectl docs serve` for a server-only process")
}
