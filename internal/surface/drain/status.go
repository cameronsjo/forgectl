package drain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// Pauses is the set of reasons claiming is paused, by kind.
type Pauses map[PauseKind]string

// Set records reason for kind and reports whether that changed anything.
func (p Pauses) Set(kind PauseKind, reason string) bool {
	if old, ok := p[kind]; ok && old == reason {
		return false
	}
	p[kind] = reason
	return true
}

// Clear drops kind and reports whether it was set.
func (p Pauses) Clear(kind PauseKind) bool {
	if _, ok := p[kind]; !ok {
		return false
	}
	delete(p, kind)
	return true
}

// Paused reports whether any reason holds.
func (p Pauses) Paused() bool { return len(p) > 0 }

// Reason is every reason, ordered by kind, joined.
func (p Pauses) Reason() string {
	kinds := slices.Sorted(maps.Keys(p))
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, string(k)+": "+p[k])
	}
	return strings.Join(parts, "; ")
}

// Drain process states, as drain.json records them.
const (
	StatusRunning = "running"
	StatusPaused  = "paused"
	StatusStopped = "stopped"
	// StatusStale is never written: a reader reports it for a running or
	// paused drain whose last tick is older than StaleTicks intervals.
	StatusStale = "stale"
)

// StatusVersion is drain.json's format version.
const StatusVersion = 1

// Attention is one row the operator should look at: in needs-you or failed.
type Attention struct {
	Name      string    `json:"name"`
	Repo      string    `json:"repo"`
	State     string    `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	StateAt   time.Time `json:"state_at"`
}

// Status is drain.json.
type Status struct {
	V            int    `json:"v"`
	Status       string `json:"status"`
	PauseReason  string `json:"pause_reason,omitempty"`
	PID          int    `json:"pid"`
	ProcessStart int64  `json:"process_start"`
	HerdrSession string `json:"herdr_session"`
	HerdrPath    string `json:"herdr_path,omitempty"`
	// StartedAt is when the drain process took the lock.
	StartedAt       time.Time      `json:"started_at"`
	LastTick        time.Time      `json:"last_tick"`
	IntervalSeconds int64          `json:"interval_seconds"`
	Seq             int64          `json:"seq"`
	Counts          map[string]int `json:"counts"`
	Attention       []Attention    `json:"attention"`
}

// DecodeStatus parses drain.json, refusing unknown fields and versions.
func DecodeStatus(data []byte) (Status, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Status
	if err := dec.Decode(&s); err != nil {
		return Status{}, fmt.Errorf("drain.json does not parse: %w", err)
	}
	if s.V != StatusVersion {
		return Status{}, fmt.Errorf("drain.json is version %d, this build reads %d", s.V, StatusVersion)
	}
	return s, nil
}

// Effective is the status a reader reports: running or paused becomes stale
// when the last tick is older than StaleTicks intervals.
func Effective(s Status, now time.Time) string {
	if s.Status != StatusRunning && s.Status != StatusPaused {
		return s.Status
	}
	interval := time.Duration(s.IntervalSeconds) * time.Second
	if interval <= 0 || now.Sub(s.LastTick) > StaleTicks*interval {
		return StatusStale
	}
	return s.Status
}

// Summarize counts rows per state and lists the rows needing attention,
// oldest first.
func Summarize(rows []worker.QueueRow) (map[string]int, []Attention) {
	counts := map[string]int{}
	attention := []Attention{}
	for _, r := range rows {
		counts[string(r.State)]++
		if r.State == worker.QueueNeedsYou || r.State == worker.QueueFailed {
			attention = append(attention, Attention{Name: r.Name, Repo: r.Repo, State: string(r.State), LastError: r.LastError, StateAt: r.StateAt})
		}
	}
	slices.SortStableFunc(attention, func(a, b Attention) int { return a.StateAt.Compare(b.StateAt) })
	return counts, attention
}

// Event kinds.
const (
	EventStart      = "start"
	EventStop       = "stop"
	EventState      = "state"
	EventPause      = "pause"
	EventResume     = "resume"
	EventUnreadable = "unreadable"
	EventError      = "error"
)

// EventVersion is the events file's line format version.
const EventVersion = 1

// Event is one line of drain-events.jsonl.
type Event struct {
	V       int       `json:"v"`
	TS      time.Time `json:"ts"`
	Seq     int64     `json:"seq"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name,omitempty"`
	Repo    string    `json:"repo,omitempty"`
	State   string    `json:"state,omitempty"`
	Attempt int       `json:"attempt"`
	Error   string    `json:"error,omitempty"`
}

// ParseEvents reads events from the files' contents, oldest first. A line
// that does not parse, or has another version, is returned as an error
// naming its position; the lines before it are kept.
func ParseEvents(chunks ...[]byte) ([]Event, error) {
	var out []Event
	for _, chunk := range chunks {
		for i, line := range bytes.Split(chunk, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Event
			if err := json.Unmarshal(line, &e); err != nil {
				return out, fmt.Errorf("drain events line %d does not parse: %w", i+1, err)
			}
			if e.V != EventVersion {
				return out, fmt.Errorf("drain events line %d is version %d, this build reads %d", i+1, e.V, EventVersion)
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// Since returns the events after cursor from the latest drain run: seq
// counts from 1 in each run, so a cursor never reaches back into an earlier
// run. A zero cursor returns every event given.
func Since(events []Event, cursor int64) []Event {
	if cursor <= 0 {
		return events
	}
	start := 0
	for i, e := range events {
		if e.Seq == 1 {
			start = i
		}
	}
	var out []Event
	for _, e := range events[start:] {
		if e.Seq > cursor {
			out = append(out, e)
		}
	}
	return out
}
