package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The drain's tick: thin I/O around the decisions in internal/surface/drain.
// Each step reads, asks a pure function, and writes what it returns, through
// the drainIO seams so a test can run a tick with no herdr, git or GitHub.

// maxDrainErrorLen bounds error text the drain keeps in a row or an event.
const maxDrainErrorLen = 600

// drainProbeTimeout bounds one screen read; a wedged herdr reads unreadable.
const drainProbeTimeout = 30 * time.Second

// drainQueue is the slice of *worker.Queue the drain uses.
type drainQueue interface {
	Rows() ([]worker.QueueRow, error)
	ClaimFor(name, launchID, session string, now time.Time) (worker.QueueRow, error)
	UpdateIf(name string, match func(worker.QueueRow) bool, now time.Time, fn func(*worker.QueueRow)) (worker.QueueRow, error)
	RemoveIf(name string, match func(worker.QueueRow) bool) (worker.QueueRow, error)
}

// drainNotifier signals the operator about a row's needs-you state. led is
// the row's own ledger row (matched by launch id), whose ref names the
// worker's pane. Each returns what failed; the drain records it as one error
// event and carries on.
type drainNotifier interface {
	// NeedsYou is called once each time a row enters needs-you, when
	// [surface.drain] notify is on.
	NeedsYou(ctx context.Context, row worker.QueueRow, led worker.Row, reason string) error
	// Cleared is called once each time a row leaves needs-you. It is not
	// gated on notify: turning notify off must not strand a pane marked
	// from before.
	Cleared(ctx context.Context, row worker.QueueRow, led worker.Row) error
}

// drainIO is everything a tick reads and writes, apart from the queue.
type drainIO struct {
	queue drainQueue
	// ledgerRows reads the ledger for a repo and herdr session.
	ledgerRows func(repo, session string) ([]worker.Row, error)
	// prober returns a reader for this tick's screen reads: one herdr
	// adapter and one predicate table for every row.
	prober func() drainProber
	// launch runs one in-process worker launch for a claimed row.
	launch func(ctx context.Context, cfg config.Config, row worker.QueueRow) drain.Attempt
	// herdrReady is herdr's readiness check; nil error means ready.
	herdrReady func(ctx context.Context) error
	// load reads the config file with the normal loader.
	load   func() (config.Config, error)
	notify drainNotifier
	// emit records one event.
	emit     func(drain.Event) error
	now      func() time.Time
	launchID func() (string, error)
}

// drainProber reads one live worker's pane.
type drainProber func(ctx context.Context, q worker.QueueRow, led worker.Row) drain.Probe

// drainer is one drain process's state between ticks.
type drainer struct {
	io      drainIO
	session string
	pauses  drain.Pauses
	memo    map[string]drain.Memo
	// settings are the last valid [surface.drain] values. An invalid file
	// pauses claiming; the watch keeps pacing and judging idleness by these.
	settings config.DrainSettings
	cfg      config.Config
	// stopping reports a stop request; the tick ends after its current step.
	stopping func() bool
	// lastErr is the last I/O error a tick met, for drain.json.
	lastErr string
}

func newDrainer(io drainIO, session string, stopping func() bool) *drainer {
	return &drainer{io: io, session: session, pauses: drain.Pauses{}, memo: map[string]drain.Memo{},
		settings: config.DefaultDrainSettings(), stopping: stopping}
}

// event records e, keeping the first write failure for drain.json.
func (d *drainer) event(e drain.Event) {
	e.Error = termsafe.SafeLineMax(e.Error, maxDrainErrorLen)
	if err := d.io.emit(e); err != nil {
		d.lastErr = "events: " + err.Error()
	}
}

func (d *drainer) pause(kind drain.PauseKind, reason string) {
	if d.pauses.Set(kind, reason) {
		d.event(drain.Event{Kind: drain.EventPause, State: string(kind), Error: reason})
	}
}

func (d *drainer) resume(kind drain.PauseKind) {
	if d.pauses.Clear(kind) {
		d.event(drain.Event{Kind: drain.EventResume, State: string(kind)})
	}
}

