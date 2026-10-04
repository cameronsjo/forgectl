package resume

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// RestartRequest is one `restart --outdated` run. The zero value of every seam
// field means production; tests replace them.
type RestartRequest struct {
	Paths     Paths
	Installed string
	// Only restricts the run to these session ids (each already validated).
	Only   []string
	DryRun bool
	Runner exec.Runner
	// HerdrBin, when set, is the absolute herdr binary every herdr call
	// runs, instead of `herdr` from PATH — for a caller like the launchd
	// watcher whose PATH is not the operator's.
	HerdrBin string
	// Progress receives one event per session per state change.
	Progress func(RestartEvent)
	// Options bounds the run; zero fields take the Default* values.
	Options RestartOptions
	// HandleSignals makes the run own SIGINT, SIGTERM, and SIGHUP (each
	// cancels the waiting, never a restart in flight) and ignore SIGPIPE, so a
	// closed terminal or a broken output pipe cannot kill the process between
	// a stop and its relaunch. A caller that already owns signals leaves it off.
	HandleSignals bool

	Lookup func() PaneLookup
	// Panes reads herdr's pane list, which finds each session's pane by its
	// session id; Lookup's environment value is the fallback.
	Panes     func(ctx context.Context) ([]HerdrPane, error)
	Binary    func() (string, error)
	Env       func(forgectl string) (RestartEnv, error)
	Ancestors func() map[int]bool
}

// PreviewLine is one --dry-run row: the action a run would take now.
type PreviewLine struct {
	SessionID string
	Action    string // restart | wait | refuse | manual | skip
	Detail    string
}

// RestartResult is what a run did.
type RestartResult struct {
	// Preview is set on a dry run, Finals on a real one.
	Preview []PreviewLine
	Finals  []RestartEvent
	// Failed counts stops or relaunches that went wrong; Left counts sessions
	// still waiting when the timeout or a cancel ended the run; PaneGone counts
	// sessions refused because herdr has no pane for them (StatePaneGone).
	Failed, Left, PaneGone int
}

// Incomplete reports whether the run owes the operator attention: a failed
// stop or relaunch, sessions it gave up waiting on, or sessions whose pane
// herdr could not find. Other skips are not incomplete — they are the safety
// checks working. A missing pane is counted because it can be temporary (a
// herdr restart renumbers panes), and an incomplete run is what brings the
// update watcher back for another attempt.
func (r RestartResult) Incomplete() bool { return r.Failed > 0 || r.Left > 0 || r.PaneGone > 0 }

// ErrRestartBusy reports that another restart run holds the lock.
var ErrRestartBusy = errors.New("another `forgectl resume restart` is running")

// restartLockName is the run lock, beside the snapshot store: forgectl's own
// state, never a file inside Claude Code's directories.
const restartLockName = "restart.lock"

// RestartOutdated plans and carries out one run over the sessions Outdated
// lists, and is the one entry point a caller (the CLI, a hook runner) needs.
//
// A real run holds an exclusive lock for its whole life, so two runs — a
// manual one and a watcher's — can never act on the same session at once. A
// session this process is running inside (an ancestor pid, as when an
// agent's shell tool runs the command) is never stopped: stopping it would
// kill this run before it could relaunch.
func RestartOutdated(ctx context.Context, req RestartRequest) (RestartResult, error) {
	if req.HerdrBin != "" && !filepath.IsAbs(req.HerdrBin) {
		return RestartResult{}, errors.New("the herdr binary must be an absolute path")
	}
	req.fill()
	list, err := Outdated(req.Paths, req.Installed, req.Lookup())
	if err != nil {
		return RestartResult{}, err
	}
	// One pane list per run, read before planning. A pane moves only when the
	// herdr server restarts, so the list is not re-read every poll; it is
	// re-read once for a session whose pane herdr then reports gone (see
	// checkSession), which covers a herdr restart mid-run.
	var ambiguous map[string][]string
	if len(list) > 0 {
		panes, listErr := req.Panes(ctx)
		list, ambiguous = resolvePanes(list, panes, listErr)
		if listErr != nil {
			req.Progress(RestartEvent{State: StateNote, Detail: paneListProblem(listErr) + "; each session's pane comes from its environment"})
		}
	}
	plan := SkipAncestors(SkipAmbiguousPanes(PlanRestart(list, req.Only), ambiguous), req.Ancestors())

	if req.DryRun {
		env, err := req.Env("")
		if err != nil {
			return RestartResult{}, err
		}
		return RestartResult{Preview: PreviewPlan(ctx, env, plan)}, nil
	}

	release, err := lockRestart(req.Paths.StoreDir)
	if err != nil {
		return RestartResult{}, err
	}
	defer release()

	forgectl, err := req.Binary()
	if err != nil {
		return RestartResult{}, err
	}
	env, err := req.Env(forgectl)
	if err != nil {
		return RestartResult{}, err
	}

	if req.HandleSignals {
		var stop context.CancelFunc
		// stop runs only on return, so a second Ctrl-C during an in-flight
		// restart is absorbed rather than killing this process.
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		// With SIGPIPE ignored, a write to a closed pipe returns EPIPE instead
		// of killing the process, and the progress writer drops the error.
		signal.Ignore(syscall.SIGPIPE)
		defer signal.Reset(syscall.SIGPIPE)
	}

	opts := req.Options
	opts.Progress = req.Progress
	opts.Panes = req.Panes
	res := RestartResult{Finals: RunRestart(ctx, env, plan, opts)}
	for _, ev := range res.Finals {
		switch ev.State {
		case StateFailed:
			res.Failed++
		case StateLeft:
			res.Left++
		case StatePaneGone:
			res.PaneGone++
		}
	}
	return res, nil
}

