package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Sentinels for a close. A caller matches on these with errors.Is to pick a
// result code; the wrapped text says what happened to this task.
var (
	// ErrNotFound means the pre-read answered 404. A 404 does not say whether
	// the task is absent or only out of this credential's reach, so the
	// message claims neither.
	ErrNotFound = errors.New("tasks: task not found")

	// ErrRepeatingTask means the task has a repeat interval or repeat mode.
	// This client closes one-off tasks only: on a repeating task "done" means
	// this occurrence, and what happens next belongs to the repeat rule. It
	// is refused before any write.
	ErrRepeatingTask = errors.New("tasks: refusing to close a repeating task")

	// ErrTrailerTooLong means the description with its closing trailer would
	// be over the send limit. The description is never cut to make room and
	// the close is never sent without its record, so the close is refused.
	ErrTrailerTooLong = errors.New("tasks: the description and its closing trailer are over the send limit")

	// ErrNotConfirmed means an update was sent and nothing this client read
	// afterwards shows the task done. The update may have been applied.
	//
	// An error carrying it never also carries ErrUnreachable or
	// ErrUnauthorized, whatever the underlying failure was. Those two have
	// dispositions built for reads — one may be answered from cache, the
	// other means the credential is dead — and neither is true of a write
	// whose outcome is unknown.
	ErrNotConfirmed = errors.New("tasks: close not confirmed")

	// ErrWriteRefused means the update was sent and the server answered with
	// a refusal, so the task is unchanged. It is what separates "the write
	// was refused" from "the pre-read failed and no write was sent", which
	// otherwise share ErrUnauthorized and ErrUnexpectedStatus.
	ErrWriteRefused = errors.New("tasks: the Vikunja instance refused the update")
)

// maxCloseBodyBytes bounds the task object a close will work from. The whole
// object is sent back in the update, so its size is this client's request
// size; a real task is a few kilobytes, and one near this ceiling is not a
// task object.
const maxCloseBodyBytes = 1 << 20

// expectedCloseKeys are the keys a close is expected to change: the two this
// client sets, the two timestamps the server stamps, and the board column and
// position a done task can be moved to. A difference in any other key between
// the pre-read and the read-back is reported in CloseResult.
var expectedCloseKeys = []string{"done", "done_at", "updated", "description", "bucket_id", "position"}

// knownTaskKeys is the key set of a Vikunja task object. A changed key is
// named in CloseResult.ChangedKeys only when it is in this list; see
// nameableKey.
var knownTaskKeys = []string{
	"id", "title", "description", "done", "done_at", "due_date", "start_date",
	"end_date", "reminders", "project_id", "repeat_after", "repeat_mode",
	"priority", "assignees", "labels", "hex_color", "percent_done",
	"identifier", "index", "related_tasks", "attachments",
	"cover_image_attachment_id", "is_favorite", "created", "updated",
	"bucket_id", "position", "reactions", "created_by",
}

// nameableKey is the shape a key must have before it is returned by name. A
// JSON object key is server text like any other, and ChangedKeys reaches tool
// output outside the board-text fence, so a key is named only when it has
// this shape AND is in knownTaskKeys. Anything else is counted.
var nameableKey = regexp.MustCompile(`^[a-z_]{1,40}$`)

// CloseRequest is one close: one task id, who is closing it, through which
// surface, and what the closer observed that says the work is finished.
//
// Closer is self-declared and is sanitized, not trusted. Surface is
// SurfaceMCP or SurfaceDone. Now is the time written into the trailer; the
// zero value means the current time.
type CloseRequest struct {
	TaskID                    int
	Closer, Surface, Evidence string
	Now                       time.Time
}

// CloseResult is what a close did. Title is board text: a caller showing it
// must fence or escape it like any other title.
type CloseResult struct {
	ID, ProjectID int
	Title         string
	// AlreadyDone is true when the pre-read showed the task done. Nothing was
	// written, so EvidenceRecorded and Confirmed are false.
	AlreadyDone bool
	// EvidenceRecorded is true only when the read-back's description ends
	// with the trailer this call wrote, line breaks after it aside. A trailer
	// already on the task never sets it.
	EvidenceRecorded bool
	// Confirmed is true when this call's update was sent and the read-back
	// says the task is done.
	Confirmed bool
	// ChangedKeys names the known task keys, outside expectedCloseKeys, whose
	// value differs between the pre-read and the read-back. UnnamedChanges
	// counts every other difference.
	ChangedKeys    []string
	UnnamedChanges int
}

