package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// restartEnvFn builds the RestartEnv a run acts through — a seam so a cli test
// drives the whole verb against a fake instead of real signals and herdr.
var restartEnvFn = func(paths resume.Paths, deps module.Deps, forgectl string) resume.RestartEnv {
	return resume.SystemRestartEnv{
		Paths:  paths,
		Runner: deps.Runner,
		RelaunchLine: func(sessionID string) (string, error) {
			return surface.QuoteCommand([]string{forgectl, "resume", sessionID})
		},
	}
}

// restartPaneLookup reads each session's HERDR_PANE_ID; a seam so a cli test
// does not pick up the pane of whatever terminal runs `go test`.
var restartPaneLookup = resume.PaneFor

// relaunchBinaryFn resolves the forgectl typed into each pane; a seam for the
// same reason.
var relaunchBinaryFn = relaunchBinary

// relaunchBinary picks the forgectl binary each pane will run.
//
// The running binary (os.Executable, unresolved, so a Homebrew link stays a
// link that survives an upgrade) is the first choice: it is the build that
// chose the sessions. The one exception is a `go run` build, which lives in a
// go-build temp directory that is deleted the moment this process exits —
// typed into a pane, it would name a file that no longer exists. That case
// falls back to the forgectl on PATH, looked up here and passed absolute, so
// the pane's own PATH is never consulted.
func relaunchBinary() (string, error) {
	exe, err := os.Executable()
	return pickRelaunchBinary(exe, err, osexec.LookPath)
}

// pickRelaunchBinary is relaunchBinary's decision, with its two inputs passed
// in: a test binary itself lives under go-build, so the real os.Executable
// cannot exercise the first branch.
func pickRelaunchBinary(exe string, exeErr error, lookPath func(string) (string, error)) (string, error) {
	if exeErr == nil && filepath.IsAbs(exe) && !strings.Contains(filepath.ToSlash(exe), "/go-build") {
		return exe, nil
	}
	path, err := lookPath("forgectl")
	if err != nil {
		return "", errors.New("find a forgectl binary for the panes to run: this one is a temporary `go run` build and none is on PATH")
	}
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		return "", fmt.Errorf("resolve %s: %w", safeTerm(path), aerr)
	}
	return abs, nil
}

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
     horizontal rules; a screen that does not look like that counts as a draft.

Failing 2 or 4 waits and re-checks every few seconds, up to --timeout (default
30m). Failing 1 or 3 is reported and never signalled. A session with no herdr
pane in its environment, or whose version cannot be compared, is reported and
left alone.

Before the stop, the same capture as ` + "`resume snapshot`" + ` runs, so the resumed
session gets its /rename name and task bodies back. The stop is SIGTERM. The
relaunch waits until the pid has exited, its registry file is gone, and the
pane's shell holds the foreground again, then types the resume command into
the pane and presses Enter; it then waits for the session to register again.
Nothing is relaunched unless the old process is confirmed gone.

The pane runs this forgectl by absolute path (os.Executable). A ` + "`go run`" + ` build
is deleted when this process exits, so under ` + "`go run`" + ` the forgectl on PATH is
used instead.

Ctrl-C stops the waiting, never a restart already signalled: that session is
still relaunched and confirmed, so no session is left stopped without a resume
or a report naming the command to run by hand.

Output is one line per session per state change: waiting (with the reason),
restarting, resumed, skipped, failed, and left (still waiting at the timeout
or Ctrl-C; never signalled). --dry-run prints each session's planned action
and what the checks say right now, using reads only.

Exit 0 when every selected session was resumed or skipped with a reason
(skips are the safety checks working, not errors). Exit 1 when a stop or
relaunch failed, or the timeout or Ctrl-C left sessions waiting. Exit 2 on
bad usage.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !outdated {
				return WithExitCode(errors.New("pass --outdated: it is the only selector `resume restart` has"), 2)
			}
			for _, id := range only {
				if !resume.ValidSessionID(id) {
					return WithExitCode(fmt.Errorf("--session %s is not a session id", safeTerm(id)), 2)
				}
			}
			if timeout <= 0 {
				return WithExitCode(errors.New("--timeout must be positive"), 2)
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
	list, err := resume.Outdated(paths, installed, restartPaneLookup())
	if err != nil {
		return err
	}
	plan := resume.PlanRestart(list, only)
	if len(plan) == 0 {
		_, err := fmt.Fprintln(out, "no outdated sessions")
		return err
	}

	forgectl := ""
	if !dryRun {
		if forgectl, err = relaunchBinaryFn(); err != nil {
			return err
		}
	}
	env := restartEnvFn(paths, deps, forgectl)

	if dryRun {
		return printRestartPlan(ctx, out, env, plan)
	}

	// Ctrl-C and SIGTERM cancel the waiting. stop() runs only on return, so a
	// second Ctrl-C during an in-flight restart is absorbed rather than killing
	// this process between the stop and the relaunch.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	finals := resume.RunRestart(ctx, env, plan, resume.RestartOptions{
		Timeout:  timeout,
		Progress: func(ev resume.RestartEvent) { printRestartEvent(out, ev) },
	})
	var failed, left int
	for _, ev := range finals {
		switch ev.State {
		case resume.StateFailed:
			failed++
		case resume.StateLeft:
			left++
		}
	}
	if failed > 0 || left > 0 {
		return WithExitCode(fmt.Errorf("%d session(s) failed, %d left waiting — each is listed above with the command to resume it by hand", failed, left), 1)
	}
	return nil
}

// printRestartEvent renders one progress line. Every field can carry
// registry- or herdr-derived text, so the whole line goes through safeTerm.
func printRestartEvent(out io.Writer, ev resume.RestartEvent) {
	line := fmt.Sprintf("%-10s %s  %s", ev.State, ev.SessionID, ev.Detail)
	if ev.Manual != "" {
		line += "; by hand: " + ev.Manual
	}
	_, _ = fmt.Fprintln(out, safeTerm(line))
}

// printRestartPlan is --dry-run: each session's planned action, and for the
// ones it would restart, what the predicate says right now. Reads only.
func printRestartPlan(ctx context.Context, out io.Writer, env resume.RestartEnv, plan []resume.RestartPlanItem) error {
	for _, item := range plan {
		action, detail := "skip", item.Reason
		switch item.Action {
		case resume.ActionManual:
			action = "manual"
		case resume.ActionRestart:
			c := resume.Preview(ctx, env, item.Session)
			switch c.Readiness {
			case resume.Ready:
				action, detail = "restart", "now: "+c.Reason
			case resume.NotYet:
				action, detail = "wait", c.Reason
			default:
				action, detail = "refuse", c.Reason+"; by hand: "+resume.ManualResume(item.SessionID)
			}
			if item.Session.Pane != "" {
				detail += " (pane " + item.Session.Pane + ", pid " + fmt.Sprint(item.Session.Pid) + ")"
			}
		}
		if _, err := fmt.Fprintln(out, safeTerm(fmt.Sprintf("%-8s %s  %s", action, item.SessionID, detail))); err != nil {
			return err
		}
	}
	return nil
}
