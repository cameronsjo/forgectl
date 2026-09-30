package resume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RegistryEntry is one ~/.claude/sessions/<pid>.json file: Claude Code's
// live-process registry. It is the ONLY source for the /rename name, and it is
// pruned when the process exits — which is exactly why forgectl snapshots it.
type RegistryEntry struct {
	Pid       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updatedAt"`
	// ProcStart is the process's start time as Claude Code recorded it, in
	// ps(1)'s lstart layout and UTC ("Tue Sep 29 23:12:27 2026"). A pid can be
	// reused once its process exits; the start time cannot, so `resume restart`
	// compares it before signalling anything.
	ProcStart string `json:"procStart"`

	// Live is the result of probing Pid, not a field on disk. A registry
	// file whose process is gone is stale — the file outliving the process
	// is the normal crash case — so Status is never trusted without it.
	Live bool `json:"-"`
}

// Updated returns the entry's last-activity time, falling back to its start
// time when the process has not reported since launch.
func (e RegistryEntry) Updated() time.Time {
	if e.UpdatedAt > 0 {
		return time.UnixMilli(e.UpdatedAt)
	}
	return time.UnixMilli(e.StartedAt)
}

// pidAlive is the liveness probe, indirected so tests can pin a pid dead or
// alive without spawning processes.
var pidAlive = processAlive

// ReadEntry re-reads one pid's registry file, <pid>.json, straight from disk,
// with Live probed fresh. It reports false when the file is missing, will not
// parse, or names an invalid session id — each of which means the entry a
// caller saw earlier can no longer be vouched for.
func ReadEntry(p Paths, pid int) (RegistryEntry, bool) {
	if pid <= 0 {
		return RegistryEntry{}, false
	}
	path := filepath.Join(p.registryDir(), strconv.Itoa(pid)+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- <int>.json under the caller's own ~/.claude/sessions
	if err != nil {
		return RegistryEntry{}, false
	}
	var e RegistryEntry
	// A body naming another pid than its file name is not this pid's entry.
	if json.Unmarshal(data, &e) != nil || !validSessionID(e.SessionID) || e.Pid != pid {
		return RegistryEntry{}, false
	}
	e.Live = pidAlive(e.Pid)
	return e, true
}

// LiveSession returns the live registry entry for a session id, if any
// process currently holds it.
func LiveSession(p Paths, sessionID string) (RegistryEntry, bool) {
	e, ok := readRegistry(p.registryDir())[sessionID]
	if !ok || !e.Live {
		return RegistryEntry{}, false
	}
	return e, true
}

// readRegistry reads every live-session file, keyed by session id. A file that
// will not parse is skipped rather than failing the scan: the registry is
// written by another process and may be caught mid-write.
//
// Two files can name the same session id (a stale one left by a crashed
// process, plus the live one). The live entry always wins; among two dead
// entries the more recently updated does.
func readRegistry(dir string) map[string]RegistryEntry {
	out := map[string]RegistryEntry{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, de.Name())) // #nosec G304 -- a .json under the caller's own ~/.claude/sessions
		if err != nil {
			continue
		}
		var e RegistryEntry
		// Same admission guard as scanHistory: a registry file is written by
		// another process, and its session id flows on into path joins and
		// claude's argv.
		if json.Unmarshal(data, &e) != nil || !validSessionID(e.SessionID) {
			continue
		}
		e.Live = pidAlive(e.Pid)
		prev, seen := out[e.SessionID]
		if seen && !supersedes(e, prev) {
			continue
		}
		out[e.SessionID] = e
	}
	return out
}

// supersedes reports whether e should replace prev for the same session id.
func supersedes(e, prev RegistryEntry) bool {
	if e.Live != prev.Live {
		return e.Live
	}
	return e.Updated().After(prev.Updated())
}

// LiveEntries returns the registry entries whose process is still running —
// the input to Snapshot, since only a live session has a /rename name and
// undeleted tasks to capture.
//
// Sorted by session id, and that is load-bearing rather than tidiness. Two live
// sessions in one checkout can both resolve to the same team task directory
// when neither has claimed it yet, and Snapshot awards it to whichever it
// processes FIRST — a claim that is then sticky forever. Ranging over
// readRegistry's map directly made that a coin flip per run, so identical
// on-disk state could pair a session with another session's tasks depending on
// Go's map iteration order. A stable order makes the outcome a function of the
// state rather than of the run.
func LiveEntries(p Paths) []RegistryEntry {
	var out []RegistryEntry
	for _, e := range readRegistry(p.registryDir()) {
		if e.Live {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}
