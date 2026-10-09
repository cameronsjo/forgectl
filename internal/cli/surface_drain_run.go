package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
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
	// slots runs `claude-slots check <need>` before a launch; nil when
	// claude-slots was not on PATH at drain start, and the drain launches
	// without the cap.
	slots func(ctx context.Context, need int) drain.SlotsCheck
	// emit records one event.
	emit     func(drain.Event) error
	now      func() time.Time
	launchID func() (string, error)
	// prRead finds a reported worker's PR through the discovery filter
	// (merge.Reader.Discover), for the closers.
	prRead func(ctx context.Context, row merge.Row) (merge.Candidate, error)
	// price prices a transcript, or returns nil (priceTranscript).
	price func(ctx context.Context, transcript string) *statusUsage
	// closeRow runs the close `surface close` runs on a reported row's own
	// ledger row.
	closeRow func(ctx context.Context, q worker.QueueRow, led worker.Row) closeResult
	// prune runs `surface prune` with the default cutoff.
	prune func(ctx context.Context, now time.Time) (pruneResult, error)
	// pruneDay and setPruneDay read and record the UTC day of the last daily
	// prune.
	pruneDay    func() (string, error)
	setPruneDay func(day string) error
	// mergeSettings resolves [surface.merge] from the config file, fresh;
	// the autopilot runs only while it says auto.
	mergeSettings func() config.MergeSettings
	// land is the merge path `surface merge` uses (merge.Lander.Land).
	land landFunc
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
	// slotsCond is the condition the last claude-slots check left
	// (drain.SlotsDecision.Cond), so each is recorded once on entry.
	slotsCond string
	// closerRead is when each reported row's PR was last read, so a row is
	// read at most every drain.CloserReadEvery. A restart forgets it.
	closerRead map[string]time.Time
	// closerNote is the last closer event recorded for each row, so a
	// repeated refusal or note is one event, not one per read. closerReadFail
	// is the same for a failed PR read, and a read that works clears it.
	closerNote     map[string]string
	closerReadFail map[string]string
	// pruneDay is the UTC day of the last daily prune; pruneDayRead says it
	// was read from drain-prune-day.
	pruneDay     string
	pruneDayRead bool
	// autopilotTried is when each reported row's merge was last tried, so a
	// row is tried at most every drain.AutopilotEvery; autopilotNote is the
	// last refusal recorded for each row (head and reasons), so a repeated
	// refusal is one event. A restart forgets both.
	autopilotTried map[string]time.Time
	autopilotNote  map[string]string
}

func newDrainer(io drainIO, session string, stopping func() bool) *drainer {
	return &drainer{io: io, session: session, pauses: drain.Pauses{}, memo: map[string]drain.Memo{},
		settings: config.DefaultDrainSettings(), stopping: stopping,
		closerRead: map[string]time.Time{}, closerNote: map[string]string{}, closerReadFail: map[string]string{},
		autopilotTried: map[string]time.Time{}, autopilotNote: map[string]string{}}
}

// announce records what the drain starts without: one note when claude-slots
// was not found, since every launch then goes ahead without the session cap.
func (d *drainer) announce() {
	if d.io.slots == nil {
		d.event(drain.Event{Kind: drain.EventNote, Error: drain.NoSlotsNote})
	}
}

// slotsHold runs the claude-slots check for the claimed row q, asking for need
// slots, and reports whether to hold it, with the reason. An event is
// recorded once per entry into a held or failed condition.
func (d *drainer) slotsHold(ctx context.Context, q worker.QueueRow, need int) (bool, string) {
	if d.io.slots == nil {
		return false, ""
	}
	dec := drain.DecideSlots(d.io.slots(ctx, need), d.slotsCond)
	d.slotsCond = dec.Cond
	if dec.Emit {
		e := dec.Event
		e.Name, e.Repo = q.Name, q.Repo
		d.event(e)
	}
	return dec.Hold, dec.Reason
}

