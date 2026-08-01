package resume

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The snapshot store had no deleter at all: every session ever observed left a
// record behind forever. The cost is not disk (a record is ~1 KiB) — it is read
// amplification, because LoadAll runs on every resume AND on every Stop hook
// through the task-directory claims.
//
// What bounds it is TRANSCRIPT EXISTENCE, not age. A record is worth keeping
// exactly as long as `claude --resume <id>` can still open the session, and
// that is decided by Claude Code's own transcript retention (cleanupPeriodDays),
// which is operator-configurable and routinely raised well past the 30-day
// default. Pruning on a fixed age would delete snapshots for sessions that are
// still rescuable — destroying the one thing the feature exists to protect.
const (
	// storeKeepDays is a BACKSTOP, not the policy. It only catches the case
	// where transcript retention is disabled entirely and transcripts never
	// disappear, so the transcript signal never retires anything. It is set
	// far above any plausible retention window so it cannot fire on a
	// normally-configured machine.
	storeKeepDays = 180
	// pruneInterval throttles the whole pass. Prune runs from a Stop hook at
	// every turn end; unthrottled, its transcript resolution roughly doubles
	// the hook's cost at steady state for work that only matters once a day.
	pruneInterval = 24 * time.Hour
	// orphanGrace keeps the sweep from racing a concurrent Save. Several
	// sessions snapshot at the same turn boundary, and one of them may be
	// mid-write.
	orphanGrace = time.Hour
	// pruneMarker records when the last pass ran. It is dot-prefixed and not
	// a .json, so LoadAll steps over it.
	pruneMarker = ".last-prune"
)

// PruneResult reports what one prune pass did. Orphans are counted separately
// from Removed because they are a different failure being cleaned up: Removed
// are records that were valid and are now retired, Orphans are debris.
type PruneResult struct {
	Removed int
	Orphans int
	Errs    []error
}

// Prune bounds the snapshot store, given the store as Snapshot already loaded
// it and the set of ids this pass wrote.
//
// keep is not an optimization — it is a safety rule. An id written this pass is
// off the table no matter what the transcript signal says, which also covers a
// Save that FAILED: a record this pass could not rewrite is never one it may
// delete.
func Prune(p Paths, store map[string]*Record, keep map[string]bool, now time.Time) PruneResult {
	var res PruneResult
	if p.StoreDir == "" {
		return res
	}
	if fi, err := os.Stat(p.StoreDir); err != nil || !fi.IsDir() {
		return res
	}
	if prunedRecently(p.StoreDir, now) {
		return res
	}

	// Classify before deleting anything. The transcript half of the decision
	// is only trustworthy in aggregate, so nothing can be removed for a missing
	// transcript until the whole store has been looked at.
	idx := &projectIndex{}
	_, projectsErr := idx.list(p)
	hits := 0
	var missing, aged []string
	for id, r := range store {
		if keep[id] {
			continue
		}
		if projectsErr == nil {
			if hasTranscript(p, id, r.Cwd, idx) {
				hits++
			} else {
				missing = append(missing, id)
			}
		}
		// A ZERO LastSeen is unknown age, not infinite age. A record written by
		// a forgectl that predates the field — or one truncated before it was
		// written — unmarshals to the zero time, which is ~2000 years old and
		// would be deleted on the first pass no matter what its transcript
		// said. That is the age half's version of the mass-delete failure the
		// hits guard closes on the transcript half: a broken signal reading as
		// unanimous consent to delete. Unknown age defers to the transcript.
		if !r.LastSeen.IsZero() && now.Sub(r.LastSeen) > storeKeepDays*24*time.Hour {
			aged = append(aged, id)
		}
	}

	doomed := make(map[string]bool, len(missing)+len(aged))
	// The mass-delete guard. Zero transcripts resolved across the whole store
	// is the signature of a BROKEN LOOKUP — an unreadable ~/.claude/projects, a
	// Claude Code layout change, a relocated home — not of N dead sessions. A
	// prune that trusts it deletes the entire store in one pass. At least one
	// hit proves the lookup works, and only then does an absence mean absence.
	if hits > 0 {
		for _, id := range missing {
			doomed[id] = true
		}
	}
	// The age backstop is independent of all of that: it needs no transcript
	// signal, so it still applies when the transcript half bailed.
	for _, id := range aged {
		doomed[id] = true
	}
	for id := range doomed {
		if err := Delete(p.StoreDir, id); err != nil {
			res.Errs = append(res.Errs, err)
			continue
		}
		res.Removed++
	}

	orphans, orphanErrs := sweepOrphans(p.StoreDir, store, keep, now)
	res.Orphans = orphans
	res.Errs = append(res.Errs, orphanErrs...)

	if err := markPruned(p.StoreDir, now); err != nil {
		res.Errs = append(res.Errs, err)
	}
	return res
}

