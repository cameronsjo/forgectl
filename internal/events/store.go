// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package events

import (
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// The store's files, all in one private directory
// ($XDG_STATE_HOME/forgectl/events, else ~/.local/state/forgectl/events):
//
//	lock                 flock(2) target; writers take it exclusive, readers shared
//	watch.json           which sessions watch which PRs
//	state.json           the last snapshot of each PR, and the last Seq
//	log.jsonl            every event, for the machine
//	feed.<session>.jsonl the events for one session's PRs
const (
	leafName  = "events"
	lockName  = "lock"
	pollLock  = "poll.lock"
	watchName = "watch.json"
	stateName = "state.json"
	logName   = "log.jsonl"
)

// Bounds. A feed or log past maxLogBytes is cut to its newest keepLines
// lines; a JSON file past maxJSONBytes is refused, not parsed.
const (
	maxJSONBytes = 4 << 20
	maxLogBytes  = 1 << 20
	keepLines    = 500
	// maxWatched caps the PRs one session watches, so a loop that watches
	// every PR it sees cannot make each poll cost hundreds of API calls.
	maxWatched = 50
	// keepState is how long a snapshot no session watches is kept, so the
	// merge gate can still read a PR shortly after its session moved on.
	keepState = 7 * 24 * time.Hour
)

// ErrTooMany is a watch that would put a session past maxWatched PRs.
var ErrTooMany = errors.New("a session watches at most 50 PRs; unwatch some first")

// Store is the event log on disk. Every method opens the directory, takes
// the lock, does its work against the pinned descriptor and lets go; no
// method holds anything between calls, so a poller and any number of
// readers can share it.
type Store struct {
	base string // the directory that holds leafName
}

// DefaultStore is the store under forgectl's machine-local state directory.
func DefaultStore() (*Store, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return nil, err
	}
	return &Store{base: filepath.Join(base, "forgectl")}, nil
}

// NewStore is a store whose directory is base/events. Tests use it.
func NewStore(base string) *Store { return &Store{base: base} }

// Dir is the store's directory.
func (s *Store) Dir() string { return filepath.Join(s.base, leafName) }

// watchFile is watch.json: session id to the refs it watches, each sorted.
type watchFile struct {
	Sessions map[string][]string `json:"sessions"`
}

// stateFile is state.json.
type stateFile struct {
	Seq int64              `json:"seq"`
	PRs map[string]PRState `json:"prs"`
}

// Watching is one session's refs.
type Watching map[string][]Ref

// Refs is every ref any session watches, each once, sorted.
func (w Watching) Refs() []Ref {
	seen := map[string]Ref{}
	for _, rs := range w {
		for _, r := range rs {
			seen[r.String()] = r
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Ref, 0, len(keys))
	for _, k := range keys {
		out = append(out, seen[k])
	}
	return out
}

// parseWatch turns watch.json into Watching, dropping any session id or ref
// that does not parse.
func parseWatch(f watchFile) Watching {
	w := Watching{}
	for sid, refs := range f.Sessions {
		if !ValidSession(sid) {
			continue
		}
		for _, raw := range refs {
			if r, err := ParseRef(raw); err == nil && r.String() == raw {
				w[sid] = append(w[sid], r)
			}
		}
	}
	return w
}

func (w Watching) file() watchFile {
	f := watchFile{Sessions: map[string][]string{}}
	for sid, refs := range w {
		if len(refs) == 0 {
			continue
		}
		keys := make([]string, 0, len(refs))
		for _, r := range refs {
			keys = append(keys, r.String())
		}
		slices.Sort(keys)
		f.Sessions[sid] = slices.Compact(keys)
	}
	return f
}

// add puts refs on session's list. It refuses past maxWatched.
func (w Watching) add(session string, refs []Ref) error {
	have := map[string]bool{}
	for _, r := range w[session] {
		have[r.String()] = true
	}
	for _, r := range refs {
		if !have[r.String()] {
			have[r.String()] = true
			w[session] = append(w[session], r)
		}
	}
	if len(w[session]) > maxWatched {
		return ErrTooMany
	}
	return nil
}

// remove takes refs off session's list, or every ref when refs is empty.
func (w Watching) remove(session string, refs []Ref) {
	if len(refs) == 0 {
		delete(w, session)
		return
	}
	drop := map[string]bool{}
	for _, r := range refs {
		drop[r.String()] = true
	}
	w[session] = slices.DeleteFunc(w[session], func(r Ref) bool { return drop[r.String()] })
	if len(w[session]) == 0 {
		delete(w, session)
	}
}

// finish takes a merged or closed PR off every list.
func (w Watching) finish(ref Ref) {
	for sid := range w {
		w.remove(sid, []Ref{ref})
	}
}

// watchers are the sessions that watch ref, sorted.
func (w Watching) watchers(ref Ref) []string {
	var out []string
	for sid, refs := range w {
		if slices.ContainsFunc(refs, func(r Ref) bool { return r == ref }) {
			out = append(out, sid)
		}
	}
	slices.Sort(out)
	return out
}

func feedName(session string) string { return "feed." + session + ".jsonl" }

// stamp gives each event its Seq and At, from seq+1 on, and returns the
// new last Seq.
func stamp(evs []Event, seq int64, now time.Time) int64 {
	at := now.UTC().Format(time.RFC3339)
	for i := range evs {
		seq++
		evs[i].Seq = seq
		evs[i].At = at
	}
	return seq
}

// prune drops snapshots no session watches that are older than keepState.
func prune(st *stateFile, w Watching, now time.Time) {
	watched := map[string]bool{}
	for _, r := range w.Refs() {
		watched[r.String()] = true
	}
	for k, snap := range st.PRs {
		if watched[k] {
			continue
		}
		t, err := time.Parse(time.RFC3339, snap.PolledAt)
		if err != nil || now.Sub(t) > keepState {
			delete(st.PRs, k)
		}
	}
}
