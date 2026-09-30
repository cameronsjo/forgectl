package pr

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Prune outcomes. `kept` is the one that is not an action: a file this sweep
// LISTED and deliberately did not touch — too young, or carrying a name whose
// age could not be read. It appears in the report because a sweep that silently
// skips a file answers "nothing to do" about a directory that still has
// something in it.
const (
	pruneOutcomeRemoved      = "removed"
	pruneOutcomeWouldRemove  = "would-remove"
	pruneOutcomeKept         = "kept"
	pruneOutcomeRefused      = "refused"
	pruneOutcomeFailed       = "failed"
	pruneOutcomeDeclined     = "declined"
	pruneOutcomeCompacted    = "compacted"
	pruneOutcomeWouldCompact = "would-compact"
	pruneOutcomeUnchanged    = "unchanged"
)

// asideSuffixHexLen is the length of the collision suffix freeAsideName appends
// when two records are set aside inside one second — randomSuffix's 8 bytes,
// hex-encoded.
const asideSuffixHexLen = 16

// asideStampFloor is the earliest set-aside stamp this build will believe:
// 2025-01-01T00:00:00Z, comfortably before forgectl's first release and
// therefore before any set-aside file can exist.
//
// A STAMP BELOW IT IS A CLOCK THAT LIED, NOT AN OLD FILE. freeAsideName writes
// the stamp from time.Now(), so a host that has not synced NTP yet — a fresh
// container or VM — names a record it set aside seconds ago at 1970. Read as an
// age, that is ~56 years: past every retention window, so the next sweep would
// unlink a brand-new record permanently. The floor routes those names into the
// same branch an unreadable name takes: LISTED, never removed, which is the
// rule parseAsideAge's own doc comment states.
//
// The floor is deliberately in the past rather than "within N years of now":
// the clock that produced the stamp is the same clock that would evaluate such
// a rule, so a rule reading `now` cannot detect the failure it exists for.
const asideStampFloor = 1735689600 // 2025-01-01T00:00:00Z

// maxRetentionDays bounds the `<N>d` retention form. It exists so the
// multiplication into a time.Duration cannot overflow into a small or negative
// window, which would read as a retention the operator never asked for.
const maxRetentionDays = 100 * 365

// minRetention is the shortest window parseRetention will accept. See its doc
// comment: refusing only zero left `1ns` as an equivalent way to say "remove
// everything", which is what a unit typo produces.
const minRetention = time.Hour

// PruneOpts drives one `pr repair --prune` invocation.
type PruneOpts struct {
	// OlderThan is how old a set-aside file's NAME must say it is before it is
	// a removal candidate.
	OlderThan time.Duration
	// LogRetention is how far back the repair audit log keeps settled rows.
	LogRetention time.Duration
	// DryRun prints what would happen and touches nothing.
	DryRun bool
	// Yes is the off-a-TTY confirmation this destructive sweep requires.
	Yes bool
}