func (r *RestartRequest) fill() {
	if r.Lookup == nil {
		r.Lookup = PaneFor
	}
	if r.Panes == nil {
		r.Panes = SystemRestartEnv{runner: r.Runner, herdr: r.HerdrBin}.ListPanes
	}
	if r.Binary == nil {
		r.Binary = RelaunchBinary
	}
	if r.Env == nil {
		paths, runner, dry, herdr := r.Paths, r.Runner, r.DryRun, r.HerdrBin
		r.Env = func(forgectl string) (RestartEnv, error) {
			if dry {
				// A dry run only reads, and has no binary to relaunch with.
				return SystemRestartEnv{paths: paths, runner: runner, herdr: herdr}, nil
			}
			e, err := NewSystemRestartEnv(paths, runner, forgectl)
			e.herdr = herdr
			return e, err
		}
	}
	if r.Ancestors == nil {
		r.Ancestors = AncestorPids
	}
	if r.Progress == nil {
		r.Progress = func(RestartEvent) {}
	}
}

// reasonAncestor is the skip reason for a session this run is inside.
const reasonAncestor = "this run is inside it (its pid is an ancestor of this process), so stopping it would kill the run before the relaunch; quit it, then run "

// SkipAncestors re-plans any restart whose pid is in ancestors as a skip.
func SkipAncestors(plan []RestartPlanItem, ancestors map[int]bool) []RestartPlanItem {
	out := make([]RestartPlanItem, len(plan))
	for i, item := range plan {
		if item.Action == ActionRestart && ancestors[item.Session.Pid] {
			item.Action = ActionSkip
			item.Reason = reasonAncestor + ManualResume(item.SessionID)
		}
		out[i] = item
	}
	return out
}

// maxAncestorDepth bounds the ppid walk; a real process tree is a handful of
// levels deep, and a cycle in corrupt data must not spin forever.
const maxAncestorDepth = 256

// AncestorPids returns this process's ancestors, parent first, up to (not
// including) pid 1. A read failure ends the walk early; the pids already
// found are kept.
func AncestorPids() map[int]bool {
	return ancestorsOf(os.Getppid(), parentPid)
}

// ancestorsOf walks the parent chain from pid with the given reader.
func ancestorsOf(pid int, parent func(int) (int, error)) map[int]bool {
	out := map[int]bool{}
	for i := 0; i < maxAncestorDepth && pid > 1 && !out[pid]; i++ {
		out[pid] = true
		next, err := parent(pid)
		if err != nil {
			break
		}
		pid = next
	}
	return out
}

// PreviewPlan is --dry-run: each session's planned action, and for the ones a
// run would restart, what the checks say right now. Reads only.
func PreviewPlan(ctx context.Context, env RestartEnv, plan []RestartPlanItem) []PreviewLine {
	lines := make([]PreviewLine, 0, len(plan))
	for _, item := range plan {
		line := PreviewLine{SessionID: item.SessionID, Action: "skip", Detail: item.Reason}
		switch item.Action {
		case ActionManual:
			line.Action = "manual"
		case ActionRestart:
			c := Preview(ctx, env, item.Session)
			switch c.Readiness {
			case Ready:
				line.Action, line.Detail = "restart", "now: "+c.Reason
			case NotYet:
				line.Action, line.Detail = "wait", c.Reason
			default:
				line.Action, line.Detail = "refuse", c.Reason+"; by hand: "+ManualResume(item.SessionID)
			}
			line.Detail += fmt.Sprintf(" (pane %s, pid %d)", paneLabel(item.Session), item.Session.Pid)
		}
		lines = append(lines, line)
	}
	return lines
}

// RelaunchBinary picks the forgectl binary each pane will run.
//
// The running binary (os.Executable, unresolved, so a Homebrew link stays a
// link that survives an upgrade) is the first choice: it is the build that
// chose the sessions. The one exception is a `go run` build, which lives in a
// go-build temp directory that is deleted the moment this process exits —
// typed into a pane, it would name a file that no longer exists. That case
// falls back to the forgectl on PATH, looked up here and passed absolute, so
// the pane's own PATH is never consulted.
func RelaunchBinary() (string, error) {
	exe, err := os.Executable()
	return pickRelaunchBinary(exe, err, osexec.LookPath)
}

// pickRelaunchBinary is RelaunchBinary's decision, with its inputs passed in:
// a test binary itself lives under go-build, so the real os.Executable cannot
// exercise the first branch.
func pickRelaunchBinary(exe string, exeErr error, lookPath func(string) (string, error)) (string, error) {
	if exeErr == nil && filepath.IsAbs(exe) && !strings.Contains(filepath.ToSlash(exe), "/go-build") {
		return exe, nil
	}
	path, err := lookPath("forgectl")
	if err != nil {
		return "", errors.New("find a forgectl binary for the panes to run: this one is a temporary `go run` build and none is on PATH")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve the forgectl on PATH: %w", err)
	}
	return abs, nil
}
