//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var exitLineRe = regexp.MustCompile(`^EXIT=(-?[0-9]{1,9})$`)

// Scan reads the queue. It is not read-only, by design: it fixes the hash of
// a hand-dropped pending item the first time it sees it, and moves a pending
// item whose bytes changed since then to skipped/ (skip_reason "changed").
func (d *Desk) Scan() (*Snapshot, error) {
	now := d.now().UTC()
	snap := &Snapshot{Dir: d.path, Taken: now}

	pending, err := d.list(DirPending)
	if err != nil {
		return nil, err
	}
	for _, e := range pending {
		name, kind, ok := kindOfFile(e.Name())
		if !ok {
			continue
		}
		it, gone, err := d.sight(name, kind, now)
		if err != nil {
			return nil, err
		}
		if !gone {
			snap.Pending = append(snap.Pending, it)
		}
	}

	running, err := d.list(DirRunning)
	if err != nil {
		return nil, err
	}
	inRunning := map[string]bool{}
	for _, e := range running {
		name, kind, ok := kindOfFile(e.Name())
		if !ok {
			continue
		}
		inRunning[name] = true
		snap.Running = append(snap.Running, d.runningItem(name, kind))
	}

	if snap.Done, err = d.doneItems(inRunning); err != nil {
		return nil, err
	}

	skipped, err := d.list(DirSkipped)
	if err != nil {
		return nil, err
	}
	for _, e := range skipped {
		name, kind, ok := kindOfFile(e.Name())
		if !ok {
			continue
		}
		it := newItem(name, kind, StateSkipped)
		it.Meta, _, _ = d.readMeta(DirSkipped, name)
		if data, err := d.readRegular(path.Join(DirSkipped, e.Name()), maxItemBytes); err == nil {
			it.Headers = HeadersFor(kind, data)
		}
		snap.Skipped = append(snap.Skipped, it)
	}

	byNumber := func(a, b Item) int {
		if c := a.Number - b.Number; c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	}
	slices.SortFunc(snap.Pending, byNumber)
	slices.SortFunc(snap.Running, byNumber)
	slices.SortFunc(snap.Skipped, byNumber)
	slices.SortFunc(snap.Done, func(a, b Item) int { // most recent first
		if c := b.Ended.Compare(a.Ended); c != 0 {
			return c
		}
		return -byNumber(a, b)
	})
	return snap, nil
}

func newItem(name string, kind Kind, state State) Item {
	n, stem := SplitName(name)
	return Item{Name: name, Number: n, Stem: stem, Kind: kind, State: state}
}

// sight reads one pending item. gone is true when the item left pending/
// during the scan: claimed by another desk, or moved to skipped/ because it
// changed.
func (d *Desk) sight(name string, kind Kind, now time.Time) (it Item, gone bool, err error) {
	it = newItem(name, kind, StateWaiting)
	data, mtime, err := d.readItem(DirPending, name+kind.Ext())
	var r *refusal
	switch {
	case errors.Is(err, ErrNotFound):
		return it, true, nil
	case errors.As(err, &r):
		it.State, it.Refusal = StateRefused, r.reason
		it.Meta, _, _ = d.readMeta(DirPending, name)
		return it, false, nil
	case err != nil:
		return it, false, err
	}
	sum := SHA256Hex(data)
	meta, ok, err := d.readMeta(DirPending, name)
	if errors.As(err, &r) {
		it.State, it.Refusal = StateRefused, r.reason
		return it, false, nil
	}
	if err != nil {
		return it, false, err
	}
	if !ok || meta.SHA256 == "" {
		// First sighting of a hand-dropped item: its hash is fixed now. The
		// file's mtime stands in for its arrival.
		meta = Meta{AddedAt: &mtime, SHA256: sum, Kind: kind}
		created, err := d.createMeta(DirPending, name, meta)
		if err != nil {
			return it, false, err
		}
		if !created { // another desk, or Add, recorded it first: theirs stands
			if meta, _, err = d.readMeta(DirPending, name); err != nil {
				return it, false, err
			}
		}
	}
	if meta.Kind != "" && meta.Kind != kind {
		it.State, it.Refusal, it.Meta = StateRefused, "another item already uses this name", meta
		return it, false, nil
	}
	if meta.SHA256 != sum {
		if err := d.skipChanged(DirPending, name, kind, meta); err != nil {
			return it, false, err
		}
		return it, true, nil
	}
	it.Meta, it.Content, it.Headers = meta, data, HeadersFor(kind, data)
	it.Stale = meta.AddedAt != nil && now.Sub(*meta.AddedAt) > StaleAfter
	return it, false, nil
}

// skipChanged moves an item whose bytes no longer match its hash to
// skipped/. It cannot be re-armed; a new item is queued instead.
func (d *Desk) skipChanged(from, name string, kind Kind, meta Meta) error {
	meta.SkipReason = SkipChanged
	if err := d.writeMeta(from, name, meta); err != nil {
		return err
	}
	if err := d.move(name, kind, from, DirSkipped); err != nil && !isNotExist(err) {
		return fmt.Errorf("desk: move changed item %s to skipped/: %w", name, err)
	}
	return nil
}

