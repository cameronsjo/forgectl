package projectview

import (
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
)

const (
	AgentsVersion      = 1
	MaxAgentRows       = 64
	MaxAgentSpans      = 512
	MaxAgentsJSONBytes = 2 << 20
)

type AgentsSnapshot struct {
	Version        int        `json:"version"`
	ProjectID      string     `json:"project_id"`
	ProjectName    string     `json:"project_name"`
	ObservedAt     time.Time  `json:"observed_at"`
	Rows           []AgentRow `json:"rows"`
	TotalThreads   int        `json:"total_threads"`
	DroppedThreads int        `json:"dropped_threads,omitempty"`
	DroppedSpans   int        `json:"dropped_spans,omitempty"`
	Partial        bool       `json:"partial,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

type AgentRow struct {
	ThreadID   string            `json:"thread_id"`
	Name       string            `json:"name"`
	Role       string            `json:"role"`
	RunID      string            `json:"run_id,omitempty"`
	Harness    string            `json:"harness,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Model      string            `json:"model,omitempty"`
	Enrollment string            `json:"enrollment"`
	State      string            `json:"state"`
	ObservedAt time.Time         `json:"observed_at"`
	Coverage   activity.Coverage `json:"coverage,omitempty"`
	Since      *time.Time        `json:"since,omitempty"`
	Spans      []AgentSpan       `json:"spans"`
	Points     []AgentPoint      `json:"points,omitempty"`
	Partial    bool              `json:"partial,omitempty"`
	Reason     string            `json:"reason,omitempty"`
}

type AgentSpan struct {
	RunID           string          `json:"run_id"`
	NativeSessionID string          `json:"native_session_id"`
	Source          activity.Source `json:"source"`
	Span            activity.Span   `json:"span"`
	ObservedThrough *time.Time      `json:"observed_through,omitempty"`
}

type AgentPoint struct {
	RunID  string          `json:"run_id"`
	TurnID string          `json:"turn_id"`
	At     time.Time       `json:"at"`
	State  activity.State  `json:"state"`
	Source activity.Source `json:"source"`
}
