package resume

import (
	"errors"
	"fmt"
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
	// storeKeepDays is a BACKSTOP, and it never overrides a transcript that
	// resolved. A record whose transcript is still there is still rescuable
	// however old it is — LastSeen ages from last USE, not from last
	// resumability, so aging one out would delete the task bodies of a session
	// `claude --resume` can still open. What the ceiling actually does is
	// release records whose transcript is GONE when the hits guard below is
	// refusing to act on that absence: a machine whose lookup stays broken
	// still retires its dead records, just slowly.
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
	// noPruneEnv is the operator's kill switch, read by DefaultPaths. Any
	// non-empty value disables pruning.
	noPruneEnv = "FORGECTL_RESUME_NO_PRUNE"
	// pruneMaxShare and pruneMinBatch bound how much ONE pass may delete: at
	// most a 1/pruneMaxShare share of the population it is judging, with a
	// floor of pruneMinBatch so a small store is not frozen by rounding.
	//
	// This is the control for the whole class of PARTIAL lookup failures, which
	// the hits guard cannot see. That guard only catches a TOTAL failure — one
	// transcript resolving anywhere authorizes deleting every record that did
	// not. But most realistic breakage is partial: a project directory that is
	// a symlink, one directory returning EACCES or EIO while its neighbours
	// answer, an unmounted volume, or a gradual Claude Code layout migration
	// where old-layout sessions supply the hits and every new-layout session
	// reads as dead. In all of them a healthy-looking minority vouches for a
	// catastrophic majority. A cap needs no theory about which channel broke:
	// retiring most of the store at once is not something a working prune ever
	// does, so the pass refuses and says why.
	pruneMaxShare = 4
	pruneMinBatch = 5
)

// pruneCap is the most one pass may delete out of a population of n.
func pruneCap(n int) int {
	if c := n / pruneMaxShare; c > pruneMinBatch {
		return c
	}
	return pruneMinBatch
}

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
	if p.StoreDir == "" || p.NoPrune {
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
		if projectsErr != nil {
			// The transcript signal is unavailable, so no absence here means
			// anything. Nothing in this store is decidable on this pass.
			continue
		}
		if hasTranscript(p, id, r.Cwd, idx) {
			hits++
			continue
		}
		missing = append(missing, id)
		// The age ceiling applies ONLY to a record already proven transcript-less.
		// It is a release valve for the hits guard, never an independent reason
		// to delete — a resolving transcript outranks any age.
		//
		// A ZERO LastSeen is unknown age, not infinite age. A record written by
		// a forgectl that predates the field — or one truncated before it was
		// written — unmarshals to the zero time, which is ~2000 years old and
		// would age out on the first pass it was seen.
		if !r.LastSeen.IsZero() && now.Sub(r.LastSeen) > storeKeepDays*24*time.Hour {
			aged = append(aged, id)
		}
	}

	// The mass-delete guard. Zero transcripts resolved across the whole store
	// is the signature of a BROKEN LOOKUP — an unreadable ~/.claude/projects, a
	// Claude Code layout change, a relocated home — not of N dead sessions. A
	// prune that trusts it deletes the entire store in one pass. At least one
	// hit proves the lookup works, and only then does an absence mean absence.
	// When it does not, the age ceiling is all that still retires anything.
	doomed := missing
	if hits == 0 {
		doomed = aged
	}
	if limit := pruneCap(len(store)); len(doomed) > limit {
		res.Errs = append(res.Errs, fmt.Errorf(
			"resume store prune REFUSED and deleted nothing: %d of %d records classified stale, over the %d-per-pass cap. "+
				"Retiring most of a store at once is the signature of a broken transcript lookup, not of that many dead sessions. "+
				"Check ~/.claude/projects; set %s=1 to disable pruning",
			len(doomed), len(store), limit, noPruneEnv))
		doomed = nil
	}
	for _, id := range doomed {
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
// A .json is proven garbage by READING it and failing to PARSE it — never by
// its absence from the store map, and never by a failure to read it at all. The
// map is a caller's argument, so deleting on its say-so means a store loaded
// from the wrong directory, or simply gone stale, reads as "every record here
// is debris". And an unreadable file is an environment problem that clears on
// its own, not a corrupt one; only loadCorrupt earns a deletion.
//
// The .json half is capped for the same reason Prune's is: misclassification
// there destroys live records. The .tmp half is deliberately uncapped, because
// no valid record is ever a .tmp — Save's only path to a live record is a
// rename ONTO a .json — so there is nothing a .tmp classification can get
// wrong, and capping it would let a crash loop wedge the sweep permanently.
func sweepOrphans(dir string, store map[string]*Record, keep map[string]bool, now time.Time) (int, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, []error{err}
	}
	var tmpDebris, jsonDebris []string
	jsonTotal, jsonClassified := 0, 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		isJSON := strings.HasSuffix(name, ".json")
		switch {
		case strings.HasSuffix(name, ".tmp"):
		case isJSON:
			jsonTotal++
			id := strings.TrimSuffix(name, ".json")
			if keep[id] {
				continue
			}
			if _, known := store[id]; known {
				continue
			}
			if _, outcome := loadRecord(dir, id); outcome != loadCorrupt {
				continue
			}
			// Counted at CLASSIFICATION, before the grace filter: the cap is
			// about how much of this directory was judged debris, not about how
			// much of it happens to be old enough to act on yet.
			jsonClassified++
		default:
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= orphanGrace {
			continue
		}
		if isJSON {
			jsonDebris = append(jsonDebris, name)
		} else {
			tmpDebris = append(tmpDebris, name)
		}
	}

	var errs []error
	if limit := pruneCap(jsonTotal); jsonClassified > limit {
		errs = append(errs, fmt.Errorf(
			"resume store orphan sweep REFUSED to delete %d unparseable record file(s) of %d, over the %d-per-pass cap. "+
				"A directory that is mostly unreadable is a storage problem, not that much debris; nothing was deleted. "+
				"Set %s=1 to disable pruning", jsonClassified, jsonTotal, limit, noPruneEnv))
		jsonDebris = nil
	}

	swept := 0
	for _, name := range append(tmpDebris, jsonDebris...) {
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
//
// A marker dated in the FUTURE — clock skew, a restore from backup, a file
// copied off another machine — also reads as never. Treating a negative age as
// "recent" would park pruning until real time caught up with the stamp.
func prunedRecently(dir string, now time.Time) bool {
	fi, err := os.Stat(filepath.Join(dir, pruneMarker))
	if err != nil {
		return false
	}
	age := now.Sub(fi.ModTime())
	return age >= 0 && age < pruneInterval
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
			continue
		}
		// A SYMLINK to a project directory reports IsDir() == false here:
		// os.ReadDir fills DirEntry.Type() from the directory entry itself, not
		// from the link target. Skipping it would drop every transcript inside
		// it from the index — and those sessions would read as transcript-less,
		// which is a deletion verdict. Only symlinks pay the extra stat.
		if e.Type()&fs.ModeSymlink != 0 && isDir(filepath.Join(p.projectsDir(), e.Name())) {
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