// sweepOrphans clears store-directory debris in one directory read: temp files
// left by a Save that died between CreateTemp and Rename, and .json files that
// no longer parse as a record (so LoadAll skips them and nothing else would
// ever reclaim them).
//
// Both leaks are silent by construction — neither file is visible to any reader
// of the store — so without this they accumulate for the life of the machine.
//
// A .json is proven garbage by trying to LOAD it, not by its absence from the
// store map. The map is a fast path only: it is a caller's argument, and
// deleting on the strength of it would mean a store loaded from the wrong
// directory — or simply gone stale — reads as "every record here is debris" and
// empties the store. Absence in the map only earns a file the reread that
// decides it.
func sweepOrphans(dir string, store map[string]*Record, keep map[string]bool, now time.Time) (int, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, []error{err}
	}
	swept := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".tmp"):
			// A temp file has no owner to look up: a Save that finished
			// renamed it away, so any survivor is debris by definition.
		case strings.HasSuffix(name, ".json"):
			id := strings.TrimSuffix(name, ".json")
			if keep[id] {
				continue
			}
			if _, known := store[id]; known {
				continue
			}
			if _, readable := Load(dir, id); readable {
				continue
			}
		default:
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= orphanGrace {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		swept++
	}
	return swept, errs
}

// prunedRecently reports whether a pass already ran inside the interval. A
// missing or unreadable marker reads as "never", so the first pass on a machine
// always runs.
func prunedRecently(dir string, now time.Time) bool {
	fi, err := os.Stat(filepath.Join(dir, pruneMarker))
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) < pruneInterval
}

// markPruned stamps the marker. The content is human-readable for debugging;
// the throttle reads the mtime.
func markPruned(dir string, now time.Time) error {
	return os.WriteFile(filepath.Join(dir, pruneMarker), []byte(now.Format(time.RFC3339)+"\n"), 0o600)
}

// projectIndex holds one read of ~/.claude/projects for a whole batch of
// transcript lookups.
//
// It exists because transcriptPath's fallback re-reads that directory on EVERY
// miss, and a prune pass is mostly misses by definition — the records it is
// deciding about are the ones whose transcripts are likely gone. On a store
// with dozens of dead records that is dozens of full directory reads per pass.
//
// A nil *projectIndex is valid and means "no caching": the fallback reads fresh
// each time, which is what a one-shot caller wants and what transcriptPath did
// before this existed.
type projectIndex struct {
	loaded bool
	dirs   []string
	err    error
}

func (i *projectIndex) list(p Paths) ([]string, error) {
	if i != nil && i.loaded {
		return i.dirs, i.err
	}
	var dirs []string
	entries, err := os.ReadDir(p.projectsDir())
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if i != nil {
		i.loaded, i.dirs, i.err = true, dirs, err
	}
	return dirs, err
}

// hasTranscript reports whether Claude Code still holds this session's
// transcript — the signal that decides whether `claude --resume <id>` can open
// it, and therefore whether the record is still worth anything.
//
// The id is re-validated here because Prune's ids come off disk and this builds
// a path from one; every other caller of transcriptPath got its id from a
// source that already checked.
func hasTranscript(p Paths, id, cwd string, idx *projectIndex) bool {
	return validSessionID(id) && transcriptPath(p, id, cwd, idx) != ""
}
