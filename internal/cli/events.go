// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/events"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/module"
)

// eventsModule declares `events`: the event log of the PRs Claude Code
// sessions watch. The poller turns GitHub changes into typed events; the c3
// mod reads a session's feed, and cadence-hooks reads a PR's last snapshot.
// See docs/commands/events.md.
var eventsModule = module.Manifest{
	Name: "events",
	Tier: module.TierExtension,
	New:  newEventsCmd,
}

// Injectable seams.
var (
	eventsStore = events.DefaultStore
	eventsNow   = time.Now
)

// eventsMinInterval is the shortest poll interval: each watched PR costs one
// GraphQL call per poll.
const eventsMinInterval = 30 * time.Second

func eventsUsage(format string, args ...any) error {
	return withJSONUsageCode(WithExitCode(fmt.Errorf(format, args...), exitUsage))
}

func newEventsCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Event log of the PRs your sessions watch",
		Long: `events turns changes on watched pull requests into typed events, so a
Claude Code session hears about them without polling.

  forgectl events watch --session ID <pr>...    a session watches PRs
  forgectl events unwatch --session ID [<pr>...] stop (every PR when none named)
  forgectl events poll [--once] [--interval 90s] read each watched PR, record changes
  forgectl events feed --session ID [--since N] a session's events after N
  forgectl events state <pr>                    a PR's last snapshot

A <pr> is owner/repo#N or a github.com pull request URL. Events carry ids,
states, SHAs and counts only, never a title, body or comment. A merged or
closed PR comes off every watch list once its event is written.`,
		Args:          safeRootArgs,
		RunE:          showRootHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SuggestionsMinimumDistance = 2
	cmd.AddCommand(newEventsWatchCmd(), newEventsUnwatchCmd(), newEventsPollCmd(deps), newEventsFeedCmd(), newEventsStateCmd())
	return cmd
}

func parseRefs(args []string) ([]events.Ref, error) {
	refs := make([]events.Ref, 0, len(args))
	for _, a := range args {
		r, err := events.ParseRef(a)
		if err != nil {
			return nil, eventsUsage("events: %v", err)
		}
		refs = append(refs, r)
	}
	return refs, nil
}

func sessionFlag(session string) error {
	if !events.ValidSession(session) {
		return eventsUsage("events: --session: %v", events.ErrBadSession)
	}
	return nil
}