// loadSettings reloads the config file. A file that does not load, or an
// invalid [surface.drain] value, pauses claiming with the reason; nothing
// falls back to a default.
func (d *drainer) loadSettings() {
	cfg, err := d.io.load()
	var s config.DrainSettings
	if err == nil {
		s, err = cfg.Surface.Drain.Resolve()
	}
	if err != nil {
		d.pause(drain.PauseConfig, termsafe.SafeLineMax(err.Error(), maxDrainErrorLen))
		return
	}
	d.settings, d.cfg = s, cfg
	d.resume(drain.PauseConfig)
}

// ledgers reads each row's ledger row, reading each (repo, session) ledger
// once. Queued rows are not read: nothing of theirs is live.
func (d *drainer) ledgers(rows []worker.QueueRow) map[string]drain.Ledger {
	return readLedgers(rows, d.session, d.io.ledgerRows)
}

// readLedgers reads each row's ledger row, reading each (repo, session)
// ledger once. A row with no recorded session reads defaultSession's.
// Queued and expired rows are not read: nothing of theirs is live.
func readLedgers(rows []worker.QueueRow, defaultSession string, ledgerRows func(repo, session string) ([]worker.Row, error)) map[string]drain.Ledger {
	type key struct{ repo, session string }
	type read struct {
		rows []worker.Row
		err  error
	}
	cache := map[key]read{}
	out := make(map[string]drain.Ledger, len(rows))
	for _, q := range rows {
		if q.State == worker.QueueQueued || q.State == worker.QueueExpired {
			continue
		}
		session := q.Session
		if session == "" {
			session = defaultSession
		}
		k := key{q.Repo, session}
		r, ok := cache[k]
		if !ok {
			r.rows, r.err = ledgerRows(k.repo, k.session)
			cache[k] = r
		}
		out[q.Name] = drain.MatchLedger(q, r.rows, r.err)
	}
	return out
}

// apply writes c to the row as read, only if the row is still exactly as
// read, and records the change. It reports whether the row was written.
func (d *drainer) apply(q worker.QueueRow, c drain.Change) (worker.QueueRow, bool) {
	if c.Note != "" {
		d.event(drain.Event{Kind: drain.EventUnreadable, Name: q.Name, Repo: q.Repo, State: string(q.State), Attempt: q.Attempts, Error: c.Note})
	}
	if !c.Writes() {
		return q, false
	}
	written, err := d.io.queue.UpdateIf(q.Name, worker.SameRead(q), d.io.now(), c.Apply)
	switch {
	case errors.Is(err, worker.ErrQueueRowChanged), errors.Is(err, worker.ErrQueueNoRow):
		// The operator dequeued it, or it moved on since the read: leave it.
		return q, false
	case err != nil:
		d.lastErr = "queue: " + err.Error()
		d.event(drain.Event{Kind: drain.EventError, Name: q.Name, Repo: q.Repo, State: string(q.State),
			Error: fmt.Sprintf("write %s -> %s: %v", q.State, nonEmptyState(c.To, q.State), err)})
		return q, false
	}
	if c.To != "" {
		d.event(drain.Event{Kind: drain.EventState, Name: q.Name, Repo: q.Repo, State: string(written.State), Attempt: written.Attempts, Error: written.LastError})
	}
	return written, true
}

func nonEmptyState(s, fallback worker.QueueState) worker.QueueState {
	if s == "" {
		return fallback
	}
	return s
}

// tick runs one pass: reload the config, settle claimed rows a dead launch
// left, watch live workers, expire, claim and launch, prune. It returns the
// rows as last read, for drain.json.
func (d *drainer) tick(ctx context.Context) []worker.QueueRow {
	d.lastErr = ""
	d.loadSettings()
	rows, ok := d.readRows()
	if !ok {
		return nil
	}
	ledgers := d.ledgers(rows)

	// Claimed rows at the start of a tick belong to no running launch: this
	// process launches every row it claims within the tick that claimed it.
	for _, q := range rows {
		if q.State == worker.QueueClaimed {
			c, m := drain.Reconcile(q, ledgers[q.Name], d.memo[q.Name])
			if _, ok := d.apply(q, c); ok || !c.Writes() {
				d.memo[q.Name] = m
			}
		}
	}
	if d.stopping() {
		return rows
	}
	d.watch(ctx, rows, ledgers)
	if d.stopping() {
		return rows
	}
	now := d.io.now()
	for _, q := range rows {
		d.apply(q, drain.Expire(q, now))
	}
	if d.stopping() {
		return rows
	}
	if rows, ok = d.readRows(); !ok {
		return nil
	}
	d.claimAndLaunch(ctx, rows, d.ledgers(rows))
	if d.stopping() {
		return rows
	}
	d.prune(rows)
	if fresh, ok := d.readRows(); ok {
		rows = fresh
	}
	return rows
}

