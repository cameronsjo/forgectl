// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// deskAskOpts are `desk ask`'s flags.
type deskAskOpts struct {
	title, stepsFile, bucket, from, notify, allow string
	links                                         []string
	asJSON                                        bool
}

func newDeskAskCmd(dir *string, deps module.Deps) *cobra.Command {
	var o deskAskOpts
	cmd := &cobra.Command{
		Use:   "ask --title <text> [--steps-file FILE|-] [--bucket now|soon|optional|coming]",
		Short: "Ask the operator for something that is not a script",
		Long: `ask queues a request for the operator: a login, a purchase, a device check,
anything that needs a person and is not a script to run. The operator answers
it in ` + "`forgectl desk ui`" + ` or ` + "`forgectl desk serve`" + ` with one of the responses you
allow: done, a free-text reply, a link, or a snooze.

Each response is appended to the desk's asks log. Block on it with
  forgectl desk watch --responses ASK-ID --deadline 540
and, with --notify self, the answer is also typed into your own hc terminal
($HC_TERMINAL_ID) as one DESK-RESPONSE line, when it is at its prompt. The
line quotes its free text as data and names the front end that recorded the
answer; read the record with desk watch --responses before acting on it.

Titles are one line (at most 200 characters); steps are markdown (at most
16 KiB) and are shown with raw HTML off; links must be http or https. Pass a
link as URL or LABEL=URL.

ask prints id=, bucket= and watch= lines (or one JSON object with --json) and
exits 0 only when the ask is recorded.`,
		Example: `  forgectl desk ask --title "Approve the new deploy key" --steps-file steps.md \
    --bucket now --link "Settings=https://example.com/keys" --notify self`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeskAsk(cmd, deps, *dir, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.title, "title", "", "one line saying what you need (required)")
	f.StringVar(&o.stepsFile, "steps-file", "", "markdown file with the steps, or - for stdin")
	f.StringVar(&o.bucket, "bucket", "soon", "now, soon (\"when you have 10 minutes\"), optional, or coming")
	f.StringArrayVar(&o.links, "link", nil, "a link to show with the ask: URL or LABEL=URL (repeatable)")
	f.StringVar(&o.from, "from", "", "who is asking (default: $FORGECTL_WORKER, else the session)")
	f.StringVar(&o.notify, "notify", "", "tell your own hc terminal about each response: self, or the value of $HC_TERMINAL_ID; other addresses are refused")
	f.StringVar(&o.allow, "allow", "done,reply,link,snooze", "the responses the operator may give")
	f.BoolVar(&o.asJSON, "json", false, "print the recorded ask as JSON")
	return cmd
}

func newDeskAsksCmd(dir *string, deps module.Deps) *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "asks [ASK-ID]",
		Short: "List open asks, or show one with its responses",
		Long: `asks lists the open and snoozed asks by bucket. --all adds answered ones.
With an ASK-ID it shows that ask, its responses and every notify attempt.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			return runDeskAsks(cmd, deps, *dir, id, all, asJSON)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include answered asks")
	cmd.Flags().BoolVar(&asJSON, "json", false, `print [{"ask","state","responses","answer","snoozed_until","notified"}], or one such object with ASK-ID`)
	return cmd
}

func newDeskRespondCmd(dir *string, deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "respond <ask-id> <done|reply|link|snooze> [<text|url|duration>]",
		Short: "Answer an ask from the command line",
		Long: `respond records the operator's answer to an ask, as desk ui and desk serve
do, and tells the asking agent when the ask has a notify address. DURATION is
a Go duration (90m, 4h) or Nd for days, at most 30 days.

Any local process can run respond, so its answers are recorded with origin
"agent", and the asking agent is told it did not come through an operator
front end.
The operator answers in desk ui or desk serve. Agents must not answer their
own asks.`,
		Example: `  forgectl desk respond ask-1a2b3c4d done
  forgectl desk respond ask-1a2b3c4d reply "Use the staging account"
  forgectl desk respond ask-1a2b3c4d snooze 4h`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeskRespond(cmd, deps, *dir, args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"response","item","notify_error"}`)
	return cmd
}

// deskWatchResponsesHelp is the --responses paragraph of `desk watch --help`.
const deskWatchResponsesHelp = `

With --responses, watch follows the asks log instead of a run. Given an
ASK-ID it prints that ask's responses as JSON lines and exits 0 once the ask
is answered (done, reply or link; a snooze is printed and the watch goes on),
1 when there is no such ask. With no ASK-ID it prints each new response as
it arrives (--since 0 replays the log first). On the deadline it exits 75
with a resume= line whose --since is the log offset to continue from.`