// PruneItem is one set-aside file the sweep considered, and what became of it.
type PruneItem struct {
	Path string `json:"path"`
	// Ref is the best-effort ref read from the file's raw bytes, used for one
	// thing only — refusing while that ref's review window is live.
	Ref string `json:"ref,omitempty"`
	// Age is how old the file's NAME says it is, rendered for a human. Empty
	// when the name carries no readable timestamp.
	Age     string `json:"age,omitempty"`
	Outcome string `json:"outcome"`
	// Reason says why a file was kept or refused, because an outcome alone
	// names no way forward.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// PruneLog is what the sweep did to the repair audit log.
type PruneLog struct {
	Path    string `json:"path"`
	Dropped int    `json:"dropped"`
	Kept    int    `json:"kept"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// PruneReport is what `pr repair --prune` returns and `--json` encodes. It is
// an OBJECT with two members rather than a bare array: the sweep answers two
// questions — what happened to the files, and what happened to the log — and a
// list of files could only ever carry the first.
type PruneReport struct {
	Items []PruneItem `json:"items"`
	Log   PruneLog    `json:"log"`
}

// ParseRetention is parseRetention for the CLI, which must reject a malformed
// window before it opens a Client — the refusal belongs to the flag, not to the
// sweep.
func ParseRetention(s string) (time.Duration, error) { return parseRetention(s) }

// parseRetention reads a retention window: everything time.ParseDuration
// accepts, plus the `<N>d` form nobody wants to spell as hours.
//
// ANYTHING UNDER minRetention REFUSES, not just zero and negative. Refusing
// only zero was a floor in name alone: `--older-than 1ns` puts every record
// past retention just as completely, and it is exactly the value a unit typo
// produces. A window shorter than an hour is not a retention policy, and the
// consequence of getting it wrong here is an unlink.
func parseRetention(s string) (time.Duration, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, errors.New("a retention window is required; give a duration like 30d or 720h")
	}
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%s is not a number of days; give a duration like 30d or 720h",
				termsafe.QuotePath(text))
		}
		if n <= 0 || n > maxRetentionDays {
			return 0, fmt.Errorf("retention window %s must be between 1d and %dd",
				termsafe.QuotePath(text), maxRetentionDays)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%s is not a duration; give something like 30d, 720h, or 90m",
			termsafe.QuotePath(text))
	}
	if d < minRetention {
		return 0, fmt.Errorf("retention window %s is shorter than the %s floor; "+
			"a window that short puts every record past retention, which is what a unit typo looks like",
			termsafe.QuotePath(text), minRetention)
	}
	return d, nil
}

// parseAsideAge reads when a record was set aside FROM ITS NAME.
//
// The name is the only honest clock here. A rename preserves mtime, so a
// set-aside file's mtime dates the record's last WRITE — often long before it
// was set aside, and in the wrong direction: it would make a file look older
// than it is and hand it to the sweep early. freeAsideName stamps the set-aside
// second into the name precisely once, and that is what this reads.
//
// A name it cannot read yields no time, and the caller LISTS such a file rather
// than removing it. Anything else would mean deleting a file whose age nothing
// established. A stamp below asideStampFloor is treated the same way, for the
// same reason: an untrusted clock establishes nothing either.
func parseAsideAge(name string) (time.Time, bool) {
	ts, ok := parseAsideStamp(name)
	if !ok || ts.Unix() < asideStampFloor {
		return time.Time{}, false
	}
	return ts, true
}

// parseAsideStamp is the name parse WITHOUT the trusted-clock floor — the
// literal bytes of the name, and nothing about whether they are believable.
// Split out so the caller can tell an UNREADABLE name from a readable one whose
// clock lied, which are the same outcome but not the same message.
func parseAsideStamp(name string) (time.Time, bool) {
	idx := strings.LastIndex(name, unreadableSuffix)
	if idx < 0 || !strings.HasSuffix(name[:idx], ".json") {
		return time.Time{}, false
	}
	rest := name[idx+len(unreadableSuffix):]
	digits := rest
	if dash := strings.IndexByte(rest, '-'); dash >= 0 {
		digits = rest[:dash]
		suffix := rest[dash+1:]
		if len(suffix) != asideSuffixHexLen {
			return time.Time{}, false
		}
		if _, err := hex.DecodeString(suffix); err != nil {
			return time.Time{}, false
		}
	}
	if digits == "" {
		return time.Time{}, false
	}
	// Digits only, explicitly: ParseInt would accept a leading sign, and a
	// signed stamp is not something freeAsideName can produce.
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return time.Time{}, false
		}
	}
	secs, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

// undatedReason says WHY a name yielded no usable age, because "kept" alone
// names no way forward and the two causes call for different actions: an
// unreadable name is someone's hand edit, an epoch-era one is a host whose
// clock was not synced when the record was set aside.
func undatedReason(name string) string {
	if ts, ok := parseAsideStamp(name); ok && ts.Unix() < asideStampFloor {
		return fmt.Sprintf("its set-aside stamp (%s) predates the %s floor, so the clock that wrote it was not trusted",
			ts.Format(time.RFC3339), time.Unix(asideStampFloor, 0).UTC().Format(time.RFC3339))
	}
	return "its name carries no readable set-aside timestamp, so nothing established its age"
}

// repairGroup is everything compaction needs to know about one row id, and
// the only per-id state the two passes hold. What compaction may drop is
// decided per group, over the log's RAW LINES, and it is deliberately
// conservative in four directions at once — every one of them a shape where
// dropping would destroy the only pointer left to something on disk:
//
//   - an UNPAIRED intent is kept at any age. That row is the signal that a
//     repair died mid-delete, and its workspace field is the only thing naming
//     a possibly-orphaned clean room. Age does not make it less true.
//   - an UNPARSEABLE line is kept, verbatim, and so is an OVER-LONG one
//     (maxRepairLogLineBytes or more) whether or not it would decode. Nothing
//     may drop what it cannot read, and readRepairLogTail already skips both
//     on the way out, so keeping them costs a reader nothing.
//   - a row with no timestamp is kept, and so is every row sharing its id: no
//     age was established, so no retention decision is available.
//   - a pair straddling the cutoff is kept whole, so a completion can never
//     outlive the intent it settles.
//
// What is left is exactly the settled history: an intent with an applied or
// failed completion beside it, both older than the cutoff.
type repairGroup struct {
	intent    bool
	completed bool
	// datable is false as soon as one row in the group carries no
	// timestamp; old is false as soon as one row is inside the window.
	datable bool
	old     bool
	// rows counts the lines carrying this id, so pass one can report what
	// the copy will drop without holding a single one of them.
	rows int
}

func (g *repairGroup) settled() bool { return g.intent && g.completed && g.datable && g.old }

// errRepairLogChanged is pass two finding a different log from the one pass
// one planned over. The lifecycle lock is what rules that out — every writer
// of the log appends under it, and compaction holds it across both passes — so
// this is a tripwire for a writer outside that contract (a hand edit, a second
// host on a shared $HOME), not a path any forgectl process takes.
var errRepairLogChanged = errors.New("the repair audit log changed between the compaction's two passes; " +
	"nothing was dropped")

// repairLogPlan is pass one's result: the per-id classification, and the
// counts pass two must reproduce exactly.
//
// MEMORY IS BOUNDED BY THE NUMBER OF DISTINCT ROW IDS, not by the log's size:
// one repairGroup plus its id string per distinct id that appears on a
// parseable line. An id comes off a line under maxRepairLogLineBytes, so no
// single id exceeds that bound either. No line is held: both passes read
// through one maxRepairLogLineBytes buffer, and an over-long line — the only
// kind that does not fit it — streams through in buffer-sized chunks.
type repairLogPlan struct {
	groups map[string]*repairGroup
	// size is the bytes pass one read. Pass two refuses unless it reads the
	// same number, which catches an append between the passes.
	size int64
	// sum is the SHA-256 of those bytes. Size and counts alone miss a
	// same-length edit that keeps every count — a completion's timestamp moved
	// from old to in-window at the same width, say — so pass two also refuses
	// unless the bytes it read hash the same (forgectl#621). It covers the two
	// passes only: an edit after pass two has read, before the temp copy is
	// renamed over the log, is not seen. The lifecycle lock is what rules that
	// window out for every forgectl writer, as it does for the passes.
	sum     [sha256.Size]byte
	kept    int
	dropped int
}

// decodeRepairLine is the ONE parse both passes run, so pass two cannot
// classify a line differently from pass one. A line with no id pairs with
// nothing and is reported as not parsed.
func decodeRepairLine(line []byte) (RepairRow, bool) {
	var row RepairRow
	if err := json.Unmarshal(line, &row); err != nil || row.ID == "" {
		return RepairRow{}, false
	}
	return row, true
}

// drops reports whether pass two leaves line out.
func (p *repairLogPlan) drops(line []byte) bool {
	row, ok := decodeRepairLine(line)
	if !ok {
		return false
	}
	g := p.groups[row.ID]
	return g != nil && g.settled()
}

// walkRepairLog reads the log once, oldest line first, holding no more than
// one maxRepairLogLineBytes buffer, and returns the bytes it read.
//
// A line under the bound goes to line, without its '\n' and only if it is not
// empty; the slice is borrowed and valid only for the call. An over-long line
// — maxRepairLogLineBytes or more before its '\n', the exact bound
// readRepairLogTail skips at, since both read through the same buffer size —
// goes to long in buffer-sized chunks instead, never assembled, with end set on
// its last chunk (from which the '\n' is stripped). Only the '\n' is ever
// stripped: a '\r' before it is content. A final line with no '\n' is
// delivered like any other.
func walkRepairLog(r io.Reader, line func([]byte) error, long func(chunk []byte, end bool) error) (int64, error) {
	br := bufio.NewReaderSize(r, maxRepairLogLineBytes)
	var n int64
	for {
		b, err := br.ReadSlice('\n')
		n += int64(len(b))
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				if lerr := long(b, false); lerr != nil {
					return n, lerr
				}
				b, err = br.ReadSlice('\n')
				n += int64(len(b))
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return n, fmt.Errorf("read repair audit log: %w", err)
			}
			if lerr := long(bytes.TrimSuffix(b, []byte("\n")), true); lerr != nil {
				return n, lerr
			}
			if err != nil {
				return n, nil // io.EOF, after an over-long final line with no '\n'
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return n, fmt.Errorf("read repair audit log: %w", err)
		}
		if b = bytes.TrimSuffix(b, []byte("\n")); len(b) > 0 {
			if lerr := line(b); lerr != nil {
				return n, lerr
			}
		}
		if err != nil {
			return n, nil // io.EOF, after a final line with no '\n'
		}
	}
}

// scanRepairLog is pass one: it classifies every id in r and counts what the
// copy will keep and drop, holding nothing per line.
func scanRepairLog(r io.Reader, cutoff time.Time) (*repairLogPlan, error) {
	p := &repairLogPlan{groups: make(map[string]*repairGroup)}
	lines := 0
	h := sha256.New()
	size, err := walkRepairLog(io.TeeReader(r, h), func(line []byte) error {
		lines++
		row, ok := decodeRepairLine(line)
		if !ok {
			return nil
		}
		g := p.groups[row.ID]
		if g == nil {
			g = &repairGroup{datable: true, old: true}
			p.groups[row.ID] = g
		}
		g.rows++
		switch row.Outcome {
		case repairOutcomeIntent:
			g.intent = true
		case repairOutcomeApplied, repairOutcomeFailed:
			g.completed = true
		}
		switch {
		case row.TS.IsZero():
			g.datable = false
		case !row.TS.Before(cutoff):
			g.old = false
		}
		return nil
	}, func(_ []byte, end bool) error {
		// An over-long line is kept unparsed even if it would decode: every
		// reader skips it (readRepairLogTail), so no reader could have seen
		// it settle, and the rewriter must not be the one place it counts.
		if end {
			lines++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.size = size
	h.Sum(p.sum[:0])
	for _, g := range p.groups {
		if g.settled() {
			p.dropped += g.rows
		}
	}
	p.kept = lines - p.dropped
	return p, nil
}

// copyKept is pass two: it re-reads r, which must be the log pass one planned
// over, and writes every line the plan keeps to w, each terminated by '\n' —
// so an unterminated final line gains one, and an empty line is gone. Kept
// lines are written byte for byte, an over-long one included.
//
// It refuses with errRepairLogChanged when r is not the log the plan was made
// over, measured by bytes read, by their SHA-256, and by the kept and dropped
// counts; w then holds a partial copy the caller must discard.
func (p *repairLogPlan) copyKept(r io.Reader, w io.Writer) error {
	bw := bufio.NewWriterSize(w, maxRepairLogLineBytes)
	kept, dropped := 0, 0
	write := func(b []byte) error {
		if _, err := bw.Write(b); err != nil {
			return fmt.Errorf("write temp audit log: %w", err)
		}
		return nil
	}
	nl := []byte("\n")
	h := sha256.New()
	size, err := walkRepairLog(io.TeeReader(r, h), func(line []byte) error {
		if p.drops(line) {
			dropped++
			return nil
		}
		kept++
		if err := write(line); err != nil {
			return err
		}
		return write(nl)
	}, func(chunk []byte, end bool) error {
		if err := write(chunk); err != nil {
			return err
		}
		if !end {
			return nil
		}
		kept++
		return write(nl)
	})
	if err != nil {
		return err
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	if size != p.size || kept != p.kept || dropped != p.dropped {
		return fmt.Errorf("%w (planned %d bytes, %d kept, %d dropped; read %d bytes, %d kept, %d dropped)",
			errRepairLogChanged, p.size, p.kept, p.dropped, size, kept, dropped)
	}
	if sum != p.sum {
		return fmt.Errorf("%w (the same %d bytes and counts, but different content)", errRepairLogChanged, size)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write temp audit log: %w", err)
	}
	return nil
}

// asideCandidate is one set-aside file the sweep enumerated, pinned at the
// moment it was read: the identity to recheck before the unlink, and the exact
// bytes the audit row will carry and the re-read must match.
type asideCandidate struct {
	name   string
	path   string
	info   fs.FileInfo
	bytes  []byte
	ref    Ref
	hasRef bool
	item   PruneItem
}

func (a *asideCandidate) refString() string {
	if !a.hasRef {
		return ""
	}
	return a.ref.String()
}

// Prune reaps set-aside records past their retention window and compacts the
// repair audit log.
//
// It is a COMPOSITE verb like Repair: one lifecycle-lock hold, unlocked cores
// only. Everything that can refuse runs before the first intent row, for the
// same reason every other repair arm does it — a dangling intent means "a
// repair died mid-delete", so a refusal that wrote one would forge that signal.
//
// The sweep is per item. A live window, an unreadable window list, or a file
// that changed underfoot refuses THAT file and nothing else: one unsettled
// record must not stop the rest of the directory from being cleaned up.
func (c *Client) Prune(ctx context.Context, opts PruneOpts) (PruneReport, error) {
	if err := validatePruneOpts(opts); err != nil {
		return PruneReport{Items: []PruneItem{}}, err
	}
	report := PruneReport{Items: []PruneItem{}}
	err := c.withLifecycleLock(ctx, "repair-prune", func() error {
		var ierr error
		report, ierr = c.pruneLocked(ctx, opts)
		return ierr
	})
	if report.Items == nil {
		report.Items = []PruneItem{}
	}
	return report, err
}

// validatePruneOpts enforces the grammar before anything is read, so a
// malformed invocation never reaches the filesystem.
func validatePruneOpts(opts PruneOpts) error {
	if opts.OlderThan <= 0 {
		return errors.New("--older-than must be a positive duration; a zero window would put every set-aside record past retention")
	}
	if opts.LogRetention <= 0 {
		return errors.New("--log-retention must be a positive duration; a zero window would drop the whole settled audit trail")
	}
	return nil
}

func (c *Client) pruneLocked(ctx context.Context, opts PruneOpts) (PruneReport, error) {
	now := time.Now().UTC()
	report := PruneReport{Items: []PruneItem{}, Log: PruneLog{Path: c.repairLogPath(), Outcome: pruneOutcomeUnchanged}}

	dirInfo, candidates, err := c.enumerateAsideFiles(now, opts.OlderThan)
	if err != nil {
		return report, err
	}
	removable := c.screenLiveWindows(ctx, candidates)

	cutoff := now.Add(-opts.LogRetention)
	plan, readErr := c.scanRepairLogFile(cutoff)
	dropped := 0
	if readErr != nil {
		// An unreadable log refuses COMPACTION only. The removals below still
		// append to it fine, and refusing the whole sweep over a line nobody can
		// parse would make one hand edit permanent.
		report.Log.Outcome = pruneOutcomeRefused
		report.Log.Error = termsafe.SafeLine(safeErrString(readErr))
		slog.Warn("Refusing to compact the repair audit log: it could not be read back.",
			"path", c.repairLogPath(), "error", readErr)
	} else {
		dropped, report.Log.Kept = plan.dropped, plan.kept
	}

	// NOTHING REMOVABLE RETURNS BEFORE THE GATE. A no-op sweep has nothing to
	// confirm, so refusing it off a terminal would refuse the one invocation
	// that was always safe.
	if len(removable) == 0 && dropped == 0 {
		report.Items = pruneItems(candidates)
		return report, nil
	}

	if opts.DryRun {
		for _, cand := range removable {
			cand.item.Outcome = pruneOutcomeWouldRemove
		}
		if dropped > 0 {
			report.Log.Outcome = pruneOutcomeWouldCompact
			report.Log.Dropped = dropped
		}
		report.Items = pruneItems(candidates)
		return report, nil
	}

	if !opts.Yes {
		if !c.isTTY() {
			report.Items = pruneItems(candidates)
			return report, fmt.Errorf("refusing to prune without confirmation: this UNLINKS %d set-aside record(s) "+
				"and rewrites %s, and there is no terminal to confirm on — pass --yes to proceed, "+
				"or --dry-run to see what it would do",
				len(removable), termsafe.QuotePath(c.repairLogPath()))
		}
		approved, cerr := c.confirmRemoval(prunePrompt(len(removable), dropped, c.repairLogPath()))
		if cerr != nil {
			report.Items = pruneItems(candidates)
			return report, fmt.Errorf("prune confirmation: %w", cerr)
		}
		if !approved {
			for _, cand := range removable {
				cand.item.Outcome = pruneOutcomeDeclined
			}
			report.Items = pruneItems(candidates)
			return report, nil
		}
	}

	for _, cand := range removable {
		c.pruneOne(cand, dirInfo)
	}
	if dropped > 0 && readErr == nil {
		c.compactRepairLog(cutoff, &report.Log)
	}
	report.Items = pruneItems(candidates)
	return report, nil
}

// enumerateAsideFiles lists every set-aside file and pins each one's identity
// and bytes. It returns the session directory's identity alongside, so the
// removal can prove the directory did not change underneath it.
func (c *Client) enumerateAsideFiles(now time.Time, olderThan time.Duration) (fs.FileInfo, []*asideCandidate, error) {
	canonicalDir, err := filepath.EvalSymlinks(c.sessionsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve pr sessions dir %s: %w", termsafe.QuotePath(c.sessionsDir), err)
	}
	dirInfo, err := os.Lstat(canonicalDir)
	if err != nil {
		return nil, nil, fmt.Errorf("stat pr sessions dir %s: %w", termsafe.QuotePath(canonicalDir), err)
	}
	if !dirInfo.IsDir() {
		return nil, nil, fmt.Errorf("pr sessions dir %s is not a directory", termsafe.QuotePath(canonicalDir))
	}
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read pr sessions dir: %w", err)
	}
	// The pin READS through the same kind of handle the removal re-reads
	// through. Opening by path would follow a symlink swapped in between the
	// Lstat and the open, putting another file's bytes into the audit row —
	// and then the pinned re-read would be comparing against those.
	root, err := openDirRoot(c.sessionsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("pin pr sessions dir %s: %w", termsafe.QuotePath(c.sessionsDir), err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil {
			slog.Debug("Failed to close the pinned pr sessions dir handle.", "error", cerr)
		}
	}()

	var candidates []*asideCandidate
	for _, e := range entries {
		name := e.Name()
		// The sweep's whole blast radius: names carrying the set-aside marker.
		// An ordinary `.json` record, the lifecycle lock, and the audit log are
		// all outside it by construction.
		if e.IsDir() || !strings.Contains(name, unreadableSuffix) {
			continue
		}
		cand := &asideCandidate{
			name: name,
			path: filepath.Join(c.sessionsDir, name),
		}
		cand.item = PruneItem{Path: cand.path}
		candidates = append(candidates, cand)

		stamp, dated := parseAsideAge(name)
		if !dated {
			cand.item.Outcome = pruneOutcomeKept
			cand.item.Reason = undatedReason(name)
			continue
		}
		age := now.Sub(stamp)
		cand.item.Age = renderAge(age)
		if age < olderThan {
			cand.item.Outcome = pruneOutcomeKept
			cand.item.Reason = fmt.Sprintf("it was set aside %s ago, inside the %s window", renderAge(age), renderAge(olderThan))
			continue
		}
		if err := pinAsideCandidate(root, cand); err != nil {
			cand.item.Outcome = pruneOutcomeRefused
			cand.item.Error = termsafe.SafeLine(safeErrString(err))
			continue
		}
		cand.ref, cand.hasRef = refFromRawRecord(cand.bytes)
		cand.item.Ref = cand.refString()
	}
	return dirInfo, candidates, nil
}

// pinAsideCandidate captures the identity and the bytes the removal will be
// authorized against. A file that cannot be pinned is refused rather than
// removed: the audit row carries these bytes, and an unlink whose row names
// nothing is the one shape that loses the file outright.
// Both the stat and the read go through the PINNED directory handle rather
// than by path. By path, a symlink swapped in between the Lstat and the open
// would put an unrelated file's bytes into the audit row — and since the
// removal's byte comparison is against exactly these bytes, the comparison
// would then agree with itself about the wrong file.
func pinAsideCandidate(root *os.Root, cand *asideCandidate) error {
	info, err := root.Lstat(cand.name)
	if err != nil {
		return fmt.Errorf("stat set-aside record %s: %w", termsafe.QuotePath(cand.path), safeTermError(err))
	}
	if !staleMemberIsRegular(info) {
		return fmt.Errorf("set-aside record %s is not a regular file; refusing to remove it",
			termsafe.QuotePath(cand.path))
	}
	beforeAsideRead(cand.name)
	data, readInfo, err := readFileInRoot(root, cand.name)
	if err != nil {
		return err
	}
	if !os.SameFile(readInfo, info) {
		return fmt.Errorf("set-aside record %s changed identity before it could be read; refusing to remove it",
			termsafe.QuotePath(cand.path))
	}
	cand.info = info
	cand.bytes = data
	return nil
}

// screenLiveWindows asks tmux ONCE about every ref the candidates carry, and
// returns the ones still eligible for removal.
//
// Two refusals, and neither is allowed to widen past the file it is about. A
// LIVE window means that record still describes a running session — a newer
// build's session, most likely, since a set-aside record is by definition one
// this build could not read. An UNREADABLE window list is not an absent window,
// so it refuses every ref-BEARING file; a file carrying no ref names no window,
// so an unreadable list says nothing about it and it proceeds.
func (c *Client) screenLiveWindows(ctx context.Context, candidates []*asideCandidate) []*asideCandidate {
	var eligible []*asideCandidate
	var refs []Ref
	for _, cand := range candidates {
		if cand.item.Outcome != "" {
			continue
		}
		eligible = append(eligible, cand)
		if cand.hasRef {
			refs = append(refs, cand.ref)
		}
	}
	if len(refs) == 0 {
		return eligible
	}
	live, tmuxOK := c.WindowsLive(ctx, refs)
	var out []*asideCandidate
	for _, cand := range eligible {
		switch {
		case !cand.hasRef:
			out = append(out, cand)
		case !tmuxOK:
			cand.item.Outcome = pruneOutcomeRefused
			cand.item.Reason = "the tmux window list could not be read, and an unreadable list is not an absent window — " +
				"check `tmux list-windows -a`, then retry"
		case live[cand.ref]:
			cand.item.Outcome = pruneOutcomeRefused
			cand.item.Reason = fmt.Sprintf("it names %s, whose review window is still live — "+
				"the build that can read this record is the one that should settle it", cand.refString())
		default:
			out = append(out, cand)
		}
	}
	return out
}

// pruneOne writes the intent, unlinks, and completes the row.
//
// THE BYTES ARE LOAD-BEARING. Unlike every other repair arm, this one does not
// rename — it unlinks — so once it returns, the audit row is the only trace the
// file ever existed, and the row's own `record` field is the only field that
// can say what was in it.
func (c *Client) pruneOne(cand *asideCandidate, dirInfo fs.FileInfo) {
	row := RepairRow{
		Ref:         cand.refString(),
		RecordPath:  cand.path,
		FromPhase:   repairPhaseUnreadable,
		Verb:        auditVerbPrune,
		Mode:        RepairModePrune,
		Record:      cappedRecordBytes(cand.bytes),
		RecordBytes: len(cand.bytes),
	}
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		cand.item.Outcome = pruneOutcomeRefused
		cand.item.Error = termsafe.SafeLine(safeErrString(err))
		slog.Warn("Refusing to remove a set-aside session record: its audit row could not be written first.",
			"path", cand.path, "error", err)
		return
	}
	if rerr := c.removeAsideFile(cand, dirInfo); rerr != nil {
		cand.item.Outcome = pruneOutcomeFailed
		cand.item.Error = termsafe.SafeLine(safeErrString(rerr))
		c.completeRepairRow(rowID, row, rerr)
		slog.Error("Failed to remove a set-aside session record; it is still on disk.",
			"path", cand.path, "error", rerr)
		return
	}
	c.completeRepairRow(rowID, row, nil)
	cand.item.Outcome = pruneOutcomeRemoved
	slog.Info("Successfully removed a set-aside session record past its retention window.",
		"path", cand.path, "age", cand.item.Age, "bytes", len(cand.bytes))
}

// removeAsideFile runs the pinned-handle protocol setAsideUndecodableRecord
// uses, ending in an unlink rather than a rename. Its authority comes from
// dev+ino identity and byte equality, neither of which needs the file to be
// decodable — which is the point, since a set-aside file is by definition one
// nothing here can read.
func (c *Client) removeAsideFile(cand *asideCandidate, dirInfo fs.FileInfo) error {
	root, err := openDirRoot(c.sessionsDir)
	if err != nil {
		return fmt.Errorf("pin pr sessions dir %s: %w", termsafe.QuotePath(c.sessionsDir), err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil {
			slog.Debug("Failed to close the pinned pr sessions dir handle.", "error", cerr)
		}
	}()

	nowDir, err := root.Lstat(".")
	if err != nil {
		return fmt.Errorf("re-stat pr sessions dir: %w", err)
	}
	if !os.SameFile(nowDir, dirInfo) {
		return fmt.Errorf("pr sessions dir %s changed identity during prune; refusing to remove %s",
			termsafe.QuotePath(c.sessionsDir), termsafe.QuotePath(cand.path))
	}
	info, err := root.Lstat(cand.name)
	if err != nil {
		return fmt.Errorf("re-stat set-aside record %s: %w", termsafe.QuotePath(cand.path), safeTermError(err))
	}
	if !os.SameFile(info, cand.info) {
		return fmt.Errorf("set-aside record %s changed identity during prune; refusing to remove it",
			termsafe.QuotePath(cand.path))
	}
	if !staleMemberIsRegular(info) {
		return fmt.Errorf("set-aside record %s is no longer a regular file; refusing to remove it",
			termsafe.QuotePath(cand.path))
	}
	beforeAsideRead(cand.name)
	data, readInfo, err := readAsideBytes(root, cand.name)
	if err != nil {
		return err
	}
	if !os.SameFile(readInfo, cand.info) {
		return fmt.Errorf("set-aside record %s changed identity before its re-read; refusing to remove it",
			termsafe.QuotePath(cand.path))
	}
	if !bytes.Equal(data, cand.bytes) {
		return fmt.Errorf("set-aside record %s changed on disk during prune; refusing to remove it — "+
			"the audit row already written carries the bytes this sweep was authorized for",
			termsafe.QuotePath(cand.path))
	}
	if err := root.Remove(cand.name); err != nil {
		return fmt.Errorf("remove set-aside record %s: %w", termsafe.QuotePath(cand.path), safeTermError(err))
	}
	return nil
}

// readAsideBytes re-reads a set-aside file through the PINNED directory handle,
// for comparison against the bytes the audit row carries.
//
// It is a var because that comparison is the one guard with no other way to
// exercise it: the window it protects is between the identity checks and the
// read, so staging a real writer there would be a race a test cannot win.
var readAsideBytes = readFileInRoot

// beforeAsideRead runs between a set-aside file's root.Lstat and its read,
// in both pinAsideCandidate and removeAsideFile. It is a no-op in production;
// a test sets it to swap the entry in exactly the window the Lstat cannot
// cover (forgectl#791). Tests must restore it and must not run in parallel
// while overriding it.
var beforeAsideRead = func(string) {}

// readFileInRoot reads one bounded file through a pinned directory handle. It
// is the shared body behind the pin read and the re-read, so the two cannot
// drift into reading the file differently — which would make their byte
// comparison meaningless.
//
// It returns the descriptor's own Fstat alongside the bytes, so each caller
// can prove the file it read is the one its Lstat checked (os.SameFile), as
// the teardown re-reads do (forgectl#791).
func readFileInRoot(root *os.Root, name string) ([]byte, fs.FileInfo, error) {
	file, info, err := openRegularInRoot(root, name)
	if err != nil {
		return nil, nil, fmt.Errorf("read set-aside record %s: %w", termsafe.QuotePath(name), safeTermError(err))
	}
	data, readErr := readBreadcrumbBytes(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, nil, fmt.Errorf("read set-aside record %s: %w", termsafe.QuotePath(name), safeTermError(readErr))
	}
	if closeErr != nil {
		return nil, nil, fmt.Errorf("close set-aside record %s after reading: %w", termsafe.QuotePath(name), safeTermError(closeErr))
	}
	return data, info, nil
}

// compactRepairLog rewrites the audit log without its settled, expired rows.
//
// THE INTENT ROW GOES INTO THE LIVE LOG FIRST, before the new content is even
// assembled, and then the log is re-read so that row rides into the replacement.
// That ordering is what makes every crash point honest: die before the rename
// and the old log is intact with one dangling intent beside it, which reads as
// "a compaction started"; die after and the new log carries the same row, which
// reads as "a compaction happened". There is no window in which the log is
// shorter than it should be with nothing saying why.
//
// The re-read is also what keeps this sweep's OWN removal rows: they were
// appended after the first classification, they are in-window by construction,
// and recomputing over the file that is actually on disk is the only way to see
// them at all.
func (c *Client) compactRepairLog(cutoff time.Time, out *PruneLog) {
	plan, err := c.scanRepairLogFile(cutoff)
	if err != nil {
		out.Outcome = pruneOutcomeRefused
		out.Error = termsafe.SafeLine(safeErrString(err))
		return
	}
	if plan.dropped == 0 {
		out.Outcome = pruneOutcomeUnchanged
		out.Kept = plan.kept
		return
	}
	// The count excludes this row itself, which is appended next and kept by
	// every pass after it (an unpaired intent is never dropped).
	row := RepairRow{
		RecordPath: c.repairLogPath(),
		Verb:       auditVerbPrune,
		Mode:       RepairModePrune,
		Detail:     fmt.Sprintf("dropped %d rows, kept %d", plan.dropped, plan.kept),
	}
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		out.Outcome = pruneOutcomeRefused
		out.Error = termsafe.SafeLine(safeErrString(err))
		return
	}
	plan, err = c.rewriteRepairLog(cutoff)
	if err != nil {
		out.Outcome = pruneOutcomeFailed
		out.Error = termsafe.SafeLine(safeErrString(err))
		c.completeRepairRow(rowID, row, err)
		slog.Error("Failed to compact the repair audit log; the previous log is intact and nothing was dropped.",
			"path", c.repairLogPath(), "error", err)
		return
	}
	c.completeRepairRow(rowID, row, nil)
	out.Outcome = pruneOutcomeCompacted
	out.Dropped = plan.dropped
	out.Kept = plan.kept
	slog.Info("Successfully compacted the pr repair audit log.",
		"path", c.repairLogPath(), "dropped", plan.dropped, "kept", plan.kept)
}

// openRepairLog opens the log for a compaction pass. A missing log is not an
// error: it reads as empty, and f is nil.
func (c *Client) openRepairLog() (*os.File, error) {
	f, err := openRepairLogFile(c.repairLogPath(), os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read repair audit log: %w", safeTermError(err))
	}
	return f, nil
}

// scanRepairLogFile runs pass one over the log on disk. It is the RAW-BYTES
// counterpart to readRepairLogTail, which decodes and therefore skips what it
// cannot parse — the one thing a rewriter must never do.
//
// A line is NOT refused for its length, unlike in readRepairLogTail: an
// over-long line is counted as kept, so compaction carries it through byte for
// byte (forgectl#544). Refusing it would refuse every --prune from then on, and
// truncating it would be dropping by another name. It is not held either
// (forgectl#554): walkRepairLog streams it, so a log with no '\n' at all costs
// one buffer, not its size.
func (c *Client) scanRepairLogFile(cutoff time.Time) (*repairLogPlan, error) {
	f, err := c.openRepairLog()
	if err != nil {
		return nil, err
	}
	if f == nil {
		return scanRepairLog(bytes.NewReader(nil), cutoff)
	}
	defer func() { _ = f.Close() }()
	return scanRepairLog(f, cutoff)
}

// rewriteRepairLog runs both passes over ONE open descriptor — plan, rewind,
// copy — and returns the plan the replacement was written from.
//
// The caller holds the lifecycle lock, and every writer of the log appends
// under it, so the file cannot change between the passes; the one descriptor
// additionally pins both passes to the same inode. copyKept still checks, and a
// log that changed anyway refuses rather than dropping rows pass one never saw.
func (c *Client) rewriteRepairLog(cutoff time.Time) (*repairLogPlan, error) {
	f, err := c.openRepairLog()
	if err != nil {
		return nil, err
	}
	if f == nil {
		plan, perr := scanRepairLog(bytes.NewReader(nil), cutoff)
		if perr != nil {
			return nil, perr
		}
		return plan, c.writeRepairLogAtomic(plan, bytes.NewReader(nil))
	}
	defer func() { _ = f.Close() }()
	plan, err := scanRepairLog(f, cutoff)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind repair audit log for the copy: %w", err)
	}
	return plan, c.writeRepairLogAtomic(plan, f)
}

// writeRepairLogAtomic replaces the log with the lines plan keeps out of src,
// mirroring writeRecordAtomic's temp-write-sync-rename-dirsync shape. The
// kept lines stream from src into the temp file; nothing is assembled in
// memory. A failure at any point, the copy's changed-log refusal included,
// leaves the previous log exactly as it was, which is the only acceptable
// failure mode for the file that is sometimes the last pointer to a clean room.
func (c *Client) writeRepairLogAtomic(plan *repairLogPlan, src io.Reader) error {
	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("derive temp audit log name: %w", err)
	}
	tmp := filepath.Join(c.sessionsDir, "."+repairLogName+".tmp-"+suffix)
	f, err := c.fs.OpenExclusive(tmp)
	if err != nil {
		return fmt.Errorf("open temp audit log: %w", safeTermError(err))
	}
	discard := func() {
		if rerr := c.fs.Remove(tmp); rerr != nil {
			slog.Warn("Failed to remove a temp audit log after a failed compaction; it is never read, but it is left behind.",
				"path", tmp, "error", rerr)
		}
	}

	if err := plan.copyKept(src, f); err != nil {
		_ = f.Close()
		discard()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		discard()
		return fmt.Errorf("sync temp audit log: %w", err)
	}
	if err := f.Close(); err != nil {
		discard()
		return fmt.Errorf("close temp audit log: %w", err)
	}
	if err := c.fs.Rename(tmp, c.repairLogPath()); err != nil {
		discard()
		return fmt.Errorf("rename the compacted audit log into place: %w", safeTermError(err))
	}
	if err := c.fs.SyncDir(c.sessionsDir); err != nil {
		return fmt.Errorf("sync pr sessions dir after compacting the audit log "+
			"(the log is written; its durability could not be confirmed): %w", err)
	}
	return nil
}

// prunePrompt is what the confirmation gate shows. It names the UNLINK
// explicitly, because this is the one repair arm that does not rename, and a
// confirmation that does not say what is destroyed is not one.
func prunePrompt(files, dropped int, logPath string) string {
	return fmt.Sprintf("Remove set-aside session records and compact the repair audit log?\n"+
		"  records: %d file(s) past the retention window — these are UNLINKED, and the audit row is the only trace left\n"+
		"  log:     %s, dropping %d settled row(s)",
		files, termsafe.QuotePath(logPath), dropped)
}

// pruneItems flattens the candidate set into the report, in enumeration order.
func pruneItems(candidates []*asideCandidate) []PruneItem {
	items := make([]PruneItem, 0, len(candidates))
	for _, cand := range candidates {
		items = append(items, cand.item)
	}
	return items
}

// renderAge renders a duration the way an operator reasons about retention:
// whole days once there is one, hours below that.
func renderAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if days := int(d.Hours() / 24); days > 0 {
		return strconv.Itoa(days) + "d"
	}
	return strconv.Itoa(int(d.Hours())) + "h"
}