// CompleteTask marks one task done and records who closed it and why.
//
//  1. GET the task. Any failure, or an object that is not shaped like a task,
//     ends the call with nothing written.
//  2. A task that is already done is returned as AlreadyDone, unwritten.
//  3. A repeating task is refused.
//  4. The trailer is appended to the description; over the limit is refused.
//  5. The object from step 1 is copied with `done` and `description` replaced.
//  6. POST it back.
//  7. GET the task again. That read-back, not the POST's answer, decides the
//     outcome.
//
// The update body is the pre-read's own object, value for value. A body built
// from the fields this package models would leave the rest out, and what the
// server does with a field left out of an update is not something to find out
// on a live board.
//
// An edit made by someone else between steps 1 and 6 is overwritten. The
// window is two requests wide.
//
// The request is checked before step 1, so a bad id, surface, or evidence
// makes no request at all.
func (c *Client) CompleteTask(ctx context.Context, req CloseRequest) (CloseResult, error) {
	if req.TaskID <= 0 {
		return CloseResult{}, fmt.Errorf("tasks: close: task_id must be a positive task id, got %d", req.TaskID)
	}
	trailer, err := trailerLine(trailerClosedBy, req.Closer, req.Surface, req.Now, req.Evidence)
	if err != nil {
		return CloseResult{}, fmt.Errorf("tasks: close task %d: %w", req.TaskID, err)
	}
	path := fmt.Sprintf("/tasks/%d", req.TaskID)

	// Pre-read, fail closed. A task this credential cannot read is one it
	// must not update, and the pre-read is also the only source of the body.
	body, err := c.get(ctx, path, nil)
	if err != nil {
		if statusIs(err, http.StatusNotFound) {
			return CloseResult{}, fmt.Errorf("%w: no task %d, or this credential cannot see it", ErrNotFound, req.TaskID)
		}
		return CloseResult{}, fmt.Errorf("tasks: close task %d: the pre-read failed, so nothing was written: %w", req.TaskID, err)
	}
	before, err := decodeCloseTask(body, req.TaskID)
	if err != nil {
		return CloseResult{}, fmt.Errorf("tasks: close task %d: the pre-read is not a task this client will update, so nothing was written: %w",
			req.TaskID, err)
	}
	result := CloseResult{ID: req.TaskID, ProjectID: before.projectID, Title: before.title}

	if before.done {
		// Never written: a second trailer on a done task would be a record of
		// a close that did not happen.
		result.AlreadyDone = true
		return result, nil
	}
	if before.repeating {
		return result, fmt.Errorf("%w: task %d has a repeat interval or repeat mode set; nothing was written",
			ErrRepeatingTask, req.TaskID)
	}
	description, err := closeDescription(before.description, trailer)
	if err != nil {
		return result, fmt.Errorf("tasks: close task %d: nothing was written: %w", req.TaskID, err)
	}

	// termsafe:allow-raw-json one string value of an outbound API request body, never terminal output
	encoded, err := json.Marshal(description)
	if err != nil {
		return result, fmt.Errorf("tasks: close task %d: encode description: %w", req.TaskID, err)
	}
	// Every other value stays the json.RawMessage the server sent. Decoding
	// one into a Go value and encoding it again would rewrite a number that
	// does not fit a float64 — a position, a large id — into a different one.
	update := maps.Clone(before.raw)
	update["done"] = json.RawMessage("true")
	update["description"] = encoded

	if _, err := c.post(ctx, path, update); err != nil {
		return result, closeWriteError(req.TaskID, err)
	}

	// Read-back. The POST's own response is not consulted: a proxy can answer
	// 200 for a write the server never applied, and the server can accept an
	// update and leave the task open.
	body, err = c.get(ctx, path, nil)
	if err != nil {
		return result, notConfirmed(req.TaskID, "the update was accepted and the read-back failed", err)
	}
	after, err := decodeCloseTask(body, req.TaskID)
	if err != nil {
		return result, notConfirmed(req.TaskID, "the update was accepted and the read-back is not a task object", err)
	}
	if !after.done {
		return result, notConfirmed(req.TaskID, "the update was accepted and the read-back shows the task still open", nil)
	}
	result.Confirmed = true
	result.EvidenceRecorded = closingLine(after.description) == trailer
	result.ChangedKeys, result.UnnamedChanges = unexpectedChanges(before.raw, after.raw)
	return result, nil
}

// closeWriteError classifies a failed update by what it says about the task.
func closeWriteError(taskID int, err error) error {
	var status statusError
	switch {
	case IsHostRefused(err):
		// Raised by the dialer before a byte was sent. The sentinel stays
		// intact so the caller still reports a refusal to send the credential.
		return fmt.Errorf("tasks: close task %d: the update was not sent: %w", taskID, err)
	case errors.Is(err, ErrUnauthorized):
		// The same credential read this task a moment ago, so this is not a
		// dead token: it is a token whose scope or project share stops at
		// reading. The caller is told so, because the bare status reads as
		// "try again later".
		return fmt.Errorf("%w: this credential can read task %d and may not update it; retrying will not help: %w",
			ErrWriteRefused, taskID, err)
	case errors.As(err, &status) && status.code < http.StatusInternalServerError:
		return fmt.Errorf("%w: task %d is unchanged: %w", ErrWriteRefused, taskID, err)
	}
	// A timeout, a dropped connection, a response that could not be read, or
	// a 5xx — which a proxy in front of the server can return for a request
	// the server did apply. None says the update was not applied.
	return notConfirmed(taskID, "the update was sent and its outcome is unknown", err)
}

