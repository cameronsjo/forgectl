package cli

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxTreeCmd prints the session → window → pane tree. Icons are on unless
// --no-icons or NO_COLOR is set (M5 promotes this to a shared, persistent
// preference across the TUI and the other read verbs).
func newTmuxTreeCmd(client *tmux.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Show the session → window → pane tree",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return writeTmuxTreeJSON(cmd, client)
			}
			noIcons, _ := cmd.Flags().GetBool("no-icons") // persistent root flag
			icons := !noIcons && os.Getenv("NO_COLOR") == ""
			out, err := client.Tree(cmd.Context(), icons)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit [{"id":...,"name":...,"attached":...,"windows":[{"id":...,"index":...,"name":...,"active":...,"panes":[{"id":...,"index":...,"command":...,"title":...,"active":...}]}]}] to stdout`)
	return cmd
}

// tmuxTreeSessionJSON, tmuxTreeWindowJSON and tmuxTreePaneJSON are the --json
// wire shape for `tmux tree`: the hierarchy the text tree draws, with the
// native ids the text omits so a script can target what it finds.
type tmuxTreeSessionJSON struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Attached bool                 `json:"attached"`
	Windows  []tmuxTreeWindowJSON `json:"windows"`
}

type tmuxTreeWindowJSON struct {
	ID     string             `json:"id"`
	Index  int                `json:"index"`
	Name   string             `json:"name"`
	Active bool               `json:"active"`
	Panes  []tmuxTreePaneJSON `json:"panes"`
}

type tmuxTreePaneJSON struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Command string `json:"command"`
	Title   string `json:"title"`
	Active  bool   `json:"active"`
}

func writeTmuxTreeJSON(cmd *cobra.Command, client *tmux.Client) error {
	ctx := cmd.Context()
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return err
	}
	windows, err := client.ListWindows(ctx)
	if err != nil {
		return err
	}
	panes, err := client.ListPanes(ctx)
	if err != nil {
		return err
	}
	return encodeTmuxTreeJSON(cmd.OutOrStdout(), sessions, windows, panes)
}

// encodeTmuxTreeJSON is the pure assembly step, testable from a fixture. It
// groups and sorts exactly as the text tree does: by native parent id, then
// sessions by name (id breaks a mid-rename tie), windows and panes by index.
// Every name is tmux's, so the encoder's own escaping is what makes it
// terminal-safe. Every level encodes [] when empty, never null.
func encodeTmuxTreeJSON(w io.Writer, sessions []tmux.Session, windows []tmux.Window, panes []tmux.Pane) error {
	winBySession := map[string][]tmux.Window{}
	for _, win := range windows {
		winBySession[win.SessionID] = append(winBySession[win.SessionID], win)
	}
	panesByWindow := map[string][]tmux.Pane{}
	for _, p := range panes {
		panesByWindow[p.WindowID] = append(panesByWindow[p.WindowID], p)
	}

	sorted := make([]tmux.Session, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].ID < sorted[j].ID
	})

	out := make([]tmuxTreeSessionJSON, 0, len(sorted))
	for _, s := range sorted {
		ws := winBySession[s.ID]
		sort.Slice(ws, func(i, j int) bool { return ws[i].Index < ws[j].Index })
		wrows := make([]tmuxTreeWindowJSON, 0, len(ws))
		for _, win := range ws {
			ps := panesByWindow[win.ID]
			sort.Slice(ps, func(i, j int) bool { return ps[i].Index < ps[j].Index })
			prows := make([]tmuxTreePaneJSON, 0, len(ps))
			for _, p := range ps {
				prows = append(prows, tmuxTreePaneJSON{
					ID: p.ID, Index: p.Index, Command: p.Command, Title: p.Title, Active: p.Active,
				})
			}
			wrows = append(wrows, tmuxTreeWindowJSON{
				ID: win.ID, Index: win.Index, Name: win.Name, Active: win.Active, Panes: prows,
			})
		}
		out = append(out, tmuxTreeSessionJSON{ID: s.ID, Name: s.Name, Attached: s.Attached, Windows: wrows})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
