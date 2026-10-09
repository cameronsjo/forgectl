package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/procstart"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface drain start|stop|status|events` run and inspect the detached
// process that launches queued briefs as herdr workers. `surface
// _drain` is that process.

// drainStartWait is how long `drain start` waits for the child to report
// running. Tests shorten it.
var drainStartWait = 5 * time.Second

// herdrSessionEnv is the variable the herdr adapter resolves its session
// from; drain start sets it in the child to the session it resolved.
const herdrSessionEnv = "HERDR_SESSION"

// drainStartResult is what `drain start --json` prints. Additive changes only
// (ADR-0008 rule 2).
type drainStartResult struct {
	PID          int    `json:"pid"`
	ProcessStart int64  `json:"process_start"`
	HerdrSession string `json:"herdr_session"`
	Status       string `json:"status"`
}

// drainStopResult is what `drain stop --json` prints.
type drainStopResult struct {
	PID      int  `json:"pid"`
	Signaled bool `json:"signaled"`
}

// drainStatusView is what `drain status --json` prints.
type drainStatusView struct {
	Status          string            `json:"status"`
	PauseReason     string            `json:"pause_reason,omitempty"`
	PID             int               `json:"pid"`
	ProcessStart    int64             `json:"process_start"`
	HerdrSession    string            `json:"herdr_session,omitempty"`
	StartedAt       *time.Time        `json:"started_at,omitempty"`
	LastTick        *time.Time        `json:"last_tick,omitempty"`
	IntervalSeconds int64             `json:"interval_seconds"`
	Seq             int64             `json:"seq"`
	Counts          map[string]int    `json:"counts"`
	Attention       []drain.Attention `json:"attention"`
	// HeldSlots counts the rows holding a slot (drain.HoldsSlot), and
	// Holding names them, so a queue held back by failed rows whose workers
	// are still live is explainable: surface close frees each.
	HeldSlots int      `json:"held_slots"`
	Holding   []string `json:"holding"`
	Error     string   `json:"error,omitempty"`
}

// drainEventsResult is what `drain events --json` prints.
type drainEventsResult struct {
	Events []drain.Event `json:"events"`
}

func newSurfaceDrainCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Run the process that launches queued briefs as herdr workers",
		Long: `drain runs a detached process that launches the queue's briefs as herdr
workers, each with the harness its row names (claude, codex, or pi), at most [surface.drain] cap at a time (default 3) and per_repo
in one repository (default 1), and watches each worker: its REPORT line marks
the row reported, a permission prompt or other blocking screen marks it
needs-you, and a worker at its prompt with no report for idle_minutes is
needs-you too. The drain never types into a pane or answers a prompt.

Before each claude launch the drain runs claude-slots check N (found on PATH
at start, 5 s cap), where N is 1 plus the launches this tick already made: exit
1 puts the row back to queued and waits; a missing or failing claude-slots
launches without the session cap. A row's --profile is
resolved from the config file at launch; an unknown name pauses claiming.

  forgectl surface drain start
  forgectl surface drain status
  forgectl surface drain events --since 12
  forgectl surface drain stop

See docs/herdr.md, "Drain".`,
	}
	cmd.AddCommand(newSurfaceDrainStartCmd(deps), newSurfaceDrainStopCmd(), newSurfaceDrainStatusCmd(), newSurfaceDrainEventsCmd())
	return cmd
}

func newSurfaceDrainStartCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the drain in the background",
		Long: `start resolves the herdr session the way surface launch does, then starts the
drain as a detached process in a session of its own, with this command's
environment and HERDR_SESSION pinned to that session. It waits up to 5
seconds for drain.json to show the drain running (or paused); otherwise it
exits 1 and prints the last line of drain-events.jsonl.

A second start while a drain holds drain.lock exits 1, naming the holder's
pid and start time. An invalid [surface.drain] value is refused before
anything starts.

--json prints {"pid","process_start","herdr_session","status"}.

Exit 0: running. Exit 1: already running, or the child never reached
running. Exit 2: a usage or setup error (herdr not found, invalid config,
an unsupported platform, or a TMPDIR under which a launch cannot create its
private run directory).

  forgectl surface drain start --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceDrainStart(cmd, deps, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"pid","process_start","herdr_session","status"} as JSON`)
	return cmd
}

