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

// HostRefusalEvent is the `event` value of a host-refusal line. A close record
// has no `event` key, so a reader of the close log tells the two kinds of line
// apart by whether the key is there.
const HostRefusalEvent = "host_refused"

// maxRecordedHostRunes caps the host a refusal line carries. It is the
// longest name DNS allows, so a real hostname is never cut, and a refused
// value of any length adds at most one bounded line to the file.
const maxRecordedHostRunes = 253

// HostRefusalRecord is one refusal of the allowed-host rule: a `tasks` verb
// was asked to read a keychain credential for a host that credential may not
// be sent to, and stopped before the read.
//
// It is kept because the refusal is otherwise only an exit code and a line on
// stderr. When the host came from text an agent read on the board, both are
// gone with the agent's session, and the operator never learns that something
// tried to send a keychain token elsewhere.
//
// It holds no token. None has been read when the rule refuses.
type HostRefusalRecord struct {
	// Time is when the verb was refused. The zero value means now.
	Time time.Time
	// Verb is the `tasks` subcommand that was refused: ls, show, ready, done,
	// or mcp. It is the command's own name, never caller text.
	Verb string
	// Host is the host that was asked for, as given.
	Host string
	// Credential is the name of the keychain entry the verb would have read.
	Credential string
}

// hostRefusalLine is the record as written, in reading order: when, what
// happened, to which verb, for which host, with which entry.
type hostRefusalLine struct {
	Time       string `json:"time"`
	Event      string `json:"event"`
	Verb       string `json:"verb"`
	Host       string `json:"host"`
	Credential string `json:"credential"`
}

// WriteHostRefusalRecord writes rec to w as one line of JSON.
//
// The host is caller text and is written as given, cut to
// maxRecordedHostRunes: what was asked for is the point of the record. It goes
// through termsafe.JSONEncoder like every other field, so a line break, a
// control, or an invisible character in it is written as an escape and cannot
// start a second line or add a field.
//
// Credential is written only when it is a keychain service name
// (ValidKeychainService) and is the empty string otherwise. A value that fails
// that check is arbitrary text typed after a flag, and this file is not the
// place for it.
//
// The whole line is built before the first byte is written and handed to w in
// one Write.
func WriteHostRefusalRecord(w io.Writer, rec HostRefusalRecord) error {
	if w == nil {
		return errors.New("tasks: host refusal record: no record writer is configured")
	}
	when := rec.Time
	if when.IsZero() {
		when = time.Now()
	}
	host := rec.Host
	if runes := []rune(host); len(runes) > maxRecordedHostRunes {
		host = string(runes[:maxRecordedHostRunes])
	}
	credential := ""
	if ValidKeychainService(rec.Credential) {
		credential = rec.Credential
	}

	var line bytes.Buffer
	if err := termsafe.JSONEncoder(&line).Encode(hostRefusalLine{
		Time:       when.UTC().Format(time.RFC3339),
		Event:      HostRefusalEvent,
		Verb:       rec.Verb,
		Host:       host,
		Credential: credential,
	}); err != nil {
		return fmt.Errorf("tasks: host refusal record: encode: %w", err)
	}
	if _, err := w.Write(line.Bytes()); err != nil {
		return fmt.Errorf("tasks: host refusal record: write: %w", err)
	}
	return nil
}