// unclaim puts a held row back to queued, quietly: the slots-held event
// already said why, once, and a state event per tick would repeat it.
func (d *drainer) unclaim(q worker.QueueRow, why string) {
	_, err := d.io.queue.UpdateIf(q.Name, worker.SameRead(q), d.io.now(), drain.Unclaim(q, why).Apply)
	if err != nil && !errors.Is(err, worker.ErrQueueRowChanged) && !errors.Is(err, worker.ErrQueueNoRow) {
		// The row stays claimed; the next tick's reconcile requeues it.
		d.lastErr = "queue: " + err.Error()
		d.event(drain.Event{Kind: drain.EventError, Name: q.Name, Repo: q.Repo, State: string(q.State),
			Error: "put the held row back to queued: " + err.Error()})
	}
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
// left, watch live workers, run the closers on reported rows, with mode auto
// try one merge, expire, claim and launch, and once a UTC day prune. It returns the rows as last read,
// for drain.json.
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
	d.closers(ctx, rows, ledgers)
	if d.stopping() {
		return rows
	}
	d.autopilot(ctx)
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
	d.dailyPrune(ctx)
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
	if len(plan) == 0 {
		// Nothing to launch ends a hold: the next one is a new entry and
		// records its own slots-held event.
		d.slotsCond = ""
		if !d.pauseHeld(drain.PauseHerdr) {
			return
		}
	}
	// started counts the launches this tick made that may have started a
	// session, so the next check asks for one more slot than that: a session
	// launched moments ago may not be registered with claude-slots yet.
	started := 0
	if err := d.io.herdrReady(ctx); err != nil {
		d.pause(drain.PauseHerdr, termsafe.SafeLineMax(err.Error(), maxDrainErrorLen))
	} else {
		d.resume(drain.PauseHerdr)
	}
	// claudeHeld is set once claude-slots holds a claude row this tick: later
	// claude rows are skipped unclaimed, and codex and pi rows, which are not
	// claude sessions, still launch.
	claudeHeld := false
	// probe lets one launch through this tick when a GitHub-read pause from
	// an earlier tick is the only one held: that launch's identity read is
	// the re-check, and a success clears the pause. A pause set during this
	// tick stops claiming until the next.
	probe := d.onlyPause(drain.PauseGitHub)
	for _, q := range plan {
		if d.stopping() {
			return
		}
		probing := false
		if d.pauses.Paused() {
			if !probe || !d.onlyPause(drain.PauseGitHub) {
				return
			}
			probing = true
		}
		if claudeHeld && q.Launch().Harness == "claude" {
			continue
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
		checkErr := drain.CheckClaimed(claimed)
		// claude-slots caps claude sessions; a codex or pi worker is not one.
		if checkErr == nil && claimed.Launch().Harness == "claude" {
			// The machine's session cap, checked as late as possible: a held
			// row goes back to queued, and no more claude rows are claimed
			// this tick.
			if hold, why := d.slotsHold(ctx, claimed, started+1); hold {
				d.unclaim(claimed, why)
				if ctx.Err() != nil {
					return // a stop cut the check short: launch nothing more
				}
				claudeHeld = true
				continue
			}
		}
		d.event(drain.Event{Kind: drain.EventState, Name: claimed.Name, Repo: claimed.Repo, State: string(claimed.State), Attempt: claimed.Attempts})
		var a drain.Attempt
		if checkErr != nil {
			a = drain.Attempt{Class: drain.ErrRowInvalid, Err: checkErr.Error(), CreatedNothing: true}
		} else {
			a = d.io.launch(ctx, d.cfg, claimed)
		}
		if !a.CreatedNothing {
			started++
		}
		if probing {
			probe = false
		}
		a.Err = termsafe.SafeLineMax(a.Err, maxDrainErrorLen)
		dec := drain.DecideLaunch(claimed, a)
		if dec.Pause != "" {
			d.pause(dec.Pause, dec.PauseReason)
		}
		if a.Class == drain.ErrNone {
			d.resume(drain.PauseGitHub)
		}
		d.apply(claimed, dec.Change)
	}
}

func (d *drainer) pauseHeld(k drain.PauseKind) bool {
	_, ok := d.pauses[k]
	return ok
}

// onlyPause reports that k is the one pause held.
func (d *drainer) onlyPause(k drain.PauseKind) bool {
	return len(d.pauses) == 1 && d.pauseHeld(k)
}

// closers reads the PR of each reported row due a read, oldest read first,
// at most drain.CloserReadsPerTick a tick, and closes the worker once its PR
// merged, or drain.ClosedGrace after it closed unmerged.
func (d *drainer) closers(ctx context.Context, rows []worker.QueueRow, ledgers map[string]drain.Ledger) {
	now := d.io.now()
	type candidate struct {
		q   worker.QueueRow
		led worker.Row
	}
	var due []candidate
	seen := map[string]bool{}
	for _, q := range rows {
		l := ledgers[q.Name]
		ok, identity := drain.CloserCandidate(q, l)
		if !ok {
			continue
		}
		seen[q.Name] = true
		if !identity {
			d.closerEvent(d.closerNote, q, drain.EventNote, fmt.Sprintf("the worker's ledger row records no GitHub repository, so the closers skip it; close it with surface close %s", q.Name))
			continue
		}
		if drain.CloserDue(d.closerRead[q.Name], now) {
			due = append(due, candidate{q, l.Row})
		}
	}
	slices.SortStableFunc(due, func(a, b candidate) int {
		if c := d.closerRead[a.q.Name].Compare(d.closerRead[b.q.Name]); c != 0 {
			return c
		}
		return strings.Compare(a.q.Name, b.q.Name)
	})
	for i, c := range due {
		if i >= drain.CloserReadsPerTick || d.stopping() {
			break
		}
		d.closeOne(ctx, c.q, c.led, now)
	}
	for name := range d.closerRead {
		if !seen[name] {
			delete(d.closerRead, name)
		}
	}
	for _, notes := range []map[string]string{d.closerNote, d.closerReadFail} {
		for name := range notes {
			if !seen[name] {
				delete(notes, name)
			}
		}
	}
}

// closerEvent records one closer event for q, unless the last one notes
// holds for it says the same.
func (d *drainer) closerEvent(notes map[string]string, q worker.QueueRow, kind, text string) {
	key := kind + "\x00" + text
	if notes[q.Name] == key {
		return
	}
	notes[q.Name] = key
	d.event(drain.Event{Kind: kind, Name: q.Name, Repo: q.Repo, State: string(q.State), Attempt: q.Attempts, Error: text})
}

// closeOne reads one reported row's PR and acts on drain.DecideCloser. A
// read that fails changes nothing: a network failure, server error or rate
// limit is an unreadable event, anything else (a refusal, an ambiguous PR, a
// repository whose id changed) an error event, each once until it changes.
func (d *drainer) closeOne(ctx context.Context, q worker.QueueRow, led worker.Row, now time.Time) {
	d.closerRead[q.Name] = now
	cand, err := d.io.prRead(ctx, mergeRow(led, &q))
	pr := drain.PRRead{State: drain.PRNone}
	switch {
	case errors.Is(err, merge.ErrNoPR):
	case err != nil:
		if transientGitHubFailure(err) {
			d.closerEvent(d.closerReadFail, q, drain.EventUnreadable, "the closer could not read the worker's PR from GitHub: "+err.Error())
		} else {
			d.closerEvent(d.closerReadFail, q, drain.EventError, "the closer's PR read was refused; the row stays reported: "+err.Error())
		}
		return
	default:
		pr = drain.PRRead{State: drain.PRState(cand.State), Number: cand.Number}
	}
	// A read that worked ends any failure condition: the next one is new.
	delete(d.closerReadFail, q.Name)
	dec := drain.DecideCloser(q, pr, now)
	if dec.Change.Writes() {
		written, ok := d.apply(q, dec.Change)
		if !ok {
			return
		}
		q = written
		if dec.Note != "" {
			d.closerEvent(d.closerNote, q, drain.EventNote, dec.Note)
		}
	}
	if !dec.Close {
		return
	}
	var cost *float64
	if dec.Merged && led.Transcript != "" {
		// A partial price (a model with no price) is not the session's cost.
		if u := d.io.price(ctx, led.Transcript); u != nil && u.Priced {
			c := u.CostUSD
			cost = &c
		}
	}
	res := d.io.closeRow(ctx, q, led)
	if !res.Closed {
		if cost != nil {
			if written, ok := d.apply(q, drain.Change{Name: q.Name, From: q.State, CostUSD: cost}); ok {
				q = written
			}
		}
		d.closerEvent(d.closerNote, q, drain.EventError, "close refused, the row stays reported: "+res.Reason)
		return
	}
	if res.ledgerFailed {
		// The workspace is closed but the ledger row still says the worker is
		// open. The row stays reported, so the next read closes again (herdr
		// then answers already gone) and retries the ledger write; a closed
		// queue row over an open ledger row would never be looked at again.
		note := "closed the worker, but its ledger row could not be updated; the row stays reported and the close is tried again: " + res.Note
		if written, ok := d.apply(q, drain.Change{Name: q.Name, From: q.State, Error: note, SetError: true, CostUSD: cost}); ok {
			q = written
		}
		d.closerEvent(d.closerNote, q, drain.EventError, note)
		return
	}
	why := dec.Why
	if res.Worktree == closeWorktreeKept {
		why += "; worktree kept: " + strings.Join(res.KeptBecause, "; ")
	}
	if res.Note != "" {
		why += "; " + res.Note
		d.closerEvent(d.closerNote, q, drain.EventNote, "close: "+res.Note)
	}
	d.apply(q, drain.Change{Name: q.Name, From: q.State, To: worker.QueueClosed, Error: why, CostUSD: cost})
}

// autopilot makes at most one merge attempt a tick, only while
// [surface.merge] resolves to mode auto (read again every tick, so setting
// the mode to anything else stops the next attempt). It reads the queue and
// ledgers again, since the closers may have just closed a row, and tries the
// reported row tried longest ago, at most every drain.AutopilotEvery, through
// the merge path `surface merge` uses, as the drain. A merge is one merged
// event and lets the closers read the row at the next tick; a refusal,
// failure or unconfirmed merge is one merge-refused event per row, head and
// reasons; GitHub unreadable is one unreadable event per condition.
func (d *drainer) autopilot(ctx context.Context) {
	if d.io.mergeSettings == nil || d.io.land == nil {
		return
	}
	s := d.io.mergeSettings()
	if s.Mode != config.MergeAuto {
		return
	}
	rows, ok := d.readRows()
	if !ok {
		return
	}
	ledgers := d.ledgers(rows)
	now := d.io.now()
	type candidate struct {
		q   worker.QueueRow
		led worker.Row
	}
	var due []candidate
	seen := map[string]bool{}
	for _, q := range rows {
		l := ledgers[q.Name]
		if !drain.AutopilotCandidate(q, l) {
			continue
		}
		seen[q.Name] = true
		if drain.AutopilotDue(d.autopilotTried[q.Name], now) {
			due = append(due, candidate{q, l.Row})
		}
	}
	for name := range d.autopilotTried {
		if !seen[name] {
			delete(d.autopilotTried, name)
		}
	}
	for name := range d.autopilotNote {
		if !seen[name] {
			delete(d.autopilotNote, name)
		}
	}
	if len(due) == 0 || d.stopping() {
		return
	}
	slices.SortStableFunc(due, func(a, b candidate) int {
		if c := d.autopilotTried[a.q.Name].Compare(d.autopilotTried[b.q.Name]); c != 0 {
			return c
		}
		return strings.Compare(a.q.Name, b.q.Name)
	})
	c := due[0]
	d.autopilotTried[c.q.Name] = now
	out := d.io.land(ctx, s, merge.ByDrain, mergeRow(c.led, &c.q), false)
	if out.AuditNote != "" && out.Result != merge.LandRefused {
		d.event(drain.Event{Kind: drain.EventError, Name: c.q.Name, Repo: c.q.Repo, State: string(c.q.State), Error: "merge audit: " + out.AuditNote})
	}
	switch out.Result {
	case merge.LandMerged:
		delete(d.autopilotNote, c.q.Name)
		delete(d.closerRead, c.q.Name) // the closers read it at the next tick
		d.event(drain.Event{Kind: drain.EventMerged, Name: c.q.Name, Repo: c.q.Repo, State: string(c.q.State),
			Error: fmt.Sprintf("PR #%d merged at head %s as %s (audit line %s); the closers close the worker at their next read", out.PR, shortSHA(out.Head), out.MergeCommit, shortSHA(out.AuditLine))})
	case merge.LandUnreadable:
		d.closerEvent(d.autopilotNote, c.q, drain.EventUnreadable, "the autopilot could not read the worker's PR: "+strings.Join(out.Reasons, "; "))
	default:
		set := slices.Clone(out.Reasons)
		slices.Sort(set)
		why := fmt.Sprintf("PR #%d at head %s: %s, %s", out.PR, shortSHA(out.Head), out.Result, strings.Join(slices.Compact(set), "; "))
		d.closerEvent(d.autopilotNote, c.q, drain.EventMergeRefused, why)
	}
}

// dailyPrune runs `surface prune` with the default cutoff once per UTC day.
// The day is recorded before the prune runs, so neither a failure nor a
// restart runs it again that day; a failure is an error event and never
// stops the drain. A drain-prune-day that cannot be read counts as never:
// one error event, and the prune runs and rewrites it with today.
func (d *drainer) dailyPrune(ctx context.Context) {
	now := d.io.now()
	if !d.pruneDayRead {
		day, err := d.io.pruneDay()
		if err != nil {
			day = ""
			d.event(drain.Event{Kind: drain.EventError, Error: "read the last daily prune day; pruning now and rewriting it with today: " + err.Error()})
		}
		d.pruneDay, d.pruneDayRead = day, true
	}
	if !drain.PruneDue(d.pruneDay, now) {
		return
	}
	d.pruneDay = now.UTC().Format(worker.UTCDayLayout)
	if err := d.io.setPruneDay(d.pruneDay); err != nil {
		d.event(drain.Event{Kind: drain.EventError, Error: "record the daily prune day: " + err.Error()})
	}
	res, err := d.io.prune(ctx, now)
	if err != nil {
		d.event(drain.Event{Kind: drain.EventError, Error: "daily prune: " + err.Error()})
		return
	}
	for _, it := range res.Removed {
		d.event(drain.Event{Kind: drain.EventState, Name: it.Name, Repo: it.Repo, State: "pruned", Error: it.Kind + " row: " + it.Reason})
	}
	for _, n := range res.Notes {
		d.event(drain.Event{Kind: drain.EventNote, Error: "daily prune: " + n})
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
// the row's harness, the row's brief text, and the branch worker/<name>. The
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
		// The row names its profile only; it resolves against the config
		// file as loaded now. An unknown name fails every row that names it
		// the same way, so it is a launch-config failure, before anything is
		// created.
		configDir, err := cfg.Surface.ProfileConfigDir(row.Profile, os.UserHomeDir)
		if err != nil {
			return drain.Attempt{Class: drain.ErrLaunchConfig, Err: "profile: " + err.Error(), CreatedNothing: true}
		}
		spec := drainSpec(row)
		spec.configDir = configDir
		attempt, err := launchFn(ctx, io.Discard, module.Deps{Runner: runner, Cfg: cfg}, spec, row.Brief)
		return attemptOf(attempt, err, row)
	}
}

// drainSpec is the worker launch for a claimed row: the row's harness (claude
// when it names none), branch worker/<name>, which must not exist yet, no
// $PATH binary, the row's model, and the claim's launch id for the ledger. The row's profile is resolved by the caller, against the
// config it launches with.
func drainSpec(row worker.QueueRow) workerSpec {
	return workerSpec{target: row.Repo, name: row.Name, branch: drain.Branch(row.Name), harness: row.Launch().Harness, allowPATH: false,
		launchID: row.LaunchID, model: row.Model, drain: true}
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
	case errors.Is(err, worker.ErrBranchExists):
		// The branch is there before the launch made anything; a retry
		// would find it again.
		return drain.ErrRowInvalid
	case launchConfigFailure(err):
		return drain.ErrLaunchConfig
	case herdrUnavailable(err):
		return drain.ErrHerdrDown
	case githubAuthFailure(err):
		return drain.ErrGitHubAuth
	case errors.Is(err, errIdentityRead):
		// GitHub could not be read before anything was created: pause and
		// requeue, never spend the row's attempts on an outage.
		return drain.ErrGitHubRead
	}
	return drain.ErrOther
}

