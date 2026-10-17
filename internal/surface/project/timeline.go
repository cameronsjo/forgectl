package project

import "time"

// MailStatus is the transport evidence a project timeline may expose. It is
// deliberately narrower than the mailbox's internal status vocabulary.
type MailStatus string

const (
	MailWaiting           MailStatus = "waiting"
	MailTransportAccepted MailStatus = "transport_accepted"
	MailDeliveryUnknown   MailStatus = "delivery_unknown"
	MailDeliveryFailed    MailStatus = "delivery_failed"
)

// MailObservation is a sanitized, transport-only mailbox observation. Legacy
// mailbox records carry no project/thread/run identity, so this type does not
// invent attribution from member names, timestamps, or working directories.
type MailObservation struct {
	MessageID    string     `json:"message_id"`
	From         string     `json:"from"`
	To           string     `json:"to"`
	FromThreadID string     `json:"from_thread_id,omitempty"`
	ToThreadID   string     `json:"to_thread_id,omitempty"`
	FromRunID    string     `json:"from_run_id,omitempty"`
	ToRunID      string     `json:"to_run_id,omitempty"`
	At           time.Time  `json:"at"`
	Status       MailStatus `json:"status"`
}

// MailThreadSummary is bounded project-state evidence accompanying the
// mailbox snapshot. ProcessLiveness remains unknown: roster state is not
// liveness evidence.
type MailThreadSummary struct {
	ThreadID           string          `json:"thread_id"`
	Role               Role            `json:"role"`
	Enrollment         Enrollment      `json:"enrollment"`
	NativeSessionKnown bool            `json:"native_session_known"`
	ProcessLiveness    ProcessLiveness `json:"process_liveness"`
	Reason             string          `json:"reason,omitempty"`
}

// MailTimeline is a bounded read-only snapshot of one project's mailbox.
// Skipped records and held/partial input remain visible instead of becoming
// an apparently complete successful timeline.
type MailTimeline struct {
	ProjectID       string              `json:"project_id"`
	ObservedAt      time.Time           `json:"observed_at"`
	ThreadSummaries []MailThreadSummary `json:"thread_summaries"`
	Entries         []MailObservation   `json:"entries"`
	Skipped         int                 `json:"skipped,omitempty"`
	Partial         bool                `json:"partial,omitempty"`
	Held            bool                `json:"held,omitempty"`
}
