package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newProjectsPullAllCmd builds `forgectl projects pull-all [dir]` — sequential
// `git pull --rebase` over every discovered project (absorbs git-pull-all).
// A repo with a dirty working tree is skipped, not pulled; a repo whose pull
// fails is reported and counted, so one bad repo doesn't abort the batch —
// same aggregate-error contract as `clone --org` (see cloneOrg).
func newProjectsPullAllCmd(client *projects.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pull-all [dir]",
		Short: "Pull every project (skips dirty checkouts)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) == 1 {
				dir = args[0]
			}

			results, err := client.PullAll(cmd.Context(), dir)
			if err != nil {
				return err
			}

			var failed int
			rows := make([]pullResultJSON, 0, len(results))
			for _, r := range results {
				if r.Status == projects.PullFailed {
					failed++
				}
				if asJSON {
					rows = append(rows, pullResultJSON{Name: r.Name, Status: r.Status.String()})
					continue
				}
				// r.Name is a raw os.ReadDir entry name, not a validated segment — any
				// directory under the projects root can carry ANSI or bidi controls
				// into it. This site is a direct Fprintf, so it bypasses the central
				// termsafe error seam that covers returned errors.
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s)\n", pullGlyph(r.Status), termsafe.SafeLine(r.Name), r.Status)
			}
			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), rows); err != nil {
					return err
				}
			}
			if failed > 0 {
				// Under --json each row's status is the verdict
				// (forgectl#862).
				return jsonVerdict(fmt.Errorf("%d of %d repos failed to pull", failed, len(results)), asJSON)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"name":...,"status":...}] to stdout; the exit code is unchanged (1 when any pull failed)`)
	return cmd
}

// pullResultJSON is one `projects pull-all --json` row (additive-only,
// ADR-0008). Status is PullStatus.String(), the same word the human line
// prints in parentheses.
type pullResultJSON struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// pullGlyph renders a one-character status badge for the pull-all report.
func pullGlyph(s projects.PullStatus) string {
	switch s {
	case projects.PullUpToDate:
		return "✓"
	case projects.PullUpdated:
		return "↓"
	case projects.PullSkippedDirty:
		return "⚠"
	case projects.PullSkippedUnknown:
		return "?"
	case projects.PullFailed:
		return "✗"
	default:
		return "?"
	}
}
