package resume

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		if json.Unmarshal(data, &e) != nil || e.SessionID == "" {
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
func LiveEntries(p Paths) []RegistryEntry {
	var out []RegistryEntry
	for _, e := range readRegistry(p.registryDir()) {
		if e.Live {
			out = append(out, e)
		}
	}
	return out
}
