//go:build unix

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/surface/projectview"
	"github.com/cameronsjo/forgectl/internal/tui"
)

type projectTimelineReport struct {
	Version   int          `json:"version"`
	ProjectID string       `json:"project_id"`
	ThreadID  string       `json:"thread_id,omitempty"`
	View      deskShowJSON `json:"view"`
}

func newSurfaceProjectTimelineCmd(deps module.Deps, root *string) *cobra.Command {
	var threadID string
	var asJSON, live, noIcons bool
	command := &cobra.Command{
		Use:   "timeline <project-id>",
		Short: "Read project evidence with Desk's renderer, without approving or launching work",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if live && asJSON {
				return projectUsage(errors.New("--live and --json cannot be combined"))
			}
			if live && !deskHasTerminal() {
				return projectUsage(tui.ErrRunWatchNeedsTerminal)
			}
			if live && termIsDumb() {
				return projectUsage(tui.DumbTerminalError("surface project timeline --live", "surface project timeline without --live"))
			}
			source := projectview.NewSource(project.Store{Root: *root}, arguments[0])
			name := arguments[0]
			if threadID != "" {
				name = threadID
			}
			references, err := source.List()
			if err != nil {
				return projectError(err)
			}
			var reference runview.RunRef
			for _, candidate := range references {
				if candidate.Name == name {
					reference = candidate
					break
				}
			}
			if reference.Name == "" {
				return projectError(fmt.Errorf("%w: thread is not in this project", project.ErrNotFound))
			}
			if live {
				return tui.RunWatch(command.Context(), source, tui.RunWatchOptions{Theme: deps.Theme, Name: reference.Name, ASCII: noIcons})
			}
			loaded, err := loadRun(source, reference)
			if err != nil {
				return projectError(err)
			}
			if asJSON {
				err = writeJSON(command.OutOrStdout(), projectTimelineReport{Version: project.Version, ProjectID: arguments[0], ThreadID: threadID, View: showJSON(loaded, false)})
			} else {
				err = writeShowText(command.OutOrStdout(), loaded, deskShowOpts{events: true, at: -1}, noIcons)
			}
			if err != nil {
				return err
			}
			if loaded.delta.Err != nil {
				return projectError(fmt.Errorf("%w: timeline is incomplete: %w", project.ErrInvalidState, loaded.delta.Err))
			}
			if loaded.delta.Partial || loaded.delta.Held || loaded.delta.Dropped != 0 {
				return projectError(fmt.Errorf("%w: timeline has bounded, held, or malformed input", project.ErrInvalidState))
			}
			return nil
		},
	}
	command.Flags().StringVar(&threadID, "thread", "", "show one exact logical thread; otherwise show the project")
	command.Flags().BoolVar(&asJSON, "json", false, "print sanitized evidence and explicit partial/unknown states")
	command.Flags().BoolVar(&live, "live", false, "open Desk's read-only live evidence viewer (requires a terminal)")
	command.Flags().BoolVar(&noIcons, "no-icons", false, "use the ASCII legend")
	return withProjectRoot(root, command)
}