// notConfirmedCauseMaxRunes caps the cause text a not-confirmed error quotes.
const notConfirmedCauseMaxRunes = 400

// notConfirmed builds the error for an update whose outcome is unknown.
//
// The cause is rendered as text and deliberately kept OFF the unwrap chain:
// see ErrNotConfirmed. Every cause that reaches here was built by this
// package from code-owned paths and escaped transport text, and it is escaped
// and capped again because it is now quoted rather than wrapped.
func notConfirmed(taskID int, what string, cause error) error {
	const advice = "it may have been applied: read the task before retrying"
	if cause == nil {
		return fmt.Errorf("%w: task %d: %s — %s", ErrNotConfirmed, taskID, what, advice)
	}
	return fmt.Errorf("%w: task %d: %s — %s (cause: %s)", ErrNotConfirmed, taskID, what, advice,
		termsafe.SafeLineMax(cause.Error(), notConfirmedCauseMaxRunes))
}

// closeTask is a task object as a close needs it: the raw object for the
// update body, and the few values this client reads, each decoded on its own.
type closeTask struct {
	raw         map[string]json.RawMessage
	done        bool
	repeating   bool
	description string
	projectID   int
	title       string
}

// decodeCloseTask checks that body is task wantID in the shape a close relies
// on, and returns it. Each check exists because the update is built from this
// object: a wrong type here would otherwise be echoed back to the server as
// this client's own request.
//
// The refusals name a key and what was wrong with it, never a value. Values
// are server text.
func decodeCloseTask(body []byte, wantID int) (closeTask, error) {
	if len(body) > maxCloseBodyBytes {
		return closeTask{}, closeShapeError("is %d bytes, over the %d limit", len(body), maxCloseBodyBytes)
	}
	// Unmarshal into a map accepts `null` and yields a nil map, so the object
	// test is on the first byte.
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return closeTask{}, closeShapeError("is not a JSON object")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return closeTask{}, malformedJSON(fmt.Sprintf("task %d", wantID), err)
	}

	task := closeTask{raw: raw}

	id, ok := rawInt(raw["id"])
	if !ok {
		return closeTask{}, closeShapeError("has no integer id")
	}
	if id != wantID {
		// The update goes to /tasks/{wantID} carrying this object. An object
		// for another task would overwrite wantID with that task's fields.
		return closeTask{}, closeShapeError("is task %d, not the task %d that was asked for", id, wantID)
	}

	if task.done, ok = rawBool(raw["done"]); !ok {
		return closeTask{}, closeShapeError("has a done value that is not true or false")
	}

	if value, present := raw["description"]; present && !rawIsNull(value) {
		if task.description, ok = rawStringValue(value); !ok {
			return closeTask{}, closeShapeError("has a description that is not a string")
		}
	}

	for _, key := range []string{"repeat_after", "repeat_mode"} {
		value, present := raw[key]
		if !present {
			continue
		}
		nonZero, ok := rawNumberNonZero(value)
		if !ok {
			return closeTask{}, closeShapeError("has a %s that is not a number", key)
		}
		task.repeating = task.repeating || nonZero
	}

	// Reported to the caller, never sent: a value of the wrong type is left
	// at its zero value and the object is not refused for it.
	task.projectID, _ = rawInt(raw["project_id"])
	task.title, _ = rawStringValue(raw["title"])
	return task, nil
}

// closeShapeError is the refusal of a 200 response that is not the task
// object a close needs. It carries ErrUnexpectedStatus, like a response that
// does not decode, so a caller's disposition for "the server answered with
// something this client does not understand" covers it.
func closeShapeError(format string, args ...any) error {
	return fmt.Errorf("%w: the task object %s", ErrUnexpectedStatus, fmt.Sprintf(format, args...))
}

func rawIsNull(value json.RawMessage) bool {
	return string(bytes.TrimSpace(value)) == "null"
}

// rawIsNumber reports whether value is a JSON number. value is one element of
// a document that already parsed, so its first byte decides its type.
func rawIsNumber(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && (value[0] == '-' || (value[0] >= '0' && value[0] <= '9'))
}

