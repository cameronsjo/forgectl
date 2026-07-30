package resume

import (
	"time"
)

// SnapshotResult reports what one capture pass did, so `resume snapshot` can
// say it plainly and the tests can assert on it.
type SnapshotResult struct {
	Sessions int // live sessions written to the store
	Tasks    int // task bodies held after the merge, across all of them
	Learned  int // session → task-directory pairings recorded for the first time
	Errs     []error
}

// Snapshot captures what a live session owns and Claude Code will discard.
//
// It runs from a Stop hook at every turn end, so it is built to be cheap and
// idempotent: a handful of registry files, one task directory per live
// session, and an atomic per-session write. Nothing here can fail a turn — the
// caller reports errors and still exits 0.
//
// What it captures is exactly what does not survive on its own:
//
//   - the /rename name, which lives only in the live-process registry;
//   - the task bodies, which Claude Code deletes on exit;
//   - the session → task-directory pairing, which nothing on disk records.
//
// Identity is deliberately NOT snapshotted — history.jsonl and the transcripts
// already hold the id, cwd, branch and title indefinitely, and duplicating
// durable data is how a cache starts lying.
func Snapshot(p Paths, now time.Time) SnapshotResult {
	var res SnapshotResult
	// Read the store's directory claims ONCE. This used to happen inside
	// ResolveTaskDir, i.e. once per live session per turn, with every record
	// carrying full task bodies.
	claimed := ClaimedTaskDirs(p)
	for _, e := range LiveEntries(p) {
		prior, hadPrior := Load(p.StoreDir, e.SessionID)

		rec := &Record{ID: e.SessionID, Cwd: e.Cwd, Version: e.Version, LastSeen: now}
		if hadPrior {
			rec.Branch = prior.Branch
			rec.TaskDir = prior.TaskDir
		}
		if e.Name != "" {
			rec.Name, rec.NameSource = e.Name, NameFromRename
		} else if hadPrior {
			rec.Name, rec.NameSource = prior.Name, prior.NameSource
		}

		priorDir := ""
		if hadPrior {
			priorDir = prior.TaskDir
		}
		dir := ResolveTaskDir(p, e.SessionID, e.Cwd, priorDir, claimed)
		if dir != "" {
			rec.TaskDir = dir
			if priorDir != dir {
				res.Learned++
				// Claim it for the rest of this pass, so two live
				// sessions in one checkout cannot both resolve to it.
				claimed[dir] = e.SessionID
			}
		}

		var priorTasks []Task
		if hadPrior {
			priorTasks = prior.Tasks
		}
		rec.Tasks = mergeTasks(priorTasks, ReadTaskDir(dir))

		if err := Save(p.StoreDir, rec); err != nil {
			res.Errs = append(res.Errs, err)
			continue
		}
		res.Sessions++
		res.Tasks += len(rec.Tasks)
	}
	return res
}

// RestoreFor puts a session's snapshotted tasks back where the resumed session
// will read them, and reports what it did.
//
// The target is the per-session directory (tasks/<id>), not necessarily the
// one the tasks were captured from: a resumed session keeps its session id, so
// that is the directory it reads. A snapshot taken from a team directory is
// therefore replayed INTO the per-session directory, which is what makes rescue
// work across the team dialect at all.
func RestoreFor(p Paths, id string) (RestoreResult, error) {
	rec, ok := Load(p.StoreDir, id)
	if !ok || len(rec.Tasks) == 0 {
		return RestoreResult{}, nil
	}
	return Restore(TaskDirFor(p, id), rec.Tasks)
}
