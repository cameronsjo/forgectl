package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// renderDegradationNotes writes each per-host/per-query degradation note to
// stderr — diagnostics never go to stdout, where they would corrupt --json
// output.
//
// A note is built from low-trust material (a query label carrying a config
// owner, a source's own diagnostic text), so each one is reduced to a single
// inert terminal line. termsafe.SafeLine is the shared human-output boundary:
// it visibly escapes C1 controls and Unicode bidi overrides as well as C0 and
// DEL, so accidental or hostile content cannot reorder what the operator reads.
//
// One function for every command that renders notes, deliberately. This started
// as a review-only helper while `projects list` printed its notes raw, and the
// two stayed out of step until the gap was found; a shared sink is what stops
// the next note-producing command from being escaped-by-accident-or-not-at-all.
func renderDegradationNotes(cmd *cobra.Command, notes []string) {
	for _, n := range notes {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: "+termsafe.SafeLine(n))
	}
}
