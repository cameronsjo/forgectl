// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/runview"
)

// deskLogOpts names a JSONL log for `desk runs` and `desk show`, and the keys
// its lines are read with.
type deskLogOpts struct {
	path                    string
	eventKey, stepKey, time string
}

func (o *deskLogOpts) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.path, "log", "", "a JSONL log to read as a run: one JSON object per line, one event each")
	cmd.Flags().StringVar(&o.eventKey, "event-key", runview.DefaultLogKeys.Event, "with --log: the key holding each line's event name")
	cmd.Flags().StringVar(&o.stepKey, "step-key", runview.DefaultLogKeys.Step, "with --log: the key holding the step an event belongs to")
	cmd.Flags().StringVar(&o.time, "time-key", runview.DefaultLogKeys.Time, "with --log: the key holding the event time (RFC 3339, or epoch seconds)")
}

func (o deskLogOpts) keys() runview.LogKeys {
	return runview.LogKeys{Event: o.eventKey, Step: o.stepKey, Time: o.time}
}

func newDeskRunsCmd(dir *string) *cobra.Command {
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

Like status, reading the desk scans it, and a scan fixes the hash of a
hand-dropped item and moves a changed one to skipped/.

Exit codes: 0 listed; 1 a source could not be read (the rest are still
listed); 2 usage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := log.check(cmd, "runs"); err != nil {
				return err
			}
			return runDeskRuns(cmd, *dir, log, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`print [{"source","name","kind","live","exit","steps","done","failed","events","updated","partial"}]`)
	log.bind(cmd)
	return cmd
}

func newDeskShowCmd(dir *string) *cobra.Command {
	var asJSON, events bool
	var at int
	var log deskLogOpts
	cmd := &cobra.Command{
		Use:   "show <name> | show --log FILE",
		Short: "Show one run as a flow: steps, edges, durations; replay with --at",
		Long: `show prints one run as the visualizer draws it: each step with its state,
duration and the steps it waits on, then the run's exit. --events adds the
event timeline, one line per event. --at N replays: the state after the run's
first N events.

NAME is a desk item (NN-name). --log FILE reads a JSONL log instead: one
JSON object per line, whose --event-key (default "event") names the event.
A log has no step model, so it shows its events and no step state.

show is the run's shape and timeline. The item's record (WHAT, WHY, sha256,
times, skip reason, log path) is ` + "`desk status NAME`" + `, which show points to and
does not repeat.

Exit codes: 0 shown; 1 no such run, or it could not be read; 2 usage.`,
		Example: `  forgectl desk show 17-nightly --events
  forgectl desk show 17-nightly --at 4
  forgectl desk show --log /tmp/build/events.jsonl --event-key kind --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			switch {
			case (name == "") == (log.path == ""):
				return deskUsage("desk show: give a run name or --log FILE, not both")
			case cmd.Flags().Changed("at") && at < 0:
				return deskUsage("desk show: --at must be 0 or more")
			}
			if err := log.check(cmd, "show"); err != nil {
				return err
			}
			replay := -1
			if cmd.Flags().Changed("at") {
				replay = at
			}
			return runDeskShow(cmd, *dir, name, log, deskShowOpts{asJSON: asJSON, events: events, at: replay})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`print {"source","name","kind","live","exit","at","events_total","steps","edges","events","counts","partial","held","note"}`)
	cmd.Flags().BoolVar(&events, "events", false, "also print the event timeline (always in --json)")
	cmd.Flags().IntVar(&at, "at", 0, "replay: show the state after the run's first N events")
	log.bind(cmd)
	return cmd
}

// deskShowOpts are show's output choices. at is -1 for no replay.
type deskShowOpts struct {
	asJSON, events bool
	at             int
}

// check refuses a key flag given without --log, where nothing would read it,
// and an empty event key, which matches no line.
func (o deskLogOpts) check(cmd *cobra.Command, verb string) error {
	if o.path == "" {
		for _, f := range []string{"event-key", "step-key", "time-key"} {
			if cmd.Flags().Changed(f) {
				return deskUsage("desk %s: --%s is only read with --log", verb, f)
			}
		}
	}
	if o.eventKey == "" {
		return deskUsage("desk %s: --event-key must not be empty", verb)
	}
	return nil
}
