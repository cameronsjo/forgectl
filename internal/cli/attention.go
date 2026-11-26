// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// attentionModule declares `attention`: the per-machine attention queue.
// `attention list` merges the desk's waiting items, its open asks, the
// chief-of-staff sweep and a stuck drain into one queue ordered by what a
// delay costs. The rows and threat model are docs/commands/attention.md.
var attentionModule = module.Manifest{
	Name:      "attention",
	Tier:      module.TierExtension,
	ConfigKey: "attention",
	New:       newAttentionCmd,
}

// Injectable seams, so command tests need no home directory, herdr or clock.
var (
	attentionGetenv   = os.Getenv
	attentionUserHome = os.UserHomeDir
	attentionNow      = time.Now
)

// attentionOpts are `list`'s flags.
type attentionOpts struct {
	session string
	asJSON  bool
}

func newAttentionCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attention",
		Short: "Per-machine attention queue: what waits on you",
		Long: `attention keeps one queue of everything on this machine that waits on you:
the desk's waiting items, the desk's open asks, the last ` + "`forgectl sweep`" + `
result and a stuck drain, one row per wait, ordered by what a delay costs: a
blocked worker, a desk item, a marker-clean PR, an ask.

  forgectl attention list [--session ID] [--json]
                                 the queue

Every source is read without writing: nothing is created, pinned, moved,
run, skipped, chmodded or locked. Ask text comes from agents and is cleaned
and cut before it is shown. The pane a row points at comes from herdr agent
list, never from a desk item or an ask alone.`,
		Args:          safeRootArgs,
		RunE:          showRootHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(newAttentionListCmd(deps))
	return cmd
}

func newAttentionListCmd(deps module.Deps) *cobra.Command {
	var o attentionOpts
	cmd := &cobra.Command{
		Use:   "list",
		Short: "The queue: one row per wait, by delay cost",
		Long: `list prints the queue. --json prints {"rows": [...], "skipped": N,
"sources": {...}}; skipped counts asks log lines and sweep entries dropped as
malformed, and sources says whether the desk, its asks, herdr and sweep.json
were read. With $FORGECTL_DESK_ASKS_HOST set (and no $DESK_DIR or
$CLAUDE_DESK_DIR), the asks live on that host: sources.asks reads "remote"
and no ask rows are shown. See docs/commands/attention.md for the row fields.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAttentionList(cmd, deps, o)
		},
	}
	cmd.Flags().StringVar(&o.session, "session", "", "only this session's rows")
	cmd.Flags().BoolVar(&o.asJSON, "json", false, "print the queue as JSON")
	return cmd
}