func (d *drainer) readRows() ([]worker.QueueRow, bool) {
	rows, err := d.io.queue.Rows()
	if err != nil {
		d.lastErr = "queue: " + err.Error()
		d.event(drain.Event{Kind: drain.EventError, Error: "read the queue: " + err.Error()})
		return nil, false
	}
	return rows, true
}

// watch reads each live worker once and applies drain.Watch.
func (d *drainer) watch(ctx context.Context, rows []worker.QueueRow, ledgers map[string]drain.Ledger) {
	var probe drainProber
	live := map[string]bool{}
	for _, q := range rows {
		if q.State == worker.QueueClaimed {
			live[q.Name] = true // its Reconcile memo outlives the tick
		}
	}
	for _, q := range rows {
		if q.State != worker.QueueLaunched && q.State != worker.QueueNeedsYou {
			continue
		}
		live[q.Name] = true
		if d.stopping() {
			return
		}
		l := ledgers[q.Name]
		p := drain.Probe{}
		if drain.NeedsProbe(q, l) {
			if probe == nil {
				probe = d.io.prober()
			}
			p = probe(ctx, q, l.Row)
		}
		c, m := drain.Watch(q, l, p, d.memo[q.Name], d.io.now(), d.settings.Idle)
		written, ok := d.apply(q, c)
		if c.Writes() && !ok {
			continue // not written: keep the old memo so the change is retried
		}
		d.memo[q.Name] = m
		if !ok {
			continue
		}
		// The notifier gets the row's own ledger row only: another launch's
		// ref names another worker's pane.
		own := l.Row
		if l.State == drain.LedgerOther {
			own = worker.Row{}
		}
		// Each call below happens once per state entry or exit, so a failure
		// is one event, never one per tick.
		if c.Notify && d.settings.Notify {
			if err := d.io.notify.NeedsYou(ctx, written, own, written.LastError); err != nil {
				d.event(drain.Event{Kind: drain.EventError, Name: written.Name, Repo: written.Repo, State: string(written.State),
					Error: "needs-you notification: " + err.Error()})
			}
		}
		if q.State == worker.QueueNeedsYou && written.State != worker.QueueNeedsYou {
			if err := d.io.notify.Cleared(ctx, written, own); err != nil {
				d.event(drain.Event{Kind: drain.EventError, Name: written.Name, Repo: written.Repo, State: string(written.State),
					Error: "clear the needs-you pane state: " + err.Error()})
			}
		}
	}
	for _, q := range rows {
		if q.State == worker.QueueReported || q.State == worker.QueueFailed {
			d.apply(q, drain.Settle(q, ledgers[q.Name]))
		}
	}
	for name := range d.memo {
		if !live[name] {
			delete(d.memo, name)
		}
	}
}