func runSurfaceDrainStart(cmd *cobra.Command, deps module.Deps, asJSON bool) error {
	if err := deps.Cfg.Surface.Drain.Validate(); err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	// Refuse a second drain here, where the operator sees it. The child
	// takes the lock for itself; this check only says who holds it.
	lock, err := files.Lock()
	if errors.Is(err, worker.ErrDrainLocked) {
		return termsafe.Error(drainHolderError(files))
	}
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	if err := lock.Close(); err != nil {
		return err
	}
	if err := drainRunDirCheck(); err != nil {
		return WithExitCode(termsafe.Error(runDirRefusal(err)), exitUsage)
	}
	adapter, err := newHerdrAdapter(cmd.ErrOrStderr())
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return errors.New("forgectl: the herdr adapter has an unexpected type")
	}
	herdrPath, err := lookHerdr()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	pid, err := drainSpawn(drainChildArgv(herdr.Session(), herdrPath), pinnedDrainEnv(os.Environ(), herdr.Session()))
	if err != nil {
		return WithExitCode(fmt.Errorf("forgectl: start the drain: %w", err), exitUsage)
	}
	st, ok := waitDrainRunning(files, pid, drainStartWait)
	if !ok {
		return termsafe.Error(fmt.Errorf("the drain (pid %d) did not report running within %s; last event: %s", pid, drainStartWait, lastDrainEvent(files)))
	}
	res := drainStartResult{PID: st.PID, ProcessStart: st.ProcessStart, HerdrSession: st.HerdrSession, Status: st.Status}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), res)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "drain %s: pid %d, herdr session %s\n", res.Status, res.PID, termsafe.SafeLineMax(res.HerdrSession, 64))
	return err
}

// runDirRefusal names TMPDIR in a run-directory failure, since TMPDIR is what
// chose the base and what the operator changes.
func runDirRefusal(err error) error {
	tmpdir := "TMPDIR is unset"
	if v, ok := os.LookupEnv("TMPDIR"); ok {
		tmpdir = "TMPDIR is " + termsafe.QuotePath(v)
	}
	return fmt.Errorf("forgectl: the drain would fail every launch: %s, and a launch cannot create its private run directory under %s: %w; "+
		"set TMPDIR to a private directory that is not a symlink, then run drain start again", tmpdir, termsafe.QuotePath(os.TempDir()), err)
}

// lookHerdr is the absolute herdr path, resolved as newHerdrAdapter does.
func lookHerdr() (string, error) {
	path, err := osexec.LookPath("herdr")
	if err != nil {
		return "", fmt.Errorf("%w: herdr not found on PATH: %w", errBackendUnavailable, err)
	}
	return filepath.Abs(path)
}

func drainChildArgv(session, herdrPath string) []string {
	return []string{"surface", "_drain", "--session", session, "--herdr-path", herdrPath}
}

// pinnedDrainEnv is env with HERDR_SESSION set to session, so the child's
// adapter resolves the session start resolved even when it came from the
// default.
func pinnedDrainEnv(env []string, session string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if len(e) > len(herdrSessionEnv) && e[:len(herdrSessionEnv)+1] == herdrSessionEnv+"=" {
			continue
		}
		out = append(out, e)
	}
	return append(out, herdrSessionEnv+"="+session)
}

