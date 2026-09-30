package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxWindowsCmd lists every window across all sessions, with its jump
// target. The TUI turns these into a one-keystroke cross-session jump (M5).
func newTmuxWindowsCmd(client *tmux.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "windows",
		Short: "List windows across all sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			windows, err := client.DisplayWindows(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeTmuxWindowsJSON(out, windows)
			}
			if len(windows) == 0 {
				fmt.Fprintln(out, "no windows")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, win := range windows {
				marker := " "
				if win.Active {
					marker = "*"
				}
				unit := "panes"
				if win.Panes == 1 {
					unit = "pane"
				}
				// "session:index" is a human LOCATION here, not a target the
				// program will act on — every jump goes by native window id.
				// It stays in the output because it is the shape operators
				// already read this column as.
				// Both names are tmux's — chosen by whoever created the session
				// and the window — so each is neutralized on its way to the
				// terminal. SafeLine is a no-op on an ordinary name.
				location := fmt.Sprintf("%s:%d", termsafe.SafeLine(win.Session), win.Index)
				fmt.Fprintf(w, "%s\t%s\t%s\t%d %s\n", marker, location, termsafe.SafeLine(win.Name), win.Panes, unit)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit [{"id":...,"session_id":...,"session":...,"index":...,"name":...,"active":...,"panes":...}] to stdout`)
	return cmd
}

// tmuxWindowRowJSON is the --json wire shape for one `tmux windows` row: the
// human table's columns plus the native ids, which are what every jump
// actually targets.
type tmuxWindowRowJSON struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Session   string `json:"session"`
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	Panes     int    `json:"panes"`
}

// writeTmuxWindowsJSON encodes windows through the sanctioned termsafe seam;
// the session and window names are tmux's, so the encoder's escaping is what
// neutralizes them. An empty result encodes [], never null.
func writeTmuxWindowsJSON(w io.Writer, windows []tmux.Window) error {
	rows := make([]tmuxWindowRowJSON, 0, len(windows))
	for _, win := range windows {
		rows = append(rows, tmuxWindowRowJSON{
			ID:        win.ID,
			SessionID: win.SessionID,
			Session:   win.Session,
			Index:     win.Index,
			Name:      win.Name,
			Active:    win.Active,
			Panes:     win.Panes,
		})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