// claimAndLaunch claims and launches rows one at a time, oldest first, while
// the slots allow and nothing pauses claiming.
func (d *drainer) claimAndLaunch(ctx context.Context, rows []worker.QueueRow, ledgers map[string]drain.Ledger) {
	plan := drain.PlanClaims(rows, ledgers, d.settings)
	if len(plan) == 0 && !d.pauseHeld(drain.PauseHerdr) {
		return
	}
	if err := d.io.herdrReady(ctx); err != nil {
		d.pause(drain.PauseHerdr, termsafe.SafeLineMax(err.Error(), maxDrainErrorLen))
	} else {
		d.resume(drain.PauseHerdr)
	}
	for _, q := range plan {
		if d.pauses.Paused() || d.stopping() {
			return
		}
		id, err := d.io.launchID()
		if err != nil {
			d.lastErr = "launch id: " + err.Error()
			return
		}
		claimed, err := d.io.queue.ClaimFor(q.Name, id, d.session, d.io.now())
		if err != nil {
			// Dequeued or claimed elsewhere since the read: skip it.
			if !errors.Is(err, worker.ErrQueueNotQueued) && !errors.Is(err, worker.ErrQueueNoRow) {
				d.lastErr = "queue: " + err.Error()
			}
			continue
		}
		d.event(drain.Event{Kind: drain.EventState, Name: claimed.Name, Repo: claimed.Repo, State: string(claimed.State), Attempt: claimed.Attempts})
		var a drain.Attempt
		if err := drain.CheckClaimed(claimed); err != nil {
			a = drain.Attempt{Class: drain.ErrRowInvalid, Err: err.Error(), CreatedNothing: true}
		} else {
			a = d.io.launch(ctx, d.cfg, claimed)
		}
		a.Err = termsafe.SafeLineMax(a.Err, maxDrainErrorLen)
		dec := drain.DecideLaunch(claimed, a)
		if dec.Pause != "" {
			d.pause(dec.Pause, dec.PauseReason)
		}
		d.apply(claimed, dec.Change)
	}
}

func (d *drainer) pauseHeld(k drain.PauseKind) bool {
	_, ok := d.pauses[k]
	return ok
}

// prune removes terminal rows past drain.PruneAfter, each only if it is
// still the row read.
func (d *drainer) prune(rows []worker.QueueRow) {
	now := d.io.now()
	for _, q := range rows {
		if !drain.Prunable(q, now) {
			continue
		}
		_, err := d.io.queue.RemoveIf(q.Name, worker.SameRead(q))
		switch {
		case err == nil:
			d.event(drain.Event{Kind: drain.EventState, Name: q.Name, Repo: q.Repo, State: "pruned", Attempt: q.Attempts})
		case errors.Is(err, worker.ErrQueueRowChanged), errors.Is(err, worker.ErrQueueNoRow):
		default:
			d.lastErr = "queue: " + err.Error()
		}
	}
}

// status renders drain.json for rows.
func (d *drainer) status(base drain.Status, rows []worker.QueueRow) drain.Status {
	s := base
	s.Status = drain.StatusRunning
	s.PauseReason = d.pauses.Reason()
	if d.pauses.Paused() {
		s.Status = drain.StatusPaused
	}
	s.IntervalSeconds = int64(d.settings.Interval / time.Second)
	s.Counts, s.Attention = drain.Summarize(rows)
	s.Error = termsafe.SafeLineMax(d.lastErr, maxDrainErrorLen)
	return s
}

// newDrainLaunchID is a fresh claim id.
func newDrainLaunchID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("forgectl: drain launch id: %w", err)
	}
	return "launch-" + hex.EncodeToString(b), nil
}

// drainLaunch is the real launch: the in-process worker launch (T8.1) with
// harness claude, the row's brief text, and the branch worker/<name>. The
// repo must still resolve to the top the row records.
func drainLaunch(runner exec.Runner) func(context.Context, config.Config, worker.QueueRow) drain.Attempt {
	return drainLaunchWith(runner, launchWorker)
}

// workerLauncher is launchWorker's shape, so a test can drive the drain's
// launch with the real spec and stubbed git and herdr.
type workerLauncher func(ctx context.Context, warn io.Writer, deps module.Deps, spec workerSpec, briefText string) (workerAttempt, error)

func drainLaunchWith(runner exec.Runner, launchFn workerLauncher) func(context.Context, config.Config, worker.QueueRow) drain.Attempt {
	return func(ctx context.Context, cfg config.Config, row worker.QueueRow) drain.Attempt {
		top, err := worker.RepoTop(ctx, runner, row.Repo)
		if err != nil {
			return drain.Attempt{Class: drain.ErrRowInvalid, Err: err.Error(), CreatedNothing: true}
		}
		if top != row.Repo {
			return drain.Attempt{Class: drain.ErrRowInvalid, CreatedNothing: true,
				Err: fmt.Sprintf("the repository top is now %s, expected the queued %s", top, row.Repo)}
		}
		attempt, err := launchFn(ctx, io.Discard, module.Deps{Runner: runner, Cfg: cfg}, drainSpec(row), row.Brief)
		return attemptOf(attempt, err, row)
	}
}

