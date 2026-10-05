//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var exitLineRe = regexp.MustCompile(`^EXIT=(-?[0-9]{1,9})$`)

// The caps on what one Scan reads from pending/: at most ScanPendingMax items
// and ScanPendingBytes of their content, in name order. Each item is read in
// full to check its hash, so without them a scan's memory grows with the
// queue. Items past either cap are counted in Snapshot.PendingUnread.
const (
	ScanPendingMax   = 200
	ScanPendingBytes = 64 << 20
)

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
	read, budget := 0, int64(ScanPendingBytes)
	for _, e := range pending {
		name, kind, ok := kindOfFile(e.Name())
		if !ok {
			continue
		}
		size := int64(maxItemBytes)
		if fi, err := e.Info(); err == nil {
			size = min(fi.Size(), maxItemBytes)
		}
		if read >= ScanPendingMax || size > budget {
			// Past the caps the item is not read, so neither is its hash fixed
			// nor its change noticed; a later scan, once earlier items have
			// left pending/, reads it.
			snap.PendingUnread++
			continue
		}
		read++
		budget -= size
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
		it.Headers = d.headersOf(DirSkipped, e.Name())
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
	it.Meta, it.Content, it.Headers = meta, data, ParseHeaders(data)
	it.Stale = meta.AddedAt != nil && now.Sub(*meta.AddedAt) > StaleAfter
	return it, false, nil
}

// PendingItem reads one pending item again, for an operator about to review
// it: its bytes, headers and meta. Unlike Scan it changes nothing. It refuses
// an item with no recorded hash yet, or whose meta names another kind, and
// returns ErrChanged when the bytes no longer hash to wantSHA or to the
// recorded hash.
func (d *Desk) PendingItem(name, wantSHA string) (Item, error) {
	kind, err := d.findKind(DirPending, name)
	if err != nil {
		return Item{}, err
	}
	it := newItem(name, kind, StateWaiting)
	data, _, err := d.readItem(DirPending, name+kind.Ext())
	if err != nil {
		return Item{}, err
	}
	meta, ok, err := d.readMeta(DirPending, name)
	switch {
	case err != nil:
		return Item{}, err
	case !ok || meta.SHA256 == "":
		return Item{}, fmt.Errorf("%w: %s has no recorded hash yet; scan the desk first", ErrRefused, describe(name))
	case meta.Kind != "" && meta.Kind != kind:
		return Item{}, fmt.Errorf("%w: %s was queued as a %s, but the file found is a %s", ErrRefused, describe(name), meta.Kind, kind)
	}
	if sum := SHA256Hex(data); sum != meta.SHA256 || sum != wantSHA {
		return Item{}, fmt.Errorf("%w: %s", ErrChanged, describe(name))
	}
	it.Meta, it.Content, it.Headers = meta, data, ParseHeaders(data)
	it.Stale = meta.AddedAt != nil && d.now().UTC().Sub(*meta.AddedAt) > StaleAfter
	return it, nil
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
	it.Headers = d.headersOf(DirRunning, name+kind.Ext())
	if it.Meta.StartedAt != nil {
		it.Started = *it.Meta.StartedAt
	}
	if d.lost(name, kind, it.Meta) {
		it.State = StateLost
	}
	return it
}

// claimGrace is how long a claimed item with no owner and no claim stamp
// may sit in running/ before it reads as lost. Claim stamps the meta right
// after its renames, so only a claim that failed between the two, or a
// hand-moved item, is ever judged by it.
const claimGrace = 30 * time.Second

// lost reports a running item whose owner process is gone without writing
// RUN-END. With no owner recorded yet (PID 0) the claim decides: see
// [Desk.claimLost].
func (d *Desk) lost(name string, kind Kind, meta Meta) bool {
	if meta.PID != 0 {
		if processAlive(meta.PID, meta.PIDStart) {
			return false
		}
	} else if !d.claimLost(name, kind, meta) {
		return false
	}
	ended, _ := d.hasRunEnd(name)
	return !ended
}

// claimLost judges a running item with no owner recorded yet (PID 0). It is
// lost when the claim stamp names a process that is gone, or, with no stamp,
// when the item file was last modified more than claimGrace ago. Otherwise
// the claimer is still between Claim and Launch (or BeginRun).
func (d *Desk) claimLost(name string, kind Kind, meta Meta) bool {
	if meta.ClaimPID != 0 {
		return !processAlive(meta.ClaimPID, meta.ClaimStart)
	}
	fi, err := d.root.Lstat(path.Join(DirRunning, name+kind.Ext()))
	if err != nil {
		return false // gone since the listing: nothing left to judge
	}
	return d.now().Sub(fi.ModTime()) > claimGrace
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
		if !found || !ValidName(name) || inRunning[name] || !e.Type().IsRegular() {
			continue
		}
		kind := KindScript
		if d.exists(path.Join(DirDone, name+extManifest)) {
			kind = KindBatch
		}
		it := newItem(name, kind, StateDone)
		it.Meta, _, _ = d.readMeta(DirDone, name)
		it.Headers = d.headersOf(DirDone, name+kind.Ext())
		if it.Meta.ExitCode != nil {
			it.ExitCode = it.Meta.ExitCode
		} else if line, err := d.lastLine(name); err == nil { // legacy: no rc in meta
			if m := exitLineRe.FindStringSubmatch(line); m != nil {
				rc, _ := strconv.Atoi(m[1])
				it.ExitCode = &rc
			}
		}
		d.fillTimes(&it, e.Name())
		out = append(out, it)
	}
	return out, nil
}

