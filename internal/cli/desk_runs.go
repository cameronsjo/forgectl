// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/runview"
)

// deskLogOpts names a JSONL log for `desk runs` and `desk show`, and the keys
// its lines are read with.
type deskLogOpts struct {
	path                    string
	eventKey, stepKey, time string
	// lens names the lens the log is read through: a name in the lenses
	// directory, or a file path (ADR-0014).
	lens string
	// followCmd is a command whose stdout is the log (show only): run once
	// for a printed view, restarted whenever it exits under --live.
	followCmd string
}

// deskStdinLog is the --log value that reads the log from stdin.
const deskStdinLog = "-"

// named reports whether a log is named at all: a file, stdin or a command.
func (o deskLogOpts) named() bool { return o.path != "" || o.followCmd != "" }

func (o *deskLogOpts) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.path, "log", "", "a log to read as a run: JSONL (one JSON object per line), or with --lens any format the lens reads; - reads it from stdin")
	cmd.Flags().StringVar(&o.eventKey, "event-key", runview.DefaultLogKeys.Event, "with --log: the key holding each line's event name")
	cmd.Flags().StringVar(&o.stepKey, "step-key", runview.DefaultLogKeys.Step, "with --log: the key holding the step an event belongs to")
	cmd.Flags().StringVar(&o.time, "time-key", runview.DefaultLogKeys.Time, "with --log: the key holding the event time (RFC 3339, or epoch seconds)")
	cmd.Flags().StringVar(&o.lens, "lens", "", "with --log: read it through a lens (NAME in the lenses directory, or a .toml file) that says how the app's lines read and which start, finish or fail a step")
}

func (o deskLogOpts) keys() runview.LogKeys {
	return runview.LogKeys{Event: o.eventKey, Step: o.stepKey, Time: o.time}
}