// drainSpec is the worker launch for a claimed row: harness claude, branch
// worker/<name>, no $PATH binary, and the claim's launch id for the ledger.
func drainSpec(row worker.QueueRow) workerSpec {
	return workerSpec{target: row.Repo, name: row.Name, branch: drain.Branch(row.Name), harness: "claude", allowPATH: false, launchID: row.LaunchID}
}

// attemptOf turns a workerAttempt and its error into the drain's view.
func attemptOf(attempt workerAttempt, err error, row worker.QueueRow) drain.Attempt {
	a := drain.Attempt{Row: attempt.row, Worktree: attempt.launched.worktree}
	if attempt.row != nil && attempt.row.Worktree != "" {
		a.Worktree = attempt.row.Worktree
	}
	if a.Worktree == "" {
		a.Worktree = worker.WorktreePath(row.Repo, row.Name)
	}
	if err == nil {
		return a
	}
	a.Class = classifyLaunchError(err)
	a.Err = err.Error()
	a.CreatedNothing = attempt.createdNothing()
	return a
}

// classifyLaunchError sorts a launch error into the drain's classes.
func classifyLaunchError(err error) drain.ErrClass {
	switch {
	case err == nil:
		return drain.ErrNone
	case errors.Is(err, worker.ErrNameTaken):
		return drain.ErrNameTaken
	case launchConfigFailure(err):
		return drain.ErrLaunchConfig
	case herdrUnavailable(err):
		return drain.ErrHerdrDown
	case githubAuthFailure(err):
		return drain.ErrGitHubAuth
	}
	return drain.ErrOther
}