func newEventsWatchCmd() *cobra.Command {
	var session string
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "watch --session ID <pr>...",
		Short:        "A session watches PRs",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := sessionFlag(session); err != nil {
				return err
			}
			refs, err := parseRefs(args)
			if err != nil {
				return err
			}
			st, err := eventsStore()
			if err != nil {
				return err
			}
			if err := st.Watch(session, refs); err != nil {
				return err
			}
			return printWatching(cmd.OutOrStdout(), st, session, asJSON)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the Claude Code session id (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the session's watch list as JSON")
	return cmd
}

func newEventsUnwatchCmd() *cobra.Command {
	var session string
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "unwatch --session ID [<pr>...]",
		Short:        "Stop watching PRs (all of them when none are named)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := sessionFlag(session); err != nil {
				return err
			}
			refs, err := parseRefs(args)
			if err != nil {
				return err
			}
			st, err := eventsStore()
			if err != nil {
				return err
			}
			if err := st.Unwatch(session, refs); err != nil {
				return err
			}
			return printWatching(cmd.OutOrStdout(), st, session, asJSON)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the Claude Code session id (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the session's watch list as JSON")
	return cmd
}

func printWatching(out io.Writer, st *events.Store, session string, asJSON bool) error {
	w, err := st.Watched()
	if err != nil {
		return err
	}
	refs := []string{}
	for _, r := range w[session] {
		refs = append(refs, r.String())
	}
	if asJSON {
		return writeJSON(out, map[string]any{"session": session, "watching": refs})
	}
	if len(refs) == 0 {
		_, err = fmt.Fprintln(out, "watching nothing")
		return err
	}
	for _, r := range refs {
		if _, err := fmt.Fprintln(out, r); err != nil {
			return err
		}
	}
	return nil
}

func newEventsPollCmd(deps module.Deps) *cobra.Command {
	var once, asJSON bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "poll [--once] [--interval 90s]",
		Short: "Read each watched PR and record what changed",
		Long: `poll reads every watched PR from GitHub, one GraphQL call each, and
records an event for each change: a push, checks finishing, a new
cadence-review marker at the head, draft to ready, the CITL label, a merge or
a close. Without --once it polls every --interval (at least 30s) until
interrupted; run it under launchd or a tmux pane. Each poll prints one line
per event, or with --json one object per poll: {"read", "skipped", "events"}.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < eventsMinInterval {
				return eventsUsage("events poll: --interval must be at least %s", eventsMinInterval)
			}
			host, err := githubauth.ResolveHost(deps.Cfg.Github.Host)
			if err != nil {
				return err
			}
			if host != githubauth.DefaultHost {
				// Refs carry no host: a watched owner/repo#N names a github.com PR.
				// Polling another host could store a same-numbered PR from there
				// under that ref.
				return eventsUsage("events poll: only github.com is supported; [github].host is set to another host")
			}
			st, err := eventsStore()
			if err != nil {
				return err
			}
			gh := &events.GitHub{Run: githubauth.Runner(deps.Runner, deps.Cfg.Github.Host), Reviewers: deps.Cfg.Sweep.Reviewers, Now: eventsNow}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			for {
				res, err := events.Poll(ctx, st, gh, eventsNow)
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					return err
				}
				if err := printPoll(cmd.OutOrStdout(), res, asJSON); err != nil {
					return err
				}
				if !asJSON && res.Skipped > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "events poll: could not read %d watched PR(s) from GitHub\n", res.Skipped) //nolint:errcheck // a diagnostic
				}
				if once {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "poll once and exit")
	cmd.Flags().DurationVar(&interval, "interval", 90*time.Second, "time between polls")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print each poll as one JSON object")
	return cmd
}

func printPoll(out io.Writer, res events.PollResult, asJSON bool) error {
	if asJSON {
		if res.Events == nil {
			res.Events = []events.Event{}
		}
		return writeJSON(out, res)
	}
	for _, e := range res.Events {
		if _, err := fmt.Fprintln(out, e.Line()); err != nil {
			return err
		}
	}
	return nil
}

func newEventsFeedCmd() *cobra.Command {
	var session string
	var since int64
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "feed --session ID [--since N]",
		Short: "A session's events after seq N",
		Long: `feed prints the session's events with seq above --since, oldest first, one
line each. --json prints {"events": [...], "last": N, "skipped": N}: last is
the highest seq in the feed, the value to pass as --since next time, and
skipped counts lines that did not parse and were left out.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := sessionFlag(session); err != nil {
				return err
			}
			if since < 0 {
				return eventsUsage("events feed: --since must not be negative")
			}
			st, err := eventsStore()
			if err != nil {
				return err
			}
			evs, last, skipped, err := st.Feed(session, since)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if evs == nil {
					evs = []events.Event{}
				}
				return writeJSON(out, map[string]any{"events": evs, "last": last, "skipped": skipped})
			}
			for _, e := range evs {
				if _, err := fmt.Fprintln(out, e.Line()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the Claude Code session id (required)")
	cmd.Flags().Int64Var(&since, "since", 0, "print only events with a higher seq")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the events as JSON")
	return cmd
}

// errNoState is the exit for `state` with no snapshot: not a failure of the
// command, an answer the caller (the merge gate) must treat as unknown.
const exitNoState = 3

func newEventsStateCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "state <pr>",
		Short: "A PR's last snapshot",
		Long: `state prints the poller's last snapshot of one PR: state, draft, head, the
head's check rollup, the cadence-review counts at the head, the CITL label,
the merge methods the repository allows, and polled_at. A PR the poller has
never read exits 3 with nothing on stdout, so a caller can tell "unknown"
from a failure.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			refs, err := parseRefs(args)
			if err != nil {
				return err
			}
			st, err := eventsStore()
			if err != nil {
				return err
			}
			snap, ok, err := st.State(refs[0])
			if err != nil {
				return err
			}
			if !ok {
				return WithExitCode(fmt.Errorf("events state: no snapshot of %s; is it watched and is the poller running?", refs[0]), exitNoState)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, snap)
			}
			review := "none"
			if r := snap.Review; r != nil {
				review = fmt.Sprintf("%d at head (crit=%d imp=%d), %d stale", r.AtHead, r.Crit, r.Imp, r.Stale)
			}
			_, err = fmt.Fprintf(out, "%s %s draft=%t head=%s checks=%s review=%s citl=%t merge=%v polled=%s\n",
				snap.Ref(), snap.State, snap.Draft, snap.Head, snap.Checks, review, snap.CITL, snap.MergeMethods, snap.PolledAt)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the snapshot as JSON")
	return cmd
}
