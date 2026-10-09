package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface prune` removes old closed, failed, expired and reported queue rows
// and old closed ledger rows (atelier P4, T10.3), adding the removed rows' cost to
// usage-daily.jsonl first, one line per row. The drain runs it once a UTC
// day.

// pruneTimeout bounds one prune: herdr probes and the state files.
const pruneTimeout = 2 * time.Minute

// pruneResult is what `surface prune --json` prints. Additive changes only
// (ADR-0008 rule 2).
type pruneResult struct {
	DryRun    bool      `json:"dry_run"`
	OlderThan string    `json:"older_than"`
	Cutoff    time.Time `json:"cutoff"`
	// Removed is what was removed, or with --dry-run what would be.
	Removed []drain.PruneItem `json:"removed"`
	Kept    []drain.PruneItem `json:"kept"`
	// Usage is the lines appended to usage-daily.jsonl, or that would be.
	Usage []drain.UsageLine `json:"usage"`
	Notes []string          `json:"notes"`
}

// pruneQueue is the slice of *worker.Queue prune uses.
type pruneQueue interface {
	Rows() ([]worker.QueueRow, error)
	PruneIf(remove func(worker.QueueRow) bool, before func([]worker.QueueRow) error) ([]worker.QueueRow, error)
}

// pruneLedger is the slice of *worker.Ledger prune uses.
type pruneLedger interface {
	Rows() ([]worker.Row, error)
	RemoveIf(name string, match func(worker.Row) bool) error
}

// pruneDeps are prune's seams.
type pruneDeps struct {
	queue   pruneQueue
	ledgers func() ([]worker.LedgerID, []string, error)
	ledger  func(worker.LedgerID) (pruneLedger, error)
	// workspace returns herdr's view of ledger rows' workspaces, or an
	// error when herdr cannot be read.
	workspace   func(ctx context.Context) (func(worker.LedgerID, worker.Row) drain.Workspace, error)
	appendUsage func([]byte) error
	// cache is the status cache; nil prunes none.
	cache pruneCache
	// worktreeGone reports whether a closed ledger row's recorded worktree
	// path no longer exists (drain.PruneInput.WorktreeGone).
	worktreeGone func(path string) (bool, error)
}

// pruneCache is the slice of worker.StatusCache prune uses.
type pruneCache interface {
	Entries() ([]worker.StatusCacheEntry, error)
	Remove(worker.StatusCacheEntry) error
}

func newSurfacePruneCmd(_ module.Deps) *cobra.Command {
	var (
		olderThan string
		dryRun    bool
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove old closed, failed, expired and reported queue rows, closed ledger rows and status cache entries",
		Long: `prune removes queue rows in state closed, failed, expired or reported whose
state_at is older than --older-than (default 30d), ledger rows at stage
closed whose closed_at is older than it, from every ledger in the state
directory, and surface status cache entries last written before the cutoff.
A reported row normally waits for the drain's closers or surface close; one
neither settled for the whole cutoff goes like the others. A closed ledger
row from before closed_at existed goes only when its started_at is older
than the cutoff and its recorded worktree path no longer exists.

A queue row whose own ledger row (same name and launch id) still has a
herdr workspace that herdr says is live, or cannot rule out, is kept, and so
is every launched queue row whose ledger is not readable while any ledger
file cannot be read at all. When herdr cannot be read (no herdr session, or
the server is down), no ledger row is removed, and prune says so.

Before a queue row with a cost_usd is removed, its cost is added to
usage-daily.jsonl in the state directory: one line per removed row,
{"day","name","launch_id","costUsd"}, day being the UTC day of its state_at.
The lines are written under the queue lock before the rows go, and nothing
is removed when they cannot be written. A prune that fails after writing can
write a row's line again on the next run, so read the file by summing the
unique (name, launch_id) lines per day. The file is append-only and never
pruned; it is the long-term cost record.

--dry-run reads everything and writes nothing. --json prints {"dry_run",
"older_than","cutoff","removed":[...],"kept":[...],"usage":[{"day","name",
"launch_id","costUsd"}],"notes"}, each row {"kind","name","repo","session","state","at",
"cost_usd","reason"}; kind is queue, ledger or status-cache (named by head), and with --dry-run removed is
what would be removed. The drain runs prune with the default cutoff once
each UTC day.

Exit 0: pruned (or previewed). Exit 1: the queue or the ledger directory
could not be read or written. Exit 2: a usage error, such as --older-than
under 1h.

  forgectl surface prune --dry-run
  forgectl surface prune --older-than 14d --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			age, err := pr.ParseRetention(olderThan)
			if err != nil {
				return WithExitCode(fmt.Errorf("--older-than: %w", err), exitUsage)
			}
			d, err := realPruneDeps()
			if err != nil {
				return WithExitCode(termsafe.Error(err), exitUsage)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), pruneTimeout)
			defer cancel()
			res, err := runPrune(ctx, d, time.Now(), age, dryRun)
			if err != nil {
				return WithExitCode(termsafe.Error(err), exitFailed)
			}
			res.OlderThan = olderThan
			return reportPrune(cmd.OutOrStdout(), res, asJSON)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "30d", "remove rows older than this (30d, 720h; at least 1h)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be removed and write nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"dry_run","older_than","cutoff","removed","kept","usage","notes"} as JSON`)
	return cmd
}