// launchConfigFailure reports a launch whose configuration cannot build a
// worker: the build step, the worker posture or harness override, or the
// binary policy (a claude found only on $PATH, unusable, or forgectl).
func launchConfigFailure(err error) bool {
	for _, target := range []error{errLaunchConfig, launch.ErrWorkerPosture, launch.ErrHarnessOverride,
		surface.ErrBinaryProvenance, surface.ErrBinaryUnusable, surface.ErrBinarySelfLoop} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// herdrUnavailable reports herdr missing from PATH, or a start cause of
// class backend-unavailable anywhere in the chain.
func herdrUnavailable(err error) bool {
	if errors.Is(err, errBackendUnavailable) {
		return true
	}
	var cause backend.StartCause
	if errors.As(err, &cause) && cause.Class() == backend.FailureUnavailable {
		return true
	}
	var pcause *backend.StartCause
	if errors.As(err, &pcause) && pcause != nil && pcause.Class() == backend.FailureUnavailable {
		return true
	}
	var lerr *backend.LaunchError
	return errors.As(err, &lerr) && lerr.Cause.Class() == backend.FailureUnavailable
}

// githubAuthMarkers are what gh prints when GitHub refuses its credentials.
//
// debt: matched on gh's stderr text, which gh can reword; upgrade to a typed
// error when internal/githubauth classifies gh failures.
var githubAuthMarkers = []string{"HTTP 401", "Bad credentials", "gh auth login", "Requires authentication", "not logged in"}

// githubAuthFailure reports a gh command that failed on authentication.
func githubAuthFailure(err error) bool {
	var ce *exec.CommandError
	if !errors.As(err, &ce) || ce.Name != "gh" {
		return false
	}
	// Matched against the raw stderr, never rendered: Error() redacts it.
	for _, m := range githubAuthMarkers {
		if strings.Contains(ce.Stderr, m) {
			return true
		}
	}
	return false
}

// drainProbe builds the real reader for one tick: one herdr adapter pinned to
// session and one predicate table. A setup failure reads every row as
// unreadable.
func drainProbe(session string) drainProber {
	unreadable := func(why string) drainProber {
		return func(context.Context, worker.QueueRow, worker.Row) drain.Probe {
			return drain.Probe{State: drain.ProbeUnreadable, Err: why}
		}
	}
	adapter, err := newHerdrAdapter(io.Discard)
	if err != nil {
		return unreadable(err.Error())
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return unreadable("the herdr adapter has an unexpected type")
	}
	path, err := config.SurfaceReadyPredicatesPath()
	if err != nil {
		return unreadable(err.Error())
	}
	table, err := ready.Load(path)
	if err != nil {
		return unreadable("readiness predicates: " + err.Error())
	}
	return func(ctx context.Context, q worker.QueueRow, led worker.Row) drain.Probe {
		return probeWorker(ctx, herdr, table, session, q, led)
	}
}

// screenReader is the one adapter call a probe makes.
type screenReader interface {
	Session() string
	WorkerScreen(ctx context.Context, ref backend.Ref) (ready.Screen, error)
}

// probeWorker reads one worker's pane once, with no waiting. A worker in
// another herdr session than the drain's is unreadable, never gone: this
// adapter cannot see it.
func probeWorker(ctx context.Context, herdr screenReader, table *ready.Table, session string, q worker.QueueRow, led worker.Row) drain.Probe {
	if want := q.Session; want != "" && want != herdr.Session() {
		return drain.Probe{State: drain.ProbeUnreadable, Err: fmt.Sprintf("the worker is in herdr session %q, the drain reads session %q", want, herdr.Session())}
	}
	if herdr.Session() != session {
		return drain.Probe{State: drain.ProbeUnreadable, Err: fmt.Sprintf("the herdr adapter resolved session %q, expected the pinned %q", herdr.Session(), session)}
	}
	if !table.Has(led.Harness) {
		return drain.Probe{State: drain.ProbeUnreadable, Err: fmt.Sprintf("no readiness predicates for harness %q", led.Harness)}
	}
	ref, err := backend.DecodeRef(led.Ref)
	if err != nil {
		return drain.Probe{State: drain.ProbeUnreadable, Err: "the ledger reference does not decode: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, drainProbeTimeout)
	defer cancel()
	s, err := herdr.WorkerScreen(ctx, ref)
	switch {
	case errors.Is(err, herdradapter.ErrWorkerGone):
		return drain.Probe{State: drain.ProbeGone}
	case err != nil:
		return drain.Probe{State: drain.ProbeUnreadable, Err: termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
	}
	p := drain.Probe{State: drain.ProbeRead, Verdict: table.Evaluate(led.Harness, s)}
	if led.Brief != nil {
		_, p.Report = worker.FindReport(s.Text, led.Brief.Marker)
	}
	return p
}

// drainHerdrTimeout bounds the pre-claim herdr readiness check.
const drainHerdrTimeout = 15 * time.Second

// realDrainIO wires the tick to the queue, the ledgers, herdr, the config
// file and the launch path. emit records an event.
func realDrainIO(deps module.Deps, session string, emit func(drain.Event) error) (drainIO, error) {
	q, err := worker.OpenQueue()
	if err != nil {
		return drainIO{}, err
	}
	return drainIO{
		queue: q,
		ledgerRows: func(repo, s string) ([]worker.Row, error) {
			led, err := worker.Open(repo, s)
			if err != nil {
				return nil, err
			}
			return led.Rows()
		},
		prober: func() drainProber { return drainProbe(session) },
		launch: drainLaunch(deps.Runner),
		herdrReady: func(ctx context.Context) error {
			adapter, err := newHerdrAdapter(io.Discard)
			if err != nil {
				return err
			}
			herdr, ok := adapter.(*herdradapter.Adapter)
			if !ok {
				return errors.New("forgectl: the herdr adapter has an unexpected type")
			}
			ctx, cancel := context.WithTimeout(ctx, drainHerdrTimeout)
			defer cancel()
			return herdr.CheckReady(ctx)
		},
		load:     drainLoadConfig,
		notify:   newNeedsYouNotifier(deps.Runner, session),
		emit:     emit,
		now:      time.Now,
		launchID: newDrainLaunchID,
	}, nil
}