func (d *Desk) runningItem(name string, kind Kind) Item {
	it := newItem(name, kind, StateRunning)
	it.Meta, _, _ = d.readMeta(DirRunning, name)
	if data, err := d.readRegular(path.Join(DirRunning, name+kind.Ext()), maxItemBytes); err == nil {
		it.Headers = HeadersFor(kind, data)
	}
	if it.Meta.StartedAt != nil {
		it.Started = *it.Meta.StartedAt
	}
	if d.lost(name, it.Meta) {
		it.State = StateLost
	}
	return it
}

// lost reports a running item whose owner process is gone without writing
// RUN-END, or a claim that never got an owner within ClaimGrace. A held
// owner lock is a live owner, whatever the pid or the clock says.
func (d *Desk) lost(name string, meta Meta) bool {
	if d.ownerAlive(name) {
		return false
	}
	return d.lostUnlocked(name, meta)
}

// lostUnlocked is lost for a caller that already holds the owner lock (or
// has just seen it free).
func (d *Desk) lostUnlocked(name string, meta Meta) bool {
	if meta.PID == 0 {
		return d.ownerless(name, meta)
	}
	if processAlive(meta.PID, meta.PIDStart) {
		return false
	}
	ended, _ := d.hasRunEnd(name)
	return !ended
}

// ownerless reports a running item with no owner recorded whose claim is
// older than ClaimGrace. The claim time is meta's claimed_at, or for a
// claim made before that field existed, the running/ file's mtime (Claim
// writes that file last). A running item with no meta yet (no hash) is a
// claim in progress: Claim moves the item before its meta, and writes
// claimed_at into the pending meta before either move. That pending meta
// times the claim, so a claim that died between the two moves is lost after
// the grace too. With no meta anywhere the item is never lost.
func (d *Desk) ownerless(name string, meta Meta) bool {
	if meta.SHA256 == "" {
		pending, ok, err := d.readMeta(DirPending, name)
		if err != nil || !ok || pending.ClaimedAt == nil {
			return false
		}
		meta = pending
	}
	var at time.Time
	if meta.ClaimedAt != nil {
		at = *meta.ClaimedAt
	} else if kind, err := d.findKind(DirRunning, name); err == nil {
		if fi, err := d.root.Lstat(path.Join(DirRunning, name+kind.Ext())); err == nil {
			at = fi.ModTime()
		}
	}
	return !at.IsZero() && d.now().Sub(at) > ClaimGrace
}

// doneItems lists history: one item per done/<name>.log, except names still
// in running/ (a running item's log is already in done/).
func (d *Desk) doneItems(inRunning map[string]bool) ([]Item, error) {
	entries, err := d.list(DirDone)
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, e := range entries {
		name, found := strings.CutSuffix(e.Name(), extLog)
		legacy := found && legacyName(name)
		if !found || (!ValidName(name) && !legacy) || inRunning[name] || !e.Type().IsRegular() {
			continue
		}
		kind := KindScript
		if d.exists(path.Join(DirDone, name+extManifest)) {
			kind = KindBatch
		}
		it := newItem(name, kind, StateDone)
		it.Legacy = legacy
		it.Meta, _, _ = d.readMeta(DirDone, name)
		if data, err := d.readRegular(path.Join(DirDone, name+kind.Ext()), maxItemBytes); err == nil {
			it.Headers = HeadersFor(kind, data)
		}
		logPath := path.Join(DirDone, e.Name())
		if it.Meta.ExitCode != nil {
			it.ExitCode = it.Meta.ExitCode
		} else if line, err := d.lastLine(logPath); err == nil { // legacy: no rc in meta
			if m := exitLineRe.FindStringSubmatch(line); m != nil {
				rc, _ := strconv.Atoi(m[1])
				it.ExitCode = &rc
			}
		}
		d.fillTimes(&it, logPath)
		out = append(out, it)
	}
	return out, nil
}

// fillTimes sets Started and Ended from meta, or for a legacy item from the
// log: its birth time (created when the run started) and its mtime (last
// written when it ended).
func (d *Desk) fillTimes(it *Item, logPath string) {
	if it.Meta.StartedAt != nil {
		it.Started = *it.Meta.StartedAt
	}
	if it.Meta.EndedAt != nil {
		it.Ended = *it.Meta.EndedAt
	}
	if !it.Started.IsZero() && !it.Ended.IsZero() {
		return
	}
	f, err := d.openRegular(logPath)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // read-only
	if it.Started.IsZero() {
		if born, ok := fileBirth(f); ok {
			it.Started = born
		}
	}
	if it.Ended.IsZero() {
		if fi, err := f.Stat(); err == nil {
			it.Ended = fi.ModTime().UTC()
		}
	}
}

func isNotExist(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}
