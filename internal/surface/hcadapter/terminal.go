package hcadapter

import (
	"bytes"
	"encoding/json"
	"errors"
)

// terminal is what forgectl reads from `hc get --json`. Every other field hc
// reports is ignored.
type terminal struct {
	Address   string            `json:"address"`
	CreatedMS int64             `json:"created_ms"`
	State     string            `json:"state"`
	Labels    map[string]string `json:"labels"`
	Turn      int               `json:"turn"`
	Status    status            `json:"status"`
}

// Terminal states hc reports.
const (
	stateLive     = "live"
	stateFinished = "finished"
)

// status is hc's OSC 7501 status. `hc get` reports it as an object
// ({app, msg, state}), `hc ls` and `hc send` as a bare string, and a plain
// program that never reported one as null. All three decode here.
type status struct {
	State string
	App   string
}

func (s *status) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case bytes.Equal(data, []byte("null")):
		*s = status{}
		return nil
	case len(data) > 0 && data[0] == '"':
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*s = status{State: v}
		return nil
	case len(data) > 0 && data[0] == '{':
		var v struct {
			State string `json:"state"`
			App   string `json:"app"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*s = status{State: v.State, App: v.App}
		return nil
	}
	return errors.New("hc status is neither null, a string, nor an object")
}

// Status states hc documents. Anything else is passed through as reported and
// treated as not ready.
const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusDone    = "done"
	StatusBlocked = "blocked"
	StatusError   = "error"
)

// valid reports whether t has the fields an identity check needs.
func (t terminal) valid() bool {
	return t.Address != "" && t.CreatedMS > 0 && (t.State == stateLive || t.State == stateFinished)
}
