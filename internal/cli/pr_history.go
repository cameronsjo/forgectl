package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
)

// newPrHistoryCmd builds `forgectl pr history` — the session audit trail as
// its own verb (forgectl#508). The trail records what teardown, cleanup,
// findings cleanup, prune and repair each removed, so "what removed this?" is a
// question about the whole lifecycle; answering it only through a flag on the
// repair verb hid it from anyone not already repairing something.
//
// It shares runRepairHistory with `pr repair --history`, which stays as an
// alias, so the two spellings cannot drift: same rows, same bare --json array,
// same stderr notes.
func newPrHistoryCmd(client *pr.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show the session audit trail (repair, teardown, cleanup, prune)",
		Long: `history prints the session audit trail: one row per intent or completion of
every destructive session verb, oldest first, newest 2000 rows at most.

Human columns: TIME  VERB  MODE  OUTCOME  REF  RECORD, with optional indented
note and detail lines. A row with outcome "intent" and no completion beside it
is a mutation that died mid-way; its workspace is the pointer to what is left.

--json prints a bare array of rows. A truncated or gapped view is reported on
stderr, never in stdout, so the array keeps its shape.

'forgectl pr repair --history' is the same view under its older spelling.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepairHistory(cmd, client, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the audit rows as a bare JSON array to stdout")
	return cmd
}