func newDeskRunsCmd(dir *string, deps module.Deps) *cobra.Command {
	var asJSON bool
	var log deskLogOpts
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List runs and how far each got: steps done, failed, live or ended",
		Long: `runs lists every desk run (running, done and skipped items) with its progress:
the run's state, its steps done of total, failed steps, and the time since it
last did anything. Live runs come first, then the most recent. --log adds a
JSONL log as one more run.

runs is the progress view; ` + "`desk status`" + ` is the queue view (waiting items,
hashes, WHAT, skip reasons). A pending item is not a run yet, so runs leaves
it out.

Like status, runs only reads the desk: it creates, moves, chmods and writes
nothing.

Exit codes: 0 listed; 1 a source could not be read (the rest are still
listed); 2 usage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := log.check(cmd, "runs"); err != nil {
				return err
			}
			return runDeskRuns(cmd, deps, *dir, log, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`print [{"source","name","kind","live","exit","steps","done","failed","events","updated","partial"}]`)
	log.bind(cmd)
	return cmd
}

func newDeskShowCmd(dir *string, deps module.Deps) *cobra.Command {
	var asJSON, events, live bool
	var at int
	var layout string
	var log deskLogOpts
	cmd := &cobra.Command{
		Use:   "show <name> | show --log FILE|- [--lens LENS] | show --follow-cmd CMD [--lens LENS]",
		Short: "Show one run as a flow: steps, edges, durations; replay with --at",
		Long: `show prints one run as the visualizer draws it: each step with its state,
duration and the steps it waits on, then the run's exit. --events adds the
event timeline, one line per event. --at N replays: the state after the run's
first N events.

NAME is a desk item (NN-name). --log FILE reads a JSONL log instead: one
JSON object per line, whose --event-key (default "event") names the event.
A log has no step model, so it shows its events and no step state.
--log - reads the log from stdin instead of a file, and --follow-cmd CMD
reads the stdout of CMD (run with /bin/sh -c, no stdin and no terminal),
for a log with no file to point at: kubectl logs -f, ssh -o BatchMode=yes
HOST tail -n +1 -F, docker logs -f. The run is named by the command's first
word. The stream is held in memory, never written to disk, up to the same
32 MiB cap as a file. A printed view reads it until it ends.

--lens teaches show to read an app's own log: a small TOML file that says
how a line splits (JSON keys, or a regular expression for plain text) and
which lines start, finish or fail a step, or end the run. With it the log
gets steps, a flow and an exit. Lenses live in the lenses directory
(` + "`desk lens list`" + ` prints it); ` + "`desk lens check`" + ` shows which lines each
rule matched, to write or fix one.

--live opens the dashboard's run view on the run alone, full screen, and
follows it as the log grows: the steps as a flow above the event timeline,
with replay. It needs a terminal (with --log -, on stdout; keys are read from
the terminal itself). Under --live a --follow-cmd command is restarted when
it exits, so the run does not end because the stream did; the restarted
command's output replaces the old, as a log file replaced on disk does, so
the command should replay its log from the start (tail needs -n +1). It is
not restarted once the lens read the run's end, nor after 10 runs in a row
with no output.
--layout flow draws the steps as a top-down flowchart instead: a box per
step with its state, duration and note, the running one marked now, from a
start to an end that shows the run's exit, beside the event timeline when
the window is wide enough.

show is the run's shape and timeline. The item's record (WHAT, WHY, sha256,
times, skip reason, log path) is ` + "`desk status NAME`" + `, which show points to and
does not repeat.

Exit codes: 0 shown; 1 no such run, or it could not be read; 2 usage.`,
		Example: `  forgectl desk show 17-nightly --events
  forgectl desk show 17-nightly --at 4
  forgectl desk show --log /tmp/build/events.jsonl --event-key kind --json
  forgectl desk show --log /var/log/backup.log --lens backup --live
  kubectl logs -f deploy/app | forgectl desk show --log - --lens app --live
  forgectl desk show --follow-cmd 'kubectl logs -f deploy/app' --lens app --live --layout flow`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			switch {
			case name == "" && !log.named():
				return deskUsage("desk show: give a run name or --log FILE; usage: forgectl desk show <name> [flags]")
			case name != "" && log.named():
				return deskUsage("desk show: give a run name or a log (--log, --follow-cmd), not both")
			case log.path != "" && log.followCmd != "":
				return deskUsage("desk show: give --log or --follow-cmd, not both")
			case cmd.Flags().Changed("follow-cmd") && strings.TrimSpace(log.followCmd) == "":
				return deskUsage("desk show: --follow-cmd must name a command")
			case layout != deskLayoutList && layout != deskLayoutFlow:
				return deskUsage("desk show: --layout is %s or %s", deskLayoutList, deskLayoutFlow)
			case cmd.Flags().Changed("layout") && !live:
				return deskUsage("desk show: --layout is the live view's; add --live")
			case cmd.Flags().Changed("at") && at < 0:
				return deskUsage("desk show: --at must be 0 or more")
			case live && (asJSON || events || cmd.Flags().Changed("at")):
				return deskUsage("desk show: --live is the live view; it takes no --json, --events or --at (its own keys step and replay)")
			}
			if err := log.check(cmd, "show"); err != nil {
				return err
			}
			replay := -1
			if cmd.Flags().Changed("at") {
				replay = at
			}
			return runDeskShow(cmd, deps, *dir, name, log, deskShowOpts{asJSON: asJSON, events: events, at: replay, live: live, layout: layout})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`print {"source","name","kind","live","exit","at","events_total","steps","edges","events","counts","partial","held","note"}`)
	cmd.Flags().BoolVar(&events, "events", false, "also print the event timeline (always in --json)")
	cmd.Flags().IntVar(&at, "at", 0, "replay: show the state after the run's first N events")
	cmd.Flags().BoolVar(&live, "live", false, "open the live run view on this run, full screen, and follow it (needs a terminal)")
	cmd.Flags().StringVar(&layout, "layout", deskLayoutList, "with --live: how the steps are drawn: list (a flow above the timeline) or flow (a top-down flowchart)")
	log.bind(cmd)
	cmd.Flags().StringVar(&log.followCmd, "follow-cmd", "", "read the log from the stdout of this command (/bin/sh -c), restarted when it exits under --live")
	return cmd
}

// deskShowOpts are show's output choices. at is -1 for no replay.
type deskShowOpts struct {
	asJSON, events, live bool
	at                   int
	layout               string
}

// The --layout values.
const (
	deskLayoutList = "list"
	deskLayoutFlow = "flow"
)

// check refuses a key flag given without --log, where nothing would read it,
// a key flag beside --lens, which names its own keys, and an empty event key,
// which matches no line.
func (o deskLogOpts) check(cmd *cobra.Command, verb string) error {
	if !o.named() {
		for _, f := range []string{"event-key", "step-key", "time-key", "lens"} {
			if cmd.Flags().Changed(f) {
				return deskUsage("desk %s: --%s is only read with a log (--log or --follow-cmd)", verb, f)
			}
		}
	}
	if cmd.Flags().Changed("lens") {
		if o.lens == "" {
			return deskUsage("desk %s: --lens must name a lens", verb)
		}
		for _, f := range []string{"event-key", "step-key", "time-key"} {
			if cmd.Flags().Changed(f) {
				return deskUsage("desk %s: --%s is not read with --lens: set the keys in the lens's [json] table", verb, f)
			}
		}
	}
	if o.eventKey == "" {
		return deskUsage("desk %s: --event-key must not be empty", verb)
	}
	return nil
}
