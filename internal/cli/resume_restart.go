package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

// restartOverride lets a cli test replace the run's seams (env, binary, pane
// lookup, ancestors, clock) before it starts; nil in production.
var restartOverride func(*resume.RestartRequest)

// newResumeRestartCmd builds `forgectl resume restart --outdated`.
func newResumeRestartCmd(deps module.Deps) *cobra.Command {
	var outdated, dryRun bool
	var only []string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "restart --outdated",
		Short: "Restart outdated Claude Code sessions in their herdr panes, once each is idle",
		Long: `restart --outdated moves every session ` + "`forgectl resume outdated`" + ` lists onto the
installed claude: it stops each one and resumes it in the same herdr pane with
` + "`forgectl resume <id>`" + `, so the configured launch profile and cwd apply and the
conversation comes back. --outdated is required; it is the only selector today.

Each session's pane is found by session id: one ` + "`herdr pane list`" + ` per run, and
the pane herdr labels "claude" with that session id is the one checked and
relaunched into. The process's HERDR_PANE_ID is the fallback, used when no
pane carries the session or the list fails or is rejected; a "note" line then
says so. A herdr server restart renumbers every pane, so a long-running
session's HERDR_PANE_ID can name a pane that no longer exists. A pane that
differs from the environment's shows as "pane <found> (found by session; env
said <old>)". A session herdr labels in two panes is refused. When herdr
answers "pane not found" for a session, the list is read once more for that
session, and if another pane now holds it the checks continue there. The
label alone is never trusted: check 3 below still runs against the found
pane.

  forgectl resume restart --outdated --dry-run        show the plan, touch nothing
  forgectl resume restart --outdated                  restart all, waiting for each to be idle
  forgectl resume restart --outdated --session <id>   restart only that session (repeatable)

A session is stopped only when all of these hold, checked again immediately
before the signal:

  1. its pid is alive and is still the claude that wrote its registry file:
     same session id and procStart, a claude executable, and a kernel start
     time matching procStart (a pid can be reused; a start time cannot);
  2. its registry status is exactly "idle" — busy, waiting on a permission
     prompt, or running a background shell (which a stop would kill) all wait;
  3. herdr confirms the pane: ` + "`herdr pane get`" + ` names the same session, and the
     pid is the pane's foreground process (a nested claude inherits its
     parent's HERDR_PANE_ID);
  4. the pane's input line is empty. "idle" says nothing about an unsent
     draft, and stopping drops one for good. The line is read off the screen
     (` + "`herdr pane read --source visible`" + `) as the "❯" line between Claude Code's two
     horizontal rules; a screen that does not look like that counts as a draft,
     and so does any text after the "❯" — including a greyed suggestion,
     which plain screen text cannot tell from typed input (it only delays).

Failing 2 or 4 waits and re-checks every few seconds, up to --timeout (default
30m). Failing 1 or 3 is reported and never signalled; a herdr error other than
"pane not found" waits instead. A session with no herdr pane (none found by
session and none in its environment), whose version cannot be compared, or
that this run is itself running inside (its pid is an ancestor of this
process, as when an agent's shell tool runs the command) is reported and left
alone.

Keystrokes typed into a session in the instant between the last screen read
and the signal cannot be seen and are lost with the process; the window is a
few milliseconds.

Before the stop, the same capture as ` + "`resume snapshot`" + ` runs, so the resumed
session gets its /rename name and task bodies back, and the relaunch command
is rendered, so nothing that could fail it is left for after the signal. The
stop is SIGTERM. The relaunch waits until the pid has exited, its registry
file is gone, and the same shell that owned the pane before the stop holds
the foreground again. It then refuses if the session is already running
again (resumed elsewhere in the gap), sends Ctrl-U to clear anything typed at
the shell prompt meanwhile, checks the shell still holds the foreground, and
types the resume command into the pane with Enter; it then waits for the
session to register again. Nothing is relaunched unless the old process is
confirmed gone.

A run holds an exclusive lock (restart.lock beside forgectl's snapshot
store), so a second run exits at once instead of acting on the same sessions.

The pane runs this forgectl by absolute path (os.Executable). A ` + "`go run`" + ` build
is deleted when this process exits, so under ` + "`go run`" + ` the forgectl on PATH is
used instead.

Ctrl-C, SIGTERM, and SIGHUP (a closed terminal) stop the waiting, never a
restart already signalled: that session is still relaunched and confirmed.
SIGPIPE is ignored, so a broken output pipe cannot kill the run either. A
failed stop or relaunch is reported with the command to run by hand. Each
herdr call the run makes is in a process group of its own, so neither a
closed terminal's hangup nor Ctrl-C reaches a call in flight; only the
progress lines are lost with the terminal. Each call is bounded at 10
seconds instead, and one that runs past it is killed, so a wedged herdr
after the stop is reported as a failure with the command to resume it by
hand. The exception is the relaunch itself: a send killed at the bound may
already have typed the line, so the run still waits for the session to
register, and reports it resumed if it does or reports delivery as unknown
(check the pane before resuming by hand) if it does not.

Output is one line per session per state change: waiting (with the reason),
restarting, resumed, skipped, failed, left (still waiting at the timeout or
Ctrl-C; never signalled), and pane-gone (herdr has no pane for it, even after
reading the list again; never signalled), plus a run-level note when herdr's
pane list cannot be used. --dry-run prints each session's planned action
and what the checks say right now, using reads only.

Exit 0 when every selected session was resumed or skipped with a reason
(skips are the safety checks working, not errors). Exit 1 when a stop or
relaunch failed, the timeout or Ctrl-C left sessions waiting, or a session's
pane could not be found (it can reappear under a new id, so the update
watcher retries it). Exit 2 on bad usage.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !outdated {
				return WithExitCode(errors.New("pass --outdated: it is the only selector `resume restart` has"), exitUsage)
			}
			for _, id := range only {
				if !resume.ValidSessionID(id) {
					return WithExitCode(fmt.Errorf("--session %s is not a session id", safeLabel(id)), exitUsage)
				}
			}
			if timeout <= 0 {
				return WithExitCode(errors.New("--timeout must be positive"), exitUsage)
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runResumeRestart(ctx, cmd.OutOrStdout(), deps, only, dryRun, timeout)
		},
	}
	cmd.Flags().BoolVar(&outdated, "outdated", false, "select the sessions `resume outdated` lists (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print each session's planned action and current check result; signal and send nothing")
	cmd.Flags().StringArrayVar(&only, "session", nil, "restrict to this session id (repeatable)")
	cmd.Flags().DurationVar(&timeout, "timeout", resume.DefaultRestartTimeout, "how long to keep re-checking sessions that are busy or hold a draft")
	return cmd
}

func runResumeRestart(ctx context.Context, out io.Writer, deps module.Deps, only []string, dryRun bool, timeout time.Duration) error {
	paths, err := resumePaths()
	if err != nil {
		return fmt.Errorf("locate session records: %w", err)
	}
	installed, err := installedVersionFn(ctx, deps)
	if err != nil {
		return err
	}
	req := resume.RestartRequest{
		Paths: paths, Installed: installed, Only: only, DryRun: dryRun,
		Runner:        deps.Runner,
		Options:       resume.RestartOptions{Timeout: timeout},
		Progress:      func(ev resume.RestartEvent) { printRestartEvent(out, ev) },
		HandleSignals: true,
	}
	if restartOverride != nil {
		restartOverride(&req)
	}
	res, err := resume.RestartOutdated(ctx, req)
	if err != nil {
		return err
	}
	if dryRun {
		return printRestartPreview(out, res.Preview)
	}
	if len(res.Finals) == 0 {
		_, err := fmt.Fprintln(out, "no outdated sessions")
		return err
	}
	if res.Incomplete() {
		return WithExitCode(fmt.Errorf("%d session(s) failed, %d left waiting, %d with no herdr pane — each is listed above with the command to resume it by hand", res.Failed, res.Left, res.PaneGone), exitFailed)
	}
	return nil
}

// printRestartEvent renders one progress line. Every field can carry
// registry- or herdr-derived text, so the whole line goes through safeText.
func printRestartEvent(out io.Writer, ev resume.RestartEvent) {
	line := fmt.Sprintf("%-10s %s  %s", ev.State, ev.SessionID, ev.Detail)
	if ev.SessionID == "" {
		// A run-level line (StateNote) names no session.
		line = fmt.Sprintf("%-10s %s", ev.State, ev.Detail)
	}
	if ev.Manual != "" {
		line += "; by hand: " + ev.Manual
	}
	// Dropped rather than returned: a closed terminal or broken pipe (SIGPIPE
	// is ignored for the run) must not stop the run, and a run mid-restart is
	// still bound to relaunch and confirm whether anyone reads the line.
	_, _ = fmt.Fprintln(out, safeText(line))
}

// printRestartPreview renders --dry-run.
func printRestartPreview(out io.Writer, lines []resume.PreviewLine) error {
	if len(lines) == 0 {
		_, err := fmt.Fprintln(out, "no outdated sessions")
		return err
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, safeText(fmt.Sprintf("%-8s %s  %s", l.Action, l.SessionID, l.Detail))); err != nil {
			return err
		}
	}
	return nil
}