// rawInt decodes value as an integer literal. A fraction or an exponent is
// refused, not rounded: an id is compared for equality, and 101.0 is not a
// spelling of an id this client will act on.
func rawInt(value json.RawMessage) (int, bool) {
	if !rawIsNumber(value) {
		return 0, false
	}
	n, err := strconv.Atoi(string(bytes.TrimSpace(value)))
	return n, err == nil
}

func rawBool(value json.RawMessage) (b, ok bool) {
	switch string(bytes.TrimSpace(value)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func rawStringValue(value json.RawMessage) (string, bool) {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || value[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", false
	}
	return s, true
}

// rawNumberNonZero reports whether value is a JSON number, and whether it is
// anything other than zero. A number too large to parse is not zero.
func rawNumberNonZero(value json.RawMessage) (nonZero, ok bool) {
	if !rawIsNumber(value) {
		return false, false
	}
	f, err := strconv.ParseFloat(string(bytes.TrimSpace(value)), 64)
	return err != nil || f != 0, true
}

// closeDescription returns the description to send: the existing one, a blank
// line, and the trailer — or the trailer alone when there is no description.
//
// If the existing description's last line is already a closed-by trailer in
// the strict grammar, that one line is replaced. This is what keeps a retry
// from stacking trailers. It is never a reason to skip the append: a trailer
// on the task is text anyone with edit access can write, so a matching line
// is not evidence that this call's record is there.
func closeDescription(existing, trailer string) (string, error) {
	if n := utf8.RuneCountInString(existing); n > maxDescriptionSendRunes {
		return "", fmt.Errorf("%w: the description is already %d characters, over the %d limit",
			ErrTrailerTooLong, n, maxDescriptionSendRunes)
	}
	out := trailer
	if base := withoutClosingTrailer(existing); strings.TrimSpace(base) != "" {
		out = base + "\n\n" + trailer
	}
	if n := utf8.RuneCountInString(out); n > maxDescriptionSendRunes {
		return "", fmt.Errorf("%w: %d characters with the trailer, and the limit is %d",
			ErrTrailerTooLong, n, maxDescriptionSendRunes)
	}
	return out, nil
}

// withoutClosingTrailer removes description's closing line when it is a strict
// closed-by trailer, along with the line breaks around it. Any other
// description is returned unchanged.
//
// Only a line that is the strict grammar from its first byte to its last is
// recognised. A trailer inside markup (`<p>closed-by: …</p>`) is not: telling
// a trailer from text that quotes one would mean parsing the board's markup,
// and a line left in place costs a second trailer, not a wrong record.
func withoutClosingTrailer(description string) string {
	body := withoutTrailingBreaks(description)
	last := lastLine(body)
	if _, ok := parseClosingTrailer(last); !ok {
		return description
	}
	return withoutTrailingBreaks(strings.TrimSuffix(body, last))
}

// closingLine is the last line of a description, not counting line breaks
// after it. A server or an editor that stores a description with a break at
// its end has added no text, so "…<trailer>\n" closes with that trailer. Both
// the replace-on-retry rule and EvidenceRecorded read the description through
// this, so the two cannot disagree about which line is last.
func closingLine(description string) string {
	return lastLine(withoutTrailingBreaks(description))
}

// withoutTrailingBreaks drops every carriage return and line feed at the end
// of s.
func withoutTrailingBreaks(s string) string {
	return strings.TrimRight(s, "\r\n")
}

// lastLine is the text after the final line feed, or all of s when it has
// none.
func lastLine(s string) string {
	return s[strings.LastIndexByte(s, '\n')+1:]
}

// unexpectedChanges compares the pre-read and the read-back outside
// expectedCloseKeys. A key that differs, or is in one object and not the
// other, is returned by name when it is a known task key and counted
// otherwise.
func unexpectedChanges(before, after map[string]json.RawMessage) (named []string, unnamed int) {
	keys := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	for key := range keys {
		if slices.Contains(expectedCloseKeys, key) {
			continue
		}
		was, hadBefore := before[key]
		is, hasAfter := after[key]
		if hadBefore && hasAfter && sameJSONValue(was, is) {
			continue
		}
		if nameableKey.MatchString(key) && slices.Contains(knownTaskKeys, key) {
			named = append(named, key)
		} else {
			unnamed++
		}
	}
	slices.Sort(named)
	return named, unnamed
}

// sameJSONValue reports whether a and b are the same JSON value, ignoring
// whitespace and the order of object keys. Numbers are compared as their
// literal text (UseNumber), so two values that differ only past float64
// precision are still different. The decoded values are compared and dropped;
// nothing decoded here is ever sent.
func sameJSONValue(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	left, errLeft := decodeForCompare(a)
	right, errRight := decodeForCompare(b)
	return errLeft == nil && errRight == nil && reflect.DeepEqual(left, right)
}

func decodeForCompare(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	err := dec.Decode(&value)
	return value, err
}
