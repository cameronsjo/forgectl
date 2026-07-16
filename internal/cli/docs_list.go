package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
)

// newDocsListCmd builds `forgectl docs list [dir|file ...]` — lists the
// indexed doc set without binding a server.
func newDocsListCmd(deps module.Deps) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list [dir|file ...]",
		Short: "List the indexed docs, most-recently-modified first",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
			if err != nil {
				return err
			}
			idx, err := docspkg.NewIndex(roots)
			if err != nil {
				return err
			}
			return printDocsList(cmd, idx.List(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	return cmd
}

// docJSON is the --json wire shape for one entry of `forgectl docs list`.
type docJSON struct {
	Root    string    `json:"root"`
	Path    string    `json:"path"`
	Title   string    `json:"title"`
	ModTime time.Time `json:"modTime"`
}

func printDocsList(cmd *cobra.Command, docs []docspkg.Doc, asJSON bool) error {
	out := cmd.OutOrStdout()

	if asJSON {
		wire := make([]docJSON, len(docs))
		for i, d := range docs {
			wire[i] = docJSON{Root: d.RootLabel, Path: d.RelPath, Title: d.Title, ModTime: d.ModTime}
		}
		enc := json.NewEncoder(out)
		return enc.Encode(wire)
	}

	if len(docs) == 0 {
		fmt.Fprintln(out, "no docs found")
		return nil
	}
	for _, d := range docs {
		fmt.Fprintf(out, "%-16s %-48s %s\n", d.RootLabel, d.RelPath, d.Title)
	}
	return nil
}