// fillTimes sets Started and Ended from meta, or for a legacy item from the
// log done/<logFile>: its birth time (created when the run started) and its
// mtime (last written when it ended).
func (d *Desk) fillTimes(it *Item, logFile string) {
	if it.Meta.StartedAt != nil {
		it.Started = *it.Meta.StartedAt
	}
	if it.Meta.EndedAt != nil {
		it.Ended = *it.Meta.EndedAt
	}
	if !it.Started.IsZero() && !it.Ended.IsZero() {
		return
	}
	f, _, err := d.openRegular(DirDone, logFile)
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

// headersOf parses the header lines of sub/file, or returns none when it
// cannot be read as a regular file of at most maxItemBytes.
func (d *Desk) headersOf(sub, file string) Headers {
	data, err := d.readCapped(sub, file, maxItemBytes)
	if err != nil {
		return Headers{}
	}
	return ParseHeaders(data)
}

// maxStatusBytes caps a batch's status.tsv: one short row per step.
const maxStatusBytes = 4 << 20

// Manifest reads and parses a batch item's manifest from running/ or done/
// (the item's own bytes, kept as the record of what ran). It reads only a
// regular file of at most 1 MiB, through the pinned desk, so a symlink or a
// FIFO at the name is refused rather than followed or waited on.
func (d *Desk) Manifest(name string) (*Manifest, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	for _, sub := range [...]string{DirRunning, DirDone} {
		data, err := d.readCapped(sub, name+extManifest, maxItemBytes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return LoadManifest(data, name+extManifest)
	}
	return nil, ErrNotFound
}

// StatusRow is one step's row in a batch's status.tsv, as the runner writes
// it: step, state, rc, start, end, deps, with "-" for a value not set yet.
// Step, State and Deps are raw file text: render them through termsafe.
type StatusRow struct {
	Step  string
	State string
	// RC is nil until the step ends.
	RC *int
	// Start and End are zero until the step starts and ends.
	Start time.Time
	End   time.Time
	Deps  []string
}

// statusHeader is the first line of every status.tsv the runner writes.
const statusHeader = "step\tstate\trc\tstart\tend\tdeps"

// BatchStatus reads done/<name>.d/status.tsv, the runner's per-step state,
// rewritten whole as each step starts and ends. It reads only a regular file
// of at most 4 MiB, through the pinned desk.
func (d *Desk) BatchStatus(name string) ([]StatusRow, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	data, err := d.readCapped(path.Join(DirDone, name+extBatchDir), "status.tsv", maxStatusBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return parseStatus(string(data))
}

func parseStatus(text string) ([]StatusRow, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if lines[0] != statusHeader {
		return nil, errors.New("desk: status.tsv does not start with the runner's header")
	}
	rows := make([]StatusRow, 0, len(lines)-1)
	for i, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("desk: status.tsv line %d has %d fields, want 6", i+2, len(f))
		}
		row := StatusRow{Step: f[0], State: f[1]}
		var err error
		if f[2] != "-" {
			rc, perr := strconv.Atoi(f[2])
			row.RC, err = &rc, perr
		}
		if err == nil {
			row.Start, err = parseEpoch(f[3])
		}
		if err == nil {
			row.End, err = parseEpoch(f[4])
		}
		if err != nil {
			return nil, fmt.Errorf("desk: status.tsv line %d: %w", i+2, err)
		}
		if f[5] != "-" {
			row.Deps = strings.Split(f[5], ",")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseEpoch reads a status.tsv time, seconds since the epoch with a
// fraction ("-" is zero).
func parseEpoch(s string) (time.Time, error) {
	if s == "-" {
		return time.Time{}, nil
	}
	secs, err := strconv.ParseFloat(s, 64)
	// Past ±1e12 s (about 31,700 years) int64(whole) below would depend on
	// the platform.
	if err != nil || math.IsNaN(secs) || math.Abs(secs) > 1e12 {
		return time.Time{}, fmt.Errorf("bad time %q", describe(s))
	}
	whole, frac := math.Modf(secs)
	return time.Unix(int64(whole), int64(math.Round(frac*1e3))*int64(time.Millisecond)).UTC(), nil
}

func isNotExist(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}
