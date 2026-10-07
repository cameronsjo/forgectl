package drain

import (
	"fmt"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// ErrClass is the kind of error a launch attempt ended with.
type ErrClass int

const (
	// ErrNone: the launch succeeded.
	ErrNone ErrClass = iota
	// ErrRowInvalid: the claimed row failed its re-check; nothing ran.
	ErrRowInvalid
	// ErrNameTaken: the ledger already holds a row with this name. The
	// attempt wrote nothing, but a retry would hit the same row.
	ErrNameTaken
	// ErrGitHubAuth: GitHub refused the credentials for the base lookup.
	ErrGitHubAuth
	// ErrHerdrDown: herdr is not running or cannot be reached.
	ErrHerdrDown
	// ErrOther: anything else.
	ErrOther
)

// Attempt is what one launch left: the error class and text, whether it is
// known to have created nothing (worker ledger row absent, or failed naming
// no worktree, workspace or recovery tag), and the ledger row it wrote.
type Attempt struct {
	Class          ErrClass
	Err            string
	CreatedNothing bool
	// Row is the ledger row the attempt left, or nil.
	Row *worker.Row
	// Worktree is the worktree path the attempt made or would make.
	Worktree string
}

// PauseKind names why claiming is paused. Several can hold at once.
type PauseKind string

const (
	// PauseConfig: [surface.drain] or the config file is invalid. Cleared
	// when a tick reads a valid file.
	PauseConfig PauseKind = "config"
	// PauseHerdr: herdr is unreachable. Retried each tick.
	PauseHerdr PauseKind = "herdr"
	// PauseGitHubAuth: GitHub refused the credentials. Cleared only by the
	// next drain start.
	PauseGitHubAuth PauseKind = "github-auth"
)

// LaunchDecision is the row change a launch attempt leads to, and a pause to
// set, if any.
type LaunchDecision struct {
	Change
	Pause       PauseKind
	PauseReason string
}

// DecideLaunch decides what a claimed row becomes after its launch attempt:
//
//   - success: launched.
//   - the row failed its re-check: failed, no attempt counted.
//   - a ledger row already holds the name: failed. The attempt created
//     nothing, but a retry would only hit that row again.
//   - GitHub auth or herdr unreachable, having created nothing: back to
//     queued, no attempt counted, and claiming pauses with that reason.
//   - any other failure that created nothing: back to queued with one more
//     attempt, or failed at MaxAttempts.
//   - a failure after something was created: failed at once, naming the
//     error, the worktree and the ledger stage. (An auth or herdr failure
//     here also pauses.)
func DecideLaunch(q worker.QueueRow, a Attempt) LaunchDecision {
	c := change(q)
	d := LaunchDecision{}
	switch a.Class {
	case ErrGitHubAuth:
		d.Pause, d.PauseReason = PauseGitHubAuth, "GitHub refused the credentials: "+a.Err
	case ErrHerdrDown:
		d.Pause, d.PauseReason = PauseHerdr, "herdr is unreachable: "+a.Err
	}
	switch {
	case a.Class == ErrNone:
		d.Change = c.to(worker.QueueLaunched, "")
	case a.Class == ErrRowInvalid:
		d.Change = c.to(worker.QueueFailed, "refused before launch: "+a.Err)
	case a.Class == ErrNameTaken:
		d.Change = c.to(worker.QueueFailed, fmt.Sprintf("%s; a ledger row named %q already exists, expected none: run surface close, then dequeue and enqueue to retry", a.Err, q.Name))
	case d.Pause != "" && a.CreatedNothing:
		d.Change = c.to(worker.QueueQueued, fmt.Sprintf("not launched, claiming paused (%s): %s", d.Pause, a.Err))
		d.ClearLaunch = true
	case a.CreatedNothing:
		n := q.Attempts + 1
		if n >= MaxAttempts {
			d.Change = c.to(worker.QueueFailed, fmt.Sprintf("attempt %d of %d failed having created nothing: %s", n, MaxAttempts, a.Err))
		} else {
			d.Change = c.to(worker.QueueQueued, fmt.Sprintf("attempt %d of %d failed having created nothing, will retry: %s", n, MaxAttempts, a.Err))
			d.ClearLaunch = true
		}
		d.Attempts, d.SetAttempts = n, true
	default:
		d.Change = c.to(worker.QueueFailed, fmt.Sprintf("launch failed after creating something: %s; worktree %s; ledger stage %s; run surface close, then dequeue and enqueue to retry",
			a.Err, nonEmpty(a.Worktree, "unknown"), ledgerStage(a.Row)))
	}
	return d
}

// Reconcile decides what a claimed row becomes when the drain finds it
// claimed at the start of a tick, which means the launch that claimed it is
// not running: a drain died mid-launch. It reads the ledger row with the
// same name and launch id:
//
//	none, or another launch id  queued (no attempt counted)
//	created nothing             queued (no attempt counted)
//	pending or worktree         failed, naming the worktree
//	launched                    launched
//	failed                      failed
//	closed                      closed
//
// An unreadable ledger leaves the row claimed (it keeps its slot) and is
// tried again next tick.
func Reconcile(q worker.QueueRow, l Ledger) Change {
	c := change(q)
	if q.State != worker.QueueClaimed {
		return c
	}
	requeue := func(why string) Change {
		c = c.to(worker.QueueQueued, why)
		c.ClearLaunch = true
		return c
	}
	switch l.State {
	case LedgerUnreadable:
		c.Note = "claimed row left as is; the ledger could not be read: " + l.Err
		return c
	case LedgerAbsent:
		return requeue("the drain stopped before this launch wrote its ledger row; requeued")
	case LedgerOther:
		return requeue(fmt.Sprintf("the ledger row named %q is another launch's (expected launch_id %s, saw %q); requeued", q.Name, q.LaunchID, l.Row.LaunchID))
	}
	if worker.CreatedNothing(l.Row) {
		return requeue("the interrupted launch created nothing; requeued")
	}
	switch l.Row.Stage {
	case worker.StagePending, worker.StageWorktree:
		wt := nonEmpty(l.Row.Worktree, worker.WorktreePath(q.Repo, q.Name))
		return c.to(worker.QueueFailed, fmt.Sprintf("the drain stopped mid-launch at ledger stage %s; worktree %s may exist: run surface close, then dequeue and enqueue to retry", l.Row.Stage, wt))
	case worker.StageLaunched:
		return c.to(worker.QueueLaunched, "")
	case worker.StageFailed:
		return c.to(worker.QueueFailed, "the launch failed: "+l.Row.Failure)
	case worker.StageClosed:
		return c.to(worker.QueueClosed, "")
	}
	return c.to(worker.QueueFailed, fmt.Sprintf("the ledger row is at unknown stage %q", l.Row.Stage))
}

func ledgerStage(r *worker.Row) string {
	if r == nil {
		return "unknown (no row read back)"
	}
	return string(r.Stage)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
