// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// attentionModule declares `attention`: the per-machine attention queue.
// Sessions publish their `What's next:` items to a private spool, and
// `attention list` merges those with the desk and the chief-of-staff sweep
// into one queue ordered by what a delay costs. The protocol and threat model
// are docs/commands/attention.md.
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

// attentionOpts are the flags `put`, `rm` and `list` share.
type attentionOpts struct {
	session, pane, socket, cwd, turn string
	asJSON                           bool
}

func newAttentionCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attention",
		Short: "Per-machine attention queue: what each session is waiting on you for",
		Long: `attention keeps one queue of everything on this machine that waits on you.
Each Claude Code session publishes the ` + "`What's next:`" + ` items of its latest reply
with ` + "`attention put`" + `; ` + "`attention list`" + ` merges them with the desk's waiting items
and the last ` + "`forgectl sweep`" + ` result, one row per wait, ordered by what a
delay costs: a blocked worker, a desk item, a marker-clean PR, an action of
yours, a question.

  forgectl attention put --session ID --turn ID [--pane ID] [--socket PATH] [--cwd NAME]
                                 parse the reply on stdin and record its items
  forgectl attention rm --session ID
                                 drop a session's record
  forgectl attention list [--session ID] [--json]
                                 the queue

Records live in $XDG_STATE_HOME/cadence/attention (~/.local/state/cadence/attention),
one 0600 file per session in a 0700 directory. Any process of yours can write
there, so every record is checked again on read, its text is cleaned and cut,
and a malformed one is skipped and counted, never shown. The pane a row
points at comes from herdr agent list, never from a record.

A record is pruned when its pane is gone from your herdr server, or, when no
herdr pane you can see vouches for it, after [attention] ttl_hours (default 24).
The desk is read without writing: nothing is pinned, moved, run or skipped.`,
		Args:          safeRootArgs,
		RunE:          showRootHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(
		newAttentionPutCmd(deps),
		newAttentionRmCmd(deps),
		newAttentionListCmd(deps),
	)
	return cmd
}

func newAttentionPutCmd(deps module.Deps) *cobra.Command {
	var o attentionOpts
	cmd := &cobra.Command{
		Use:   "put --session ID --turn ID",
		Short: "Record the What's next items of the reply on stdin",
		Long: fmt.Sprintf(`put reads one assistant reply on stdin, finds its last "What's next:" block
outside fenced code, and records the block's items as this session's record,
replacing the one before. A reply with no block records no items, so the
session's earlier asks leave the queue. stdin must not be a terminal; a reply
longer than %d KiB is parsed from its last %d KiB.

--session is the Claude Code session id, and names the record's file. --pane
and --socket are $HERDR_PANE_ID and $HERDR_SOCKET_PATH, used only to prune a
record whose pane is gone; --cwd is the session's directory name. Each value
is checked: a session id is 1 to 64 letters, digits, '_' or '-'.

It prints items=, seen= and read= (the block's item lines, and how many the
parser understood). After writing it prunes the spool, and drops any other
session's record for the same pane.`, attentionStdinCap>>10, attentionStdinCap>>10),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAttentionPut(cmd, deps, o)
		},
	}
	cmd.Flags().StringVar(&o.session, "session", "", "the session id (required)")
	cmd.Flags().StringVar(&o.turn, "turn", "", "the turn id (required)")
	cmd.Flags().StringVar(&o.pane, "pane", "", "the session's herdr pane ($HERDR_PANE_ID)")
	cmd.Flags().StringVar(&o.socket, "socket", "", "the session's herdr socket ($HERDR_SOCKET_PATH)")
	cmd.Flags().StringVar(&o.cwd, "cwd", "", "the session's directory name")
	return cmd
}

func newAttentionRmCmd(deps module.Deps) *cobra.Command {
	var o attentionOpts
	cmd := &cobra.Command{
		Use:   "rm --session ID",
		Short: "Drop a session's record",
		Long: `rm deletes the session's record, when a session ends. A session with no
record is not an error.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAttentionRm(cmd, deps, o)
		},
	}
	cmd.Flags().StringVar(&o.session, "session", "", "the session id (required)")
	return cmd
}

func newAttentionListCmd(deps module.Deps) *cobra.Command {
	var o attentionOpts
	cmd := &cobra.Command{
		Use:   "list",
		Short: "The queue: one row per wait, by delay cost",
		Long: `list prunes the spool, then prints the queue. --json prints
{"rows": [...], "skipped": N, "sources": {...}}; skipped counts records and
sweep entries dropped as malformed, and sources says whether the spool, the
desk, herdr and sweep.json were read. See docs/commands/attention.md for the
row fields.`,
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