// waitDrainRunning polls drain.json until it shows pid running or paused.
func waitDrainRunning(files worker.DrainFiles, pid int, wait time.Duration) (drain.Status, bool) {
	deadline := time.Now().Add(wait)
	for {
		if data, err := files.ReadStatus(); err == nil && data != nil {
			if st, err := drain.DecodeStatus(data); err == nil && st.PID == pid &&
				(st.Status == drain.StatusRunning || st.Status == drain.StatusPaused) {
				return st, true
			}
		}
		if time.Now().After(deadline) {
			return drain.Status{}, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lastDrainEvent renders the events file's last line, or says there is none.
func lastDrainEvent(files worker.DrainFiles) string {
	older, current, err := files.ReadEvents()
	if err != nil {
		return "the events file could not be read: " + err.Error()
	}
	events, err := drain.ParseEvents(older, current)
	if len(events) == 0 {
		if err != nil {
			return err.Error()
		}
		return "none"
	}
	return drainEventLine(events[len(events)-1])
}

// drainHolderError names the drain that holds the lock, from drain.json.
func drainHolderError(files worker.DrainFiles) error {
	data, err := files.ReadStatus()
	if err != nil || data == nil {
		return errors.New("a drain already holds drain.lock (drain.json is unreadable, so its pid is unknown)")
	}
	st, err := drain.DecodeStatus(data)
	if err != nil {
		return fmt.Errorf("a drain already holds drain.lock (%v)", err)
	}
	return fmt.Errorf("a drain is already running: pid %d, process start time %d, started %s", st.PID, st.ProcessStart, st.StartedAt.UTC().Format(time.RFC3339))
}

func newSurfaceDrainStopCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Ask the running drain to stop after its current step",
		Long: `stop sends SIGTERM to the drain named in drain.json, only when its recorded
process start time is non-zero and equals the live process's start time: a
pid that was reused, or one whose start time cannot be read, is refused.
The drain finishes its current step (a launch in progress completes) and
exits, recording status stopped.

--json prints {"pid","signaled"}.

Exit 0: signaled. Exit 1: refused (no drain running, or the pid does not
match). Exit 2: drain.json cannot be read.

  forgectl surface drain stop`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceDrainStop(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"pid","signaled"} as JSON`)
	return cmd
}

func runSurfaceDrainStop(cmd *cobra.Command, asJSON bool) error {
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	data, err := files.ReadStatus()
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	if data == nil {
		return errors.New("no drain is running: there is no drain.json")
	}
	st, err := drain.DecodeStatus(data)
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	if err := checkDrainSignalable(st, procstart.Of); err != nil {
		return termsafe.Error(err)
	}
	if err := drainSignal(st.PID); err != nil {
		return fmt.Errorf("signal drain pid %d: %w", st.PID, err)
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), drainStopResult{PID: st.PID, Signaled: true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "sent SIGTERM to drain pid %d; it stops after its current step\n", st.PID)
	return err
}

// checkDrainSignalable refuses a signal unless drain.json names a drain that
// has not recorded stopping, with a non-zero start time equal to the live
// process's. startOf reads a live start time.
func checkDrainSignalable(st drain.Status, startOf func(int) (int64, error)) error {
	switch {
	case st.Status == drain.StatusStopped:
		return fmt.Errorf("no drain is running: drain.json says stopped (pid %d)", st.PID)
	case st.PID <= 0:
		return fmt.Errorf("drain.json names no pid (saw %d)", st.PID)
	case st.ProcessStart == 0:
		return fmt.Errorf("drain.json records no process start time for pid %d; refusing to signal a pid that cannot be checked", st.PID)
	}
	live, err := startOf(st.PID)
	if err != nil {
		return fmt.Errorf("pid %d: expected process start time %d, could not read the live one: %w", st.PID, st.ProcessStart, err)
	}
	if live != st.ProcessStart {
		return fmt.Errorf("pid %d is another process now: expected process start time %d, saw %d", st.PID, st.ProcessStart, live)
	}
	return nil
}

func newSurfaceDrainStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the drain runs, and the rows that need you",
		Long: `status prints the drain's state from drain.json: running, paused (with the
pause reason), stopped, or stale (running or paused, but the last tick is
older than 3 intervals). It names the pid, process start time, herdr
session and last tick, counts the queue's rows per state, lists the rows
in needs-you or failed with their last error, and names the rows holding a
slot (a failed row whose worker is still live holds one until surface
close). Counts and rows are read from the queue now, not from the last tick.

--json prints {"status","pause_reason","pid","process_start","herdr_session",
"started_at","last_tick","interval_seconds","seq","counts","attention",
"held_slots","holding","error"}, where attention is
[{"name","repo","state","last_error","state_at"}].

Exit 0: printed, whatever the state. Exit 2: drain.json or the queue cannot
be read.

  forgectl surface drain status --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceDrainStatus(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"status","pause_reason","pid","process_start","herdr_session","started_at","last_tick","interval_seconds","seq","counts","attention","held_slots","holding","error"} as JSON`)
	return cmd
}

func runSurfaceDrainStatus(cmd *cobra.Command, asJSON bool) error {
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	data, err := files.ReadStatus()
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	q, err := worker.OpenQueue()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	rows, err := q.Rows()
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	view := drainStatusView{Status: drain.StatusStopped}
	if data != nil {
		st, err := drain.DecodeStatus(data)
		if err != nil {
			return WithExitCode(termsafe.Error(err), exitUsage)
		}
		view = drainStatusView{
			Status: drain.Effective(st, time.Now()), PauseReason: st.PauseReason, PID: st.PID, ProcessStart: st.ProcessStart,
			HerdrSession: st.HerdrSession, StartedAt: timePtr(st.StartedAt), LastTick: timePtr(st.LastTick),
			IntervalSeconds: st.IntervalSeconds, Seq: st.Seq, Error: st.Error,
		}
	}
	view.Counts, view.Attention = drain.Summarize(rows)
	view.Holding = drain.Held(rows, readLedgers(rows, view.HerdrSession, func(repo, session string) ([]worker.Row, error) {
		led, err := worker.Open(repo, session)
		if err != nil {
			return nil, err
		}
		return led.Rows()
	}))
	view.HeldSlots = len(view.Holding)
	return reportDrainStatus(cmd.OutOrStdout(), view, asJSON)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func reportDrainStatus(out io.Writer, v drainStatusView, asJSON bool) error {
	if asJSON {
		return writeJSON(out, v)
	}
	line := "drain " + termsafe.SafeLineMax(v.Status, 16)
	if v.PID > 0 && v.Status != drain.StatusStopped {
		line += fmt.Sprintf(": pid %d, herdr session %s", v.PID, termsafe.SafeLineMax(v.HerdrSession, 64))
	}
	if v.LastTick != nil {
		line += ", last tick " + v.LastTick.UTC().Format(time.RFC3339)
	}
	if _, err := fmt.Fprintln(out, line); err != nil {
		return err
	}
	if v.PauseReason != "" {
		if _, err := fmt.Fprintf(out, "paused: %s\n", termsafe.SafeLineMax(v.PauseReason, 400)); err != nil {
			return err
		}
	}
	if v.Error != "" {
		if _, err := fmt.Fprintf(out, "last error: %s\n", termsafe.SafeLineMax(v.Error, 400)); err != nil {
			return err
		}
	}
	states := []worker.QueueState{worker.QueueQueued, worker.QueueClaimed, worker.QueueLaunched, worker.QueueNeedsYou,
		worker.QueueReported, worker.QueueFailed, worker.QueueClosed, worker.QueueExpired}
	counts := ""
	for _, s := range states {
		if n := v.Counts[string(s)]; n > 0 {
			counts += fmt.Sprintf(" %s=%d", s, n)
		}
	}
	if counts == "" {
		counts = " none"
	}
	if _, err := fmt.Fprintf(out, "rows:%s\n", counts); err != nil {
		return err
	}
	if v.HeldSlots > 0 {
		names := make([]string, 0, len(v.Holding))
		for _, n := range v.Holding {
			names = append(names, termsafe.SafeLineMax(n, 64))
		}
		if _, err := fmt.Fprintf(out, "slots held: %d (%s)\n", v.HeldSlots, strings.Join(names, ", ")); err != nil {
			return err
		}
	}
	for _, a := range v.Attention {
		if _, err := fmt.Fprintf(out, "  %s  %s  %s\n", termsafe.SafeLineMax(a.Name, 64), termsafe.SafeLineMax(a.State, 16), termsafe.SafeLineMax(a.LastError, 300)); err != nil {
			return err
		}
	}
	return nil
}

func newSurfaceDrainEventsCmd() *cobra.Command {
	var (
		asJSON bool
		since  int64
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Print the drain's events: state changes, pauses and resumes",
		Long: `events prints drain-events.jsonl (and the rotated drain-events.jsonl.1 before
it), oldest first: one event per row state change, pause, resume, drain start
and stop, a row becoming unreadable, claude-slots holding launches
(slots-held), a note such as claude-slots missing, and with [surface.merge]
mode auto the autopilot's merges (merged) and refusals (merge-refused, once
per row, head and reasons). seq counts from 1 in each drain
process, so --since <seq> prints only the latest run's events after that
seq; a cursor resets when the drain restarts.

--json prints {"events":[{"v","ts","seq","kind","name","repo","state",
"attempt","error"}]}.

Exit 0: printed. Exit 2: the events file cannot be read or does not parse.

  forgectl surface drain events
  forgectl surface drain events --since 40 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceDrainEvents(cmd, since, asJSON)
		},
	}
	cmd.Flags().Int64Var(&since, "since", 0, "print only the latest drain run's events after this seq")
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"events":[{"v","ts","seq","kind","name","repo","state","attempt","error"}]} as JSON`)
	return cmd
}

func runSurfaceDrainEvents(cmd *cobra.Command, since int64, asJSON bool) error {
	if since < 0 {
		return WithExitCode(fmt.Errorf("--since must be 0 or more, got %d", since), exitUsage)
	}
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	older, current, err := files.ReadEvents()
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	events, err := drain.ParseEvents(older, current)
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	events = drain.Since(events, since)
	if asJSON {
		if events == nil {
			events = []drain.Event{}
		}
		return writeJSON(cmd.OutOrStdout(), drainEventsResult{Events: events})
	}
	for _, e := range events {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), drainEventLine(e)); err != nil {
			return err
		}
	}
	return nil
}

// drainEventLine renders one event for a terminal, every field escaped.
func drainEventLine(e drain.Event) string {
	line := fmt.Sprintf("%s #%d %s", e.TS.UTC().Format(time.RFC3339), e.Seq, termsafe.SafeLineMax(e.Kind, 16))
	if e.Name != "" {
		line += " " + termsafe.SafeLineMax(e.Name, 64)
	}
	if e.State != "" {
		line += " " + termsafe.SafeLineMax(e.State, 32)
	}
	if e.Attempt > 0 {
		line += fmt.Sprintf(" attempt=%d", e.Attempt)
	}
	if e.Error != "" {
		line += ": " + termsafe.SafeLineMax(e.Error, 400)
	}
	return line
}

// newSurfaceDrainProcessCmd builds the hidden `surface _drain`: the drain
// process itself, started by drain start. Running it by hand grants nothing
// drain start does not. Hidden is presentation, not a control.
func newSurfaceDrainProcessCmd(deps module.Deps) *cobra.Command {
	var session, herdrPath string
	cmd := &cobra.Command{
		Use:          "_drain --session <name> --herdr-path <path>",
		Short:        "Internal: the surface drain process",
		Hidden:       true,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDrainProcess(context.Background(), deps, session, herdrPath)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the herdr session drain start resolved")
	cmd.Flags().StringVar(&herdrPath, "herdr-path", "", "the herdr binary drain start resolved")
	return cmd
}

// drainSessionPin checks the child resolves what drain start resolved.
func drainSessionPin(session, herdrPath string) error {
	if session == "" || herdrPath == "" {
		return errors.New("_drain needs --session and --herdr-path; run surface drain start")
	}
	adapter, err := newHerdrAdapter(io.Discard)
	if err != nil {
		return err
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return errors.New("forgectl: the herdr adapter has an unexpected type")
	}
	if herdr.Session() != session {
		return fmt.Errorf("the herdr session resolves to %q, expected the pinned %q", herdr.Session(), session)
	}
	path, err := lookHerdr()
	if err != nil {
		return err
	}
	if path != herdrPath {
		return fmt.Errorf("herdr resolves to %s, expected the pinned %s", path, herdrPath)
	}
	return nil
}

// drainLoadConfig reloads the config file with the normal loader. A file
// that failed to decode is an error, never a partial Config.
func drainLoadConfig() (config.Config, error) {
	cfg := config.Load()
	if err := cfg.DecodeError(); err != nil {
		return config.Config{}, err
	}
	if cfg.DecodeDegraded() {
		return config.Config{}, errors.New("the config file could not be located or read")
	}
	return cfg, nil
}
