package resume

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is one file in forgectl's snapshot store: everything about a session
// that Claude Code will not still be holding once the session exits.
//
// It is deliberately more than a task cache. Claude Code records the
// session-id → task-directory association NOWHERE durable (see tasks.go), so
// the store doubles as that index: the first Snapshot that observes a live
// session together with its task directory writes the pairing down, and every
// later Restore reads it back. That is the only reason task rescue survives a
// session whose task directory is not named after it.
type Record struct {
	ID         string    `json:"id"`
	Cwd        string    `json:"cwd,omitempty"`
	Name       string    `json:"name,omitempty"`
	NameSource string    `json:"name_source,omitempty"`
	Version    string    `json:"version,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	TaskDir    string    `json:"task_dir,omitempty"`
	LastSeen   time.Time `json:"last_seen"`
	Tasks      []Task    `json:"tasks,omitempty"`
}

// Task is one task body, carried both parsed (for display) and raw (for
// byte-faithful write-back). Restore replays Raw rather than re-encoding the
// parsed fields, so a field Claude Code adds in a later version survives a
// snapshot/restore round trip through a forgectl that never heard of it.
type Task struct {
	ID          string          `json:"id"`
	Subject     string          `json:"subject,omitempty"`
	Description string          `json:"description,omitempty"`
	ActiveForm  string          `json:"activeForm,omitempty"`
	Status      string          `json:"status,omitempty"`
	Blocks      []string        `json:"blocks,omitempty"`
	BlockedBy   []string        `json:"blockedBy,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

// Load reads one session's record. The bool reports whether one existed; a
// record that will not parse reads as absent, since a corrupt cache should
// degrade to "not snapshotted yet" rather than fail the command.
func Load(dir, id string) (*Record, bool) {
	if dir == "" || !validSessionID(id) {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json")) // #nosec G304 -- id is validated to be a session-id shape
	if err != nil {
		return nil, false
	}
	var r Record
	if json.Unmarshal(data, &r) != nil {
		return nil, false
	}
	// The id inside the file is a THIRD entry point, distinct from the
	// filename this function was handed. LoadAll keys its result on r.ID, so
	// a record whose body disagreed with its filename would put an
	// unvalidated string into Scan's session map — and from there into
	// transcript path joins and claude's argv, which is exactly the class the
	// admission checks in scanHistory and readRegistry close. Requiring the
	// two to agree validates the body and rejects a mismatch in one step.
	if r.ID != id {
		return nil, false
	}
	return &r, true
}

// LoadAll reads every record in the store, keyed by session id.
func LoadAll(dir string) map[string]*Record {
	out := map[string]*Record{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if r, ok := Load(dir, id); ok {
			out[r.ID] = r
		}
	}
	return out
}

// Save writes one record, replacing any previous one.
//
// The write is atomic (temp file in the same directory, then rename) because
// Save runs from a Stop hook at every turn end: a snapshot interrupted
// mid-write must not leave a truncated record where a complete one was.
func Save(dir string, r *Record) error {
	if !validSessionID(r.ID) {
		return fmt.Errorf("refusing to store record with invalid session id %q", r.ID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create resume store %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(dir, r.ID+".json")
	tmp, err := os.CreateTemp(dir, r.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp record in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, final)
}

// mergeTasks folds live task bodies over previously snapshotted ones, keyed by
// task id.
//
// The direction is the whole point: Claude Code DELETES task files when a
// session ends, so a later snapshot legitimately sees fewer tasks than an
// earlier one. Dropping to the live set would quietly discard exactly the
// tasks the feature exists to rescue — so prior tasks are retained, and a
// live body only overwrites the stored one for the same id.
func mergeTasks(prior, live []Task) []Task {
	if len(prior) == 0 {
		return live
	}
	byID := make(map[string]Task, len(prior)+len(live))
	order := make([]string, 0, len(prior)+len(live))
	for _, t := range prior {
		if _, seen := byID[t.ID]; !seen {
			order = append(order, t.ID)
		}
		byID[t.ID] = t
	}
	for _, t := range live {
		if _, seen := byID[t.ID]; !seen {
			order = append(order, t.ID)
		}
		byID[t.ID] = t
	}
	sortTaskIDs(order)
	out := make([]Task, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// validSessionID guards every path built from an id AND every id that reaches
// claude's argv. Session ids are uuids and task-directory names are
// uuid-shaped; anything with a separator, a dot, or a non-uuid byte in it is
// not one, and must never reach filepath.Join.
//
// The leading-dash rejection is the argv half, and it is not theoretical: the
// hex-plus-dash charset alone admits "-c", "-d", and "-a", which reach
// `claude --resume <id>` as flag-shaped tokens. Whether claude's parser would
// actually treat one as a separate flag is unconfirmed — but the id is
// disk-sourced, the check costs nothing, and a real session id never starts
// with a dash.
func validSessionID(id string) bool {
	if id == "" || len(id) > 64 || id[0] == '-' {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
