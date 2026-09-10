package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// tmuxLsRowJSON is the --json wire shape for one `tmux ls` row — the same
// four fields the human table's marker/name/count/path columns show.
type tmuxLsRowJSON struct {
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached bool   `json:"attached"`
	Path     string `json:"path"`
}

// writeTmuxLsJSON encodes sessions as a JSON array through the sanctioned
// termsafe seam. Name and Path are tmux's, not forgectl's, so they are
// untrusted text — the encoder's own terminal-escaping is what neutralizes
// them here, the same guarantee SafeLine gives the human table. An empty
// result encodes [], never null.
func writeTmuxLsJSON(w io.Writer, sessions []tmux.Session) error {
	rows := make([]tmuxLsRowJSON, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, tmuxLsRowJSON{
			Name:     s.Name,
			Windows:  s.Windows,
			Attached: s.Attached,
			Path:     s.Path,
		})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// newTmuxLsCmd lists sessions as a plain aligned table. The colored,
// icon-aware rendering is the TUI's job (M5); this is the power-mode glance.
func newTmuxLsCmd(client *tmux.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List tmux sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sessions, err := client.ListSessions(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeTmuxLsJSON(out, sessions)
			}
			if len(sessions) == 0 {
				fmt.Fprintln(out, "no tmux sessions")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, s := range sessions {
				marker := "○"
				if s.Attached {
					marker = "●"
				}
				unit := "windows"
				if s.Windows == 1 {
					unit = "window"
				}
				// Name and Path are tmux's, not forgectl's: whoever created the
				// session chose both, so each is untrusted text on its way to a
				// terminal. SafeLine leaves an ordinary name or path unchanged —
				// the table an operator reads every day is byte-identical — and
				// escapes in place anything that could repaint or reorder the
				// line. Deliberately not QuotePath: that wraps every value in
				// quotes, rewriting rows nobody asked it to touch.
				fmt.Fprintf(w, "%s\t%s\t%d %s\t%s\n",
					marker, termsafe.SafeLine(s.Name), s.Windows, unit, termsafe.SafeLine(s.Path))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"name":...,"windows":...,"attached":...,"path":...}] to stdout`)
	return cmd
}
