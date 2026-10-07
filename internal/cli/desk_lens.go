// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"github.com/spf13/cobra"
)

// newDeskLensCmd is `desk lens`: the lenses that teach forgectl to read an
// app's log as a run (ADR-0014).
func newDeskLensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lens",
		Short: "Teach forgectl an app's log: list lenses, and check one against a log",
		Long: `A lens is a small TOML file that teaches forgectl to read one app's log as
a run: how a line splits into an event, a step and a time, and which lines
start, finish or fail a step, or end the run. ` + "`desk show --log FILE --lens NAME`" + `
then draws the log as steps and a flow, and ` + "`--live`" + ` follows it as it grows,
so the log is read for you rather than by you.

A lens lives in the lenses directory as NAME.toml (` + "`desk lens list`" + ` prints
where), or anywhere when --lens names its path.

    about  = "nightly backup"
    format = "text"                 # or "json": one JSON object per line

    [text]                          # format = "text": split each line
    pattern = '^(?P<time>\S+) (?P<level>\w+) (?P<event>.*)$'

    [[rule]]                        # first match wins
    action = "start"                # start, close, fail, skip, end, note, ignore
    match  = '^backing up (?P<step>\S+)'
    [[rule]]
    action = "close"
    match  = '^backed up (?P<step>\S+)'
    [[rule]]
    action = "fail"
    field  = "level"                # match a field instead of the event
    match  = '^ERROR$'
    step   = "upload"
    [[rule]]
    action = "end"
    match  = '^done rc=(?P<exit>\d+)'
    [[rule]]
    action = "note"                 # no step: just say it in plain words
    match  = '^q=ord\.sel cid=(?P<cid>\d+)'
    say    = "querying orders for customer {cid}"

When a regular expression is not enough, write a translator instead, in any
language: it reads the app's log and writes one JSON line per event in the
event vocabulary, which the built-in lens ` + "`events`" + ` reads:

    {"event":"backing up","step":"photos","action":"start","time":"2026-10-07T01:00:01Z"}
    {"event":"done","action":"end","exit":0}

action is start, close, fail, skip or end (or absent: the event is shown
with no effect on a step). forgectl never runs the translator; pipe it into
a file and point --log at that file.

The full format is in docs/commands/desk.md. ` + "`desk lens check`" + ` shows what a
lens makes of a real log: each rule's hits, the rules that never matched,
and the most common lines no rule matched, which is the loop to write a lens
by, or to have an agent write one.`,
	}
	cmd.AddCommand(newDeskLensListCmd(), newDeskLensCheckCmd())
	return cmd
}

func newDeskLensListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the lenses in the lenses directory, and where it is",
		Long: `list prints the lenses directory, then one line per lens in it: its name,
format, rule count and about line. A lens that does not parse is listed
with its error.

Exit codes: 0 listed (an empty or missing directory too); 1 a lens did not
parse; 2 usage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runDeskLensList(cmd, asJSON) },
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"dir","lenses":[{"name","path","format","rules","about","error"}]}`)
	return cmd
}

func newDeskLensCheckCmd() *cobra.Command {
	var asJSON bool
	var logPath string
	cmd := &cobra.Command{
		Use:   "check <lens> --log FILE",
		Short: "Show what a lens makes of a log: rule hits, unused rules, unmatched lines",
		Long: `check reads FILE through the lens and reports, without drawing the run:
how many lines became events, were ignored or dropped; how many lines each
rule matched, marking a rule that matched none; the steps found; and the
most common events no rule matched, the lines a new rule could claim.

LENS is a name in the lenses directory, or a path to a .toml file.

Exit codes: 0 checked; 1 the log could not be read in full; 2 usage, or the
lens does not parse (the error names the line to fix).`,
		Example: `  forgectl desk lens check backup --log /var/log/backup.log
  forgectl desk lens check ./myapp.toml --log app.log --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if logPath == "" {
				return deskUsage("desk lens check: --log FILE is required: the log to check the lens against")
			}
			return runDeskLensCheck(cmd, args[0], logPath, asJSON)
		},
	}
	cmd.Flags().StringVar(&logPath, "log", "", "the log to read through the lens")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`print {"lens","format","events","ignored","dropped","dropped_fields","rules":[{"n","rule","hits"}],"steps","unmatched","unmatched_top":[{"name","count"}],"partial","note"}`)
	return cmd
}