// realPruneDeps wires prune to the machine's queue, ledgers and herdr.
func realPruneDeps() (pruneDeps, error) {
	q, err := worker.OpenQueue()
	if err != nil {
		return pruneDeps{}, err
	}
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return pruneDeps{}, err
	}
	cache, err := worker.OpenStatusCache()
	if err != nil {
		return pruneDeps{}, err
	}
	return pruneDeps{
		queue:   q,
		ledgers: worker.ListLedgers,
		ledger: func(id worker.LedgerID) (pruneLedger, error) {
			return worker.Open(id.Repo, id.Session)
		},
		workspace:    herdrWorkspaces,
		appendUsage:  files.AppendUsage,
		cache:        cache,
		worktreeGone: pathGone,
	}, nil
}

// pathGone reports whether path no longer exists, without following a
// final symlink. Any error other than "does not exist" means it cannot say.
func pathGone(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	}
	return false, err
}

// herdrWorkspaces probes ledger rows' workspaces through the herdr session
// this process resolves. A row from another session cannot be seen from
// here, so herdr cannot say.
func herdrWorkspaces(ctx context.Context) (func(worker.LedgerID, worker.Row) drain.Workspace, error) {
	adapter, err := newHerdrAdapter(io.Discard)
	if err != nil {
		return nil, err
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return nil, errors.New("the herdr adapter has an unexpected type")
	}
	if err := herdr.CheckReady(ctx); err != nil {
		return nil, err
	}
	return func(id worker.LedgerID, r worker.Row) drain.Workspace {
		if id.Session != herdr.Session() {
			return drain.WorkspaceUnknown
		}
		ref, err := backend.DecodeRef(r.Ref)
		if err != nil {
			return drain.WorkspaceUnknown
		}
		switch herdr.Probe(ctx, ref).State() {
		case backend.ProbePresent:
			return drain.WorkspaceLive
		case backend.ProbeGone:
			return drain.WorkspaceGone
		}
		return drain.WorkspaceUnknown
	}, nil
}

