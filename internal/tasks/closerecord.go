package tasks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The outcomes a close record can carry. Four describe an update that was
// sent; the fifth describes a call refused before anything was sent, because
// the session had used up its closes.
const (
	// CloseOutcomeClosed: the update was sent and the read-back shows done.
	CloseOutcomeClosed = "closed"
	// CloseOutcomeNotConfirmed: the update was sent and nothing read
	// afterwards shows the task done. It may have been applied.
	CloseOutcomeNotConfirmed = "not_confirmed"
	// CloseOutcomeWriteRefused: the update was sent and the server refused it.
	CloseOutcomeWriteRefused = "write_refused"
	// CloseOutcomeUnauthorized: the update was sent and refused because this
	// credential, which had just read the task, may not update it.
	CloseOutcomeUnauthorized = "unauthorized"
	// CloseOutcomeCap: the MCP session's close limit refused the call. No
	// request was made, so the record's project id is zero.
	CloseOutcomeCap = "close_cap"
)

var closeOutcomes = []string{
	CloseOutcomeClosed, CloseOutcomeNotConfirmed, CloseOutcomeWriteRefused,
	CloseOutcomeUnauthorized, CloseOutcomeCap,
}

// CredentialSourceTokenFile is the credential source a record names when the
// token came from a mounted file. The file's path is operator configuration
// and is not what a reader of the record needs; that the token was not a
// keychain entry is.
const CredentialSourceTokenFile = "token-file"

// CloseRecord is one line of the close log: who asked for which task to be
// closed, through what, with which credential, and what became of it.
//
// It exists because the trailer on the task is not a record. The trailer is
// board text that anyone the project is shared with can edit, and a refused
// or unconfirmed update leaves no trailer at all.
//
// Credential names where the token came from — a keychain service name, or
// CredentialSourceTokenFile. It is never the token: no field here is a Token,
// and WriteCloseRecord is handed none to read.
type CloseRecord struct {
	// Time is when the call was made. The zero value means now.
	Time time.Time
	// TaskID is the id the caller asked to close. ProjectID is the project
	// the pre-read found it in, and zero when nothing was read.
	TaskID, ProjectID int
	// Surface is SurfaceMCP or SurfaceDone.
	Surface string
	// Closer is the name the caller declared, as declared. WriteCloseRecord
	// reduces it the way the trailer does.
	Closer string
	// Evidence is the caller's evidence text.
	Evidence string
	// Credential and Host say which credential source was used against which
	// instance. Both are operator configuration.
	Credential, Host string
	// Outcome is one of the CloseOutcome constants.
	Outcome string
}

// closeRecordLine is the record as written. The field order is the reading
// order of a line: when, what, who, why, with what, and how it ended.
type closeRecordLine struct {
	Time       string `json:"time"`
	TaskID     int    `json:"task_id"`
	ProjectID  int    `json:"project_id"`
	Surface    string `json:"surface"`
	Closer     string `json:"closer"`
	Evidence   string `json:"evidence"`
	Credential string `json:"credential"`
	Host       string `json:"host"`
	Outcome    string `json:"outcome"`
}

// WriteCloseRecord writes rec to w as one line of JSON.
//
// It writes to w and nowhere else. In particular it does not go through
// slog: forgectl's global logger discards everything unless `log_level` is
// set, and a close record that exists only when logging happens to be on is
// not a record.
//
// The line is safe to print: it goes through termsafe.JSONEncoder, so a
// control or invisible character in any field is written as an escape.
//
// Two fields are caller text and are reduced here, so that every caller gets
// the same treatment and none can forget it:
//
//   - Closer goes through the sanitizer the trailer uses, with the same
//     fallback, so the record and the trailer on the task name the closer
//     identically.
//   - Evidence that a trailer would refuse — a line break, a control
//     character, more than maxEvidenceRunes, or something shaped like an API
//     token — is written as the empty string. A record of a refused call
//     must not become the place the refused text lands.
//
// The whole line is built before the first byte is written, so a refusal
// here leaves w untouched.
func WriteCloseRecord(w io.Writer, rec CloseRecord) error {
	if w == nil {
		return errors.New("tasks: close record: no record writer is configured")
	}
	fallback, err := defaultCloser(rec.Surface)
	if err != nil {
		return fmt.Errorf("tasks: close record: %w", err)
	}
	if !slices.Contains(closeOutcomes, rec.Outcome) {
		return fmt.Errorf("tasks: close record: unknown outcome %s", termsafe.QuoteArgMax(rec.Outcome, 0))
	}
	when := rec.Time
	if when.IsZero() {
		when = time.Now()
	}
	evidence := ""
	if _, err := sanitizeEvidence(rec.Evidence); err == nil {
		evidence = strings.TrimSpace(rec.Evidence)
	}

	var line bytes.Buffer
	if err := termsafe.JSONEncoder(&line).Encode(closeRecordLine{
		Time:       when.UTC().Format(time.RFC3339),
		TaskID:     rec.TaskID,
		ProjectID:  rec.ProjectID,
		Surface:    rec.Surface,
		Closer:     sanitizeCloser(rec.Closer, fallback),
		Evidence:   evidence,
		Credential: rec.Credential,
		Host:       rec.Host,
		Outcome:    rec.Outcome,
	}); err != nil {
		return fmt.Errorf("tasks: close record: encode: %w", err)
	}
	if _, err := w.Write(line.Bytes()); err != nil {
		return fmt.Errorf("tasks: close record: write: %w", err)
	}
	return nil
}