// launchConfigFailure reports a launch whose configuration cannot build a
// worker: the build step, the worker posture or harness override, the binary
// policy (a claude found only on $PATH, unusable, or forgectl), or a private
// run directory the environment cannot provide (a TMPDIR that is unset, a
// symlink, too long, or shared without the sticky bit). Each fails every
// launch the same way, so a retry only leaves another worktree behind
// (forgectl#1188).
func launchConfigFailure(err error) bool {
	for _, target := range []error{errLaunchConfig, launch.ErrWorkerPosture, launch.ErrHarnessOverride,
		surface.ErrBinaryProvenance, surface.ErrBinaryUnusable, surface.ErrBinarySelfLoop,
		surface.ErrRunDir, surface.ErrSocketPathTooLong} {
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

// claudeSlotsTimeout caps one claude-slots check; a slower one launches
// without the cap. Tests shorten it.
var claudeSlotsTimeout = 5 * time.Second

// maxClaudeSlotsOutput bounds what a check's output is read into.
const maxClaudeSlotsOutput = 4 << 10

// lookClaudeSlots is the absolute claude-slots path on PATH, or "" when it is
// not there (or not usable): the drain then launches without the cap.
func lookClaudeSlots() string {
	path, err := osexec.LookPath("claude-slots")
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return abs
}

// cappedOutput keeps the first maxClaudeSlotsOutput bytes written to it and
// drops the rest, so a noisy tool cannot grow the drain.
type cappedOutput struct{ b []byte }

func (c *cappedOutput) Write(p []byte) (int, error) {
	if room := maxClaudeSlotsOutput - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// runClaudeSlots runs `<path> check <need>` with the drain's environment,
// stdin from /dev/null, and a claudeSlotsTimeout cap, and reports how it
// ended with the first non-empty line of its output as the reason. A check
// cut short by parent (the drain stopping) is Stopped, never a timeout.
func runClaudeSlots(parent context.Context, path string, need int) drain.SlotsCheck {
	if parent.Err() != nil {
		return drain.SlotsCheck{Need: need, Exit: -1, Stopped: true}
	}
	ctx, cancel := context.WithTimeout(parent, claudeSlotsTimeout)
	defer cancel()
	var out cappedOutput
	cmd := osexec.CommandContext(ctx, path, "check", strconv.Itoa(need)) //nolint:gosec // G204: the claude-slots path resolved at drain start, a count argument
	cmd.Stdout, cmd.Stderr = &out, &out
	// A child that keeps the pipes open after the kill must not hold the tick.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	c := drain.SlotsCheck{Need: need, Exit: -1, Reason: slotsReason(string(out.b))}
	var exitErr *osexec.ExitError
	switch {
	case err == nil:
		c.Exit = 0
	case parent.Err() != nil:
		c.Stopped = true
	case ctx.Err() != nil:
		c.TimedOut = true
	case errors.As(err, &exitErr):
		c.Exit = exitErr.ExitCode()
	default:
		c.Err = termsafe.SafeLineMax(err.Error(), 200)
	}
	return c
}

// slotsReason is the first non-empty line of s, safe to print and capped.
func slotsReason(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return termsafe.SafeLineMax(line, 200)
		}
	}
	return ""
}

// realDrainIO wires the tick to the queue, the ledgers, herdr, the config
// file, claude-slots (slotsPath, or "" when it was not found at start) and
// the launch path. emit records an event.
func realDrainIO(deps module.Deps, session, slotsPath string, emit func(drain.Event) error) (drainIO, error) {
	q, err := worker.OpenQueue()
	if err != nil {
		return drainIO{}, err
	}
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return drainIO{}, err
	}
	var slots func(context.Context, int) drain.SlotsCheck
	if slotsPath != "" {
		slots = func(ctx context.Context, need int) drain.SlotsCheck { return runClaudeSlots(ctx, slotsPath, need) }
	}
	return drainIO{
		slots: slots,
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
		prRead: func(ctx context.Context, row merge.Row) (merge.Candidate, error) {
			ctx, cancel := context.WithTimeout(ctx, statusTimeout)
			defer cancel()
			// No cache: the closers read live state only.
			return merge.Reader{GH: githubauth.Runner(deps.Runner, githubauth.DefaultHost)}.Discover(ctx, row)
		},
		price: func(ctx context.Context, transcript string) *statusUsage {
			return priceTranscript(ctx, deps.Runner, osexec.LookPath, transcript)
		},
		closeRow: drainCloseRow(deps.Runner, session),
		prune: func(ctx context.Context, now time.Time) (pruneResult, error) {
			d, err := realPruneDeps()
			if err != nil {
				return pruneResult{}, err
			}
			ctx, cancel := context.WithTimeout(ctx, drainPruneTimeout)
			defer cancel()
			return runPrune(ctx, d, now, drain.PruneAfter, false)
		},
		pruneDay:      files.ReadPruneDay,
		setPruneDay:   files.WritePruneDay,
		mergeSettings: localMergeSettings,
		land: func(ctx context.Context, s config.MergeSettings, by merge.By, row merge.Row, dryRun bool) merge.Outcome {
			l, err := realLander(deps.Runner)
			if err != nil {
				return merge.Outcome{Result: merge.LandFailed, Reasons: []string{"the merge audit file could not be opened: " + err.Error()}, Err: err}
			}
			ctx, cancel := context.WithTimeout(ctx, mergeTimeout)
			defer cancel()
			return l.Land(ctx, s, by, row, dryRun)
		},
	}, nil
}