// runPrune reads the queue and every ledger, decides with drain.PlanPrune,
// and, unless dryRun, removes the queue rows (appending their cost to
// usage-daily.jsonl under the queue lock first) and then the ledger rows,
// each only if it is still the row read. An error means the queue or the
// ledger directory could not be read, or the queue could not be written.
func runPrune(ctx context.Context, d pruneDeps, now time.Time, olderThan time.Duration, dryRun bool) (pruneResult, error) {
	res := pruneResult{DryRun: dryRun, Cutoff: now.Add(-olderThan).UTC(), Removed: []drain.PruneItem{}, Kept: []drain.PruneItem{}, Usage: []drain.UsageLine{}, Notes: []string{}}
	rows, err := d.queue.Rows()
	if err != nil {
		return res, fmt.Errorf("read the queue: %w", err)
	}
	ids, bad, err := d.ledgers()
	if err != nil {
		return res, fmt.Errorf("list the ledgers: %w", err)
	}
	for _, name := range bad {
		res.Notes = append(res.Notes, "skipped a ledger file that could not be read: "+name)
	}
	in := drain.PruneInput{Now: now, OlderThan: olderThan, Queue: rows, BadLedgers: bad, WorktreeGone: d.worktreeGone}
	stores := map[worker.LedgerID]pruneLedger{}
	for _, id := range ids {
		pl := drain.PruneLedger{ID: id}
		led, err := d.ledger(id)
		if err == nil {
			pl.Rows, err = led.Rows()
		}
		if err != nil {
			pl.Err = termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
		} else {
			stores[id] = led
		}
		in.Ledgers = append(in.Ledgers, pl)
	}
	if d.cache != nil {
		if in.Cache, err = d.cache.Entries(); err != nil {
			res.Notes = append(res.Notes, "the status cache could not be listed: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
		}
	}
	if ws, err := d.workspace(ctx); err != nil {
		in.HerdrErr = termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
	} else {
		in.Workspace = ws
	}
	plan := drain.PlanPrune(in)
	res.Kept = append(res.Kept, plan.Keep...)
	res.Notes = append(res.Notes, plan.Notes...)

	var queueItems, ledgerItems, cacheItems []drain.PruneItem
	for _, it := range plan.Remove {
		switch it.Kind {
		case drain.PruneKindQueue:
			queueItems = append(queueItems, it)
		case drain.PruneKindLedger:
			ledgerItems = append(ledgerItems, it)
		default:
			cacheItems = append(cacheItems, it)
		}
	}
	if dryRun {
		res.Removed = append(res.Removed, plan.Remove...)
		res.Usage = append(res.Usage, drain.UsageLines(pruneItemRows(queueItems))...)
		return res, nil
	}

	removed, err := pruneQueueRows(d, queueItems)
	if err != nil {
		return res, err
	}
	res.Usage = append(res.Usage, drain.UsageLines(removed)...)
	gone := map[string]bool{}
	for _, r := range removed {
		gone[r.Name] = true
	}
	for _, it := range queueItems {
		if gone[it.Name] {
			res.Removed = append(res.Removed, it)
			continue
		}
		it.Reason = "the row changed since prune read it"
		res.Kept = append(res.Kept, it)
	}
	for _, it := range ledgerItems {
		id, row := it.LedgerRow()
		err := stores[id].RemoveIf(row.Name, worker.SameRow(row))
		switch {
		case err == nil:
			res.Removed = append(res.Removed, it)
		case errors.Is(err, worker.ErrRowChanged), errors.Is(err, worker.ErrNoRow):
			it.Reason = "the row changed since prune read it"
			res.Kept = append(res.Kept, it)
		default:
			it.Reason = "the ledger could not be written: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
			res.Kept = append(res.Kept, it)
		}
	}
	for _, it := range cacheItems {
		if err := d.cache.Remove(it.CacheEntry()); err != nil {
			it.Reason = "not removed: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
			res.Kept = append(res.Kept, it)
			continue
		}
		res.Removed = append(res.Removed, it)
	}
	return res, nil
}

// pruneQueueRows removes the planned queue rows that are still as read, in
// one write under the queue lock, appending the cost of those it removes to
// usage-daily.jsonl first.
func pruneQueueRows(d pruneDeps, items []drain.PruneItem) ([]worker.QueueRow, error) {
	if len(items) == 0 {
		return nil, nil
	}
	planned := make(map[string]worker.QueueRow, len(items))
	for _, it := range items {
		planned[it.Name] = it.QueueRow()
	}
	removed, err := d.queue.PruneIf(func(r worker.QueueRow) bool {
		was, ok := planned[r.Name]
		return ok && worker.SameRead(was)(r)
	}, func(rows []worker.QueueRow) error {
		lines, err := usageLines(drain.UsageLines(rows))
		if err != nil || len(lines) == 0 {
			return err
		}
		return d.appendUsage(lines)
	})
	if err != nil {
		return nil, fmt.Errorf("remove queue rows (nothing was removed): %w", err)
	}
	return removed, nil
}

func pruneItemRows(items []drain.PruneItem) []worker.QueueRow {
	out := make([]worker.QueueRow, 0, len(items))
	for _, it := range items {
		out = append(out, it.QueueRow())
	}
	return out
}

// usageLines renders usage-daily.jsonl lines.
func usageLines(lines []drain.UsageLine) ([]byte, error) {
	var b []byte
	for _, u := range lines {
		// termsafe:allow-raw-json private 0600 usage file of numbers and a date, read back as data
		line, err := json.Marshal(u)
		if err != nil {
			return nil, err
		}
		b = append(append(b, line...), '\n')
	}
	return b, nil
}

func reportPrune(out io.Writer, r pruneResult, asJSON bool) error {
	if asJSON {
		return writeJSON(out, r)
	}
	verb := "removed"
	if r.DryRun {
		verb = "would remove"
	}
	var b strings.Builder
	count := map[string]int{}
	for _, it := range r.Removed {
		count[it.Kind]++
	}
	fmt.Fprintf(&b, "%s %d queue rows, %d ledger rows and %d status cache entries older than %s\n", verb,
		count[drain.PruneKindQueue], count[drain.PruneKindLedger], count[drain.PruneKindCache], r.Cutoff.Format(time.RFC3339))
	for _, it := range r.Removed {
		fmt.Fprintf(&b, "  %s %s %s (%s)\n", it.Kind, termsafe.SafeLineMax(it.Name, 64), termsafe.SafeLineMax(it.State, 16), safeColumnPath(it.Repo))
	}
	for _, u := range r.Usage {
		fmt.Fprintf(&b, "usage %s %s: $%.2f\n", u.Day, termsafe.SafeLineMax(u.Name, 64), u.CostUSD)
	}
	for _, it := range r.Kept {
		if it.Kind == drain.PruneKindQueue && (it.State == string(worker.QueueClosed) || it.State == string(worker.QueueFailed) || it.State == string(worker.QueueExpired)) {
			fmt.Fprintf(&b, "  kept %s: %s\n", termsafe.SafeLineMax(it.Name, 64), termsafe.SafeLineMax(it.Reason, 300))
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", termsafe.SafeLineMax(n, 300))
	}
	_, err := io.WriteString(out, b.String())
	return err
}
