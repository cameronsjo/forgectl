package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// UsageTimestamp renders t as the exact activity timestamp a row carries: UTC,
// RFC3339, truncated to the second. Sub-second precision is dropped
// deliberately — it narrows nothing an operator asked to measure and only
// sharpens the activity trace the row already is.
func UsageTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// Validate checks the closed vocabularies and the timestamp before anything is
// opened, so a malformed event is refused in memory rather than appended and
// skipped forever after.
func (e UsageEventV1) Validate() error {
	if e.SchemaVersion != UsageSchemaVersion {
		return fmt.Errorf("%w: schema_version %d is not %d", ErrUsageInvalidEvent, e.SchemaVersion, UsageSchemaVersion)
	}
	if e.Event != UsageEventExecAttempt {
		return fmt.Errorf("%w: unknown event", ErrUsageInvalidEvent)
	}
	if e.Harness != "claude" && e.Harness != "codex" {
		return fmt.Errorf("%w: unknown harness", ErrUsageInvalidEvent)
	}
	switch e.SessionMode {
	case UsageSessionNew, UsageSessionResume, UsageSessionFork, UsageSessionUnknown:
	default:
		return fmt.Errorf("%w: unknown session_mode", ErrUsageInvalidEvent)
	}
	switch e.Posture {
	case UsagePostureDefault, UsagePostureBuilder, UsagePostureAgents:
	default:
		return fmt.Errorf("%w: unknown posture", ErrUsageInvalidEvent)
	}
	// A local-offset timestamp would leak the operator's timezone into a row
	// that is otherwise location-free, so the canonical UTC "Z" form is the
	// only accepted spelling — not merely a parseable RFC3339 string.
	parsed, err := time.Parse(time.RFC3339, e.TS)
	if err != nil || UsageTimestamp(parsed) != e.TS {
		return fmt.Errorf("%w: ts must be a UTC RFC3339 second", ErrUsageInvalidEvent)
	}
	return nil
}

// EncodeUsageEvent validates and renders one complete row plus its newline.
//
// The whole row is built and bounded in memory before the caller is allowed to
// touch the store: an oversized or invalid event must cost zero opens, zero
// locks, and zero bytes on disk. The returned slice is written in exactly one
// call, so a row is either wholly present or recoverably absent.
func EncodeUsageEvent(e UsageEventV1) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Keep the raw model string byte-exact: HTML escaping would rewrite <, >,
	// and & in a value the operator configured, and this file is never served
	// to a browser.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUsageInvalidEvent, err)
	}
	// json.Encoder already appends the newline that terminates the row.
	if buf.Len() > MaxUsageRowBytes {
		return nil, fmt.Errorf("%w: %d bytes over the %d-byte bound", ErrUsageRowTooLarge, buf.Len()-MaxUsageRowBytes, MaxUsageRowBytes)
	}
	return buf.Bytes(), nil
}