// drainCloseRow is the closers' close: the close `surface close` runs
// (closeWorker through realCloseSteps), on the row's own ledger row, through
// a herdr adapter that must resolve the drain's pinned session.
func drainCloseRow(run exec.Runner, session string) func(context.Context, worker.QueueRow, worker.Row) closeResult {
	return func(ctx context.Context, q worker.QueueRow, led worker.Row) closeResult {
		refuse := func(why string) closeResult {
			return closeResult{Name: led.Name, Branch: led.Branch, Workspace: closeWorkspaceRefused, Worktree: closeWorktreeUntouched, Reason: why}
		}
		adapter, err := newHerdrAdapter(io.Discard)
		if err != nil {
			return refuse("herdr: " + err.Error())
		}
		herdr, ok := adapter.(*herdradapter.Adapter)
		if !ok {
			return refuse("the herdr adapter has an unexpected type")
		}
		if herdr.Session() != session || (q.Session != "" && q.Session != session) {
			return refuse(fmt.Sprintf("the worker is in herdr session %q, the drain's is %q", q.Session, session))
		}
		ledger, err := worker.Open(q.Repo, session)
		if err != nil {
			return refuse("the ledger: " + err.Error())
		}
		ctx, cancel := context.WithTimeout(ctx, closeTimeout)
		defer cancel()
		return closeWorker(ctx, led, false, time.Now(), realCloseSteps(run, herdr, ledger, q.Repo, led))
	}
}
