package resume

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Two directory dialects live under ~/.claude/tasks, and they are not old and
// new — they are per-session and per-team, both current as of Claude Code
// 2.1.220:
//
//   - DialectSession — tasks/<full-session-uuid>/. Keyed on the session id
//     itself. Verified live: a headless session wrote tasks/<its id>/1.json,
//     and `claude --resume <id>` kept the same id, reused that same directory,
//     saw the existing task through TaskList, and appended 2.json. This is the
//     dialect that makes task rescue work, and it is what Restore creates.
//   - DialectTeam — tasks/session-<8hex>/. Keyed on the LEAD SESSION id of an
//     agent team (~/.claude/teams/session-<8hex>/config.json), which survives
//     a /clear while the session id rotates and so is routinely NOT the id of
//     the session writing the tasks. Measured on this machine: only 139 of 360
//     such directories match any session-id prefix, whereas 199 of 199
//     uuid-form directories are exact session ids.
//
// Nothing on disk records the session → team-directory association durably
// (the transcript carries no team, agent, or task-directory reference at all),
// which is why forgectl's own store carries it — see store.go.
//
// debt: Restore targets DialectSession. If a future Claude Code stops keying
// per-session task directories on the session id, Restore would write where
// nothing reads and silently rescue nothing. DriftCheck is the tripwire: it
// compares the dialect of the most recently written task directory against
// what Restore would create, and `forgectl doctor` surfaces the divergence.
const (
	DialectSession = "session"
	DialectTeam    = "team"
	// DialectUnknown is a task-directory name matching neither known shape.
	// It exists so the drift check can FIRE on a third convention instead of
	// mistaking it for a dialect it recognizes.
	DialectUnknown = "unknown"
)

// Dialect classifies one ~/.claude/tasks directory name.
//
// The unknown case is load-bearing, not defensive padding. Classifying by
// "anything not prefixed session- is per-session" would make a future third
// naming convention read as the very dialect Restore writes — so DriftCheck
// would see a per-session directory that is not one, report no drift, and let
// task rescue fail exactly as silently as the tripwire exists to prevent. A
// per-session directory IS a session id, so it is checked by shape.
func Dialect(name string) string {
	switch {
	case strings.HasPrefix(name, "session-"):
		return DialectTeam
	case validSessionID(name):
		return DialectSession
	default:
		return DialectUnknown
	}
}

// TaskDirFor returns the per-session task directory for an id — the path
// Restore writes and a resumed session reads.
func TaskDirFor(p Paths, id string) string {
	if !validSessionID(id) {
		return ""
	}
	return filepath.Join(p.tasksDir(), id)
}

// ResolveTaskDir finds the directory holding a session's live tasks, newest
// evidence first:
//
//  1. the directory a previous snapshot already recorded, if it still exists —
//     once observed, the pairing is authoritative and does not need rediscovery;
//  2. tasks/<session-id>, the per-session dialect;
//  3. the task directory of an agent team whose membership names this cwd,
//     most recently modified first — the only available join for the team
//     dialect, and the reason Snapshot records what it finds.
//
// An empty return means the session has no tasks on disk right now, which is
// the normal state for one that has already exited.
func ResolveTaskDir(p Paths, id, cwd, prior string) string {
	if prior != "" && isDir(prior) {
		return prior
	}
	if d := TaskDirFor(p, id); d != "" && isDir(d) {
		return d
	}
	return teamTaskDirForCwd(p, cwd, claimedTaskDirs(p, id))
}

// claimedTaskDirs returns the task directories the store has already paired
// with some OTHER session.
//
// Two sessions working in one checkout can each have a team, and cwd cannot
// tell their task directories apart — so without this, the newest-first
// heuristic could hand session A the directory already known to belong to
// session B. That is worse than finding nothing: A's snapshot would absorb B's
// tasks, and resuming A would inject them into A's own task list. An already
// claimed directory is therefore off the table for everyone else.
func claimedTaskDirs(p Paths, self string) map[string]bool {
	claimed := map[string]bool{}
	for id, rec := range LoadAll(p.StoreDir) {
		if id != self && rec.TaskDir != "" {
			claimed[rec.TaskDir] = true
		}
	}
	return claimed
}

// teamConfig is the subset of ~/.claude/teams/<name>/config.json we read.
type teamConfig struct {
	Name          string `json:"name"`
	LeadSessionID string `json:"leadSessionId"`
	Members       []struct {
		Cwd string `json:"cwd"`
	} `json:"members"`
}

// teamTaskDirForCwd finds the most recently written team task directory whose
// team has a member working in cwd, ignoring any directory already claimed by
// another session.
//
// This is a heuristic and is deliberately the LAST resort: two sessions in one
// checkout can both have teams, and cwd cannot tell them apart. It only ever
// runs for a live session during Snapshot, where "most recently written" is a
// strong tiebreak — and whatever it picks is written to the store, so the
// guess is made once and then read back as fact. That last property is why the
// claimed set matters: a wrong guess would otherwise harden into a wrong fact.
func teamTaskDirForCwd(p Paths, cwd string, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	entries, err := os.ReadDir(p.teamsDir())
	if err != nil {
		return ""
	}
	best, bestMod := "", int64(0)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.teamsDir(), e.Name(), "config.json")) // #nosec G304 -- a config.json under the caller's own ~/.claude/teams
		if err != nil {
			continue
		}
		var tc teamConfig
		if json.Unmarshal(data, &tc) != nil {
			continue
		}
		matched := false
		for _, m := range tc.Members {
			if m.Cwd == cwd {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		dir := filepath.Join(p.tasksDir(), e.Name())
		if claimed[dir] {
			continue
		}
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		if mod := fi.ModTime().UnixNano(); mod > bestMod {
			best, bestMod = dir, mod
		}
	}
	return best
}

// ReadTaskDir reads every task body in a directory, ordered by task id. A file
// that will not parse is skipped — one malformed body must not cost the rest.
func ReadTaskDir(dir string) []Task {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- a .json inside the task dir the caller resolved
		if err != nil {
			continue
		}
		var t Task
		if json.Unmarshal(data, &t) != nil || t.ID == "" {
			continue
		}
		t.Raw = append(json.RawMessage(nil), data...)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return taskIDLess(out[i].ID, out[j].ID) })
	return out
}

// RestoreResult reports what a write-back actually did, so the caller can say
// so plainly instead of claiming a rescue it did not perform.
type RestoreResult struct {
	Dir       string
	Written   int
	Skipped   int // already present in the live directory — never overwritten
	Watermark int
}

// Restore writes snapshotted task bodies back into the directory the resumed
// session will read.
//
// Two rules make it safe to run before every resume:
//
//   - A file already in the live directory ALWAYS wins. Restore only creates
//     what is missing, so re-running it is a no-op and it can never clobber a
//     task the session itself owns.
//   - .highwatermark is raised, never lowered. Claude Code allocates the next
//     task id from it, so a watermark below a restored id would hand the
//     session an id that is already on disk.
func Restore(dir string, tasks []Task) (RestoreResult, error) {
	res := RestoreResult{Dir: dir}
	if dir == "" {
		return res, fmt.Errorf("no task directory to restore into")
	}
	if len(tasks) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, fmt.Errorf("create task directory %s: %w", dir, err)
	}
	for _, t := range tasks {
		if !validTaskID(t.ID) {
			continue
		}
		path := filepath.Join(dir, t.ID+".json")
		if _, err := os.Stat(path); err == nil {
			res.Skipped++
			continue
		}
		body := t.Raw
		if len(body) == 0 {
			encoded, err := json.MarshalIndent(t, "", "  ")
			if err != nil {
				continue
			}
			body = encoded
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return res, fmt.Errorf("restore task %s: %w", t.ID, err)
		}
		res.Written++
	}
	watermark, err := raiseWatermark(dir, tasks)
	if err != nil {
		return res, err
	}
	res.Watermark = watermark
	return res, nil
}

// raiseWatermark sets .highwatermark to the highest task id present, unless the
// file already names a higher one.
func raiseWatermark(dir string, tasks []Task) (int, error) {
	high := 0
	for _, t := range tasks {
		if n, err := strconv.Atoi(t.ID); err == nil && n > high {
			high = n
		}
	}
	path := filepath.Join(dir, ".highwatermark")
	if data, err := os.ReadFile(path); err == nil { // #nosec G304 -- a fixed name inside the resolved task dir
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n >= high {
			return n, nil
		}
	}
	if high == 0 {
		return 0, nil
	}
	// No trailing newline: Claude Code writes the bare decimal.
	if err := os.WriteFile(path, []byte(strconv.Itoa(high)), 0o600); err != nil {
		return 0, fmt.Errorf("write task watermark in %s: %w", dir, err)
	}
	return high, nil
}

// Drift is what DriftCheck found: the dialect Restore writes versus the one
// Claude Code most recently used.
type Drift struct {
	Restores string // the dialect Restore creates
	Newest   string // the dialect of the most recently written task directory
	Dir      string // that directory's name, for the report
	Checked  bool   // false when there were no task directories to compare
}

// Drifted reports whether the two disagree — the condition that would make
// Restore write somewhere nothing reads.
func (d Drift) Drifted() bool { return d.Checked && d.Restores != d.Newest }

// DriftCheck compares what Restore creates against the dialect of the most
// recently written task directory. It is the tripwire for the version coupling
// documented at the top of this file, and is surfaced by `forgectl doctor`.
//
// A team-dialect newest directory is NOT drift on its own — team and
// per-session directories legitimately coexist, so the check looks for the
// per-session dialect having disappeared entirely from recent writes.
func DriftCheck(p Paths) Drift {
	d := Drift{Restores: DialectSession}
	entries, err := os.ReadDir(p.tasksDir())
	if err != nil {
		return d
	}
	newest, newestMod := "", int64(0)
	sawSession := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if Dialect(e.Name()) == DialectSession {
			sawSession = true
		}
		if mod := fi.ModTime().UnixNano(); mod > newestMod {
			newest, newestMod = e.Name(), mod
		}
	}
	if newest == "" {
		return d
	}
	d.Checked = true
	d.Dir = newest
	// Any per-session directory on disk means the dialect Restore writes is
	// still one Claude Code reads; only its total absence is drift.
	if sawSession {
		d.Newest = DialectSession
	} else {
		d.Newest = Dialect(newest)
	}
	return d
}

// validTaskID guards the path a restored body is written to. Task ids are
// small decimals ("1", "2", …) and name a file directly.
func validTaskID(id string) bool {
	if id == "" || len(id) > 10 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// taskIDLess orders task ids numerically, falling back to string order for any
// id that is not a plain decimal.
func taskIDLess(a, b string) bool {
	na, erra := strconv.Atoi(a)
	nb, errb := strconv.Atoi(b)
	if erra == nil && errb == nil {
		return na < nb
	}
	return a < b
}

// sortTaskIDs orders a slice of task ids in place, numerically where possible.
func sortTaskIDs(ids []string) {
	sort.Slice(ids, func(i, j int) bool { return taskIDLess(ids[i], ids[j]) })
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
