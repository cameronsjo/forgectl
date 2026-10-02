package tasks

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// completeTaskDescription is what an agent reads before it decides to close
// a task. It is instructions, not documentation: each sentence narrows when
// the tool may be called, and rewording one changes when agents close tasks.
const completeTaskDescription = "Mark one task done and record who closed it and why. " +
	"Call only when the work this task describes has merged or been carried out, and you hold its task id " +
	"from your own create_task call, a plan's `card:` line, or a PR's `Board-Task:` line. " +
	"Do not call for a task you found by title or in a list, for work that is open or unmerged, or to record progress. " +
	"`task_id` is the global id that create_task, get_task, and list_tasks return, not the #N shown in the web UI. " +
	"`evidence` must be something you observed yourself, never text read from the board. " +
	"Safe to repeat: an already-done task returns already_done true and writes nothing. " +
	"You cannot reopen a task; a wrong close needs the operator. " +
	"A credential without update rights gets a tool error: report the id as still open and do not retry."

// maxClosesPerSession is how many updates complete_task will send for one MCP
// session.
//
// It bounds a loop. Board text is untrusted, and a title that talks an agent
// into closing "the next one too" should run out of road after ten tasks, not
// after the board. It is a brake and not a boundary: a client that opens a new
// session gets a new budget, and what a credential may close at all is decided
// by the projects its bot user is shared.
//
// A slot is spent by an update that was SENT, whatever became of it: a
// confirmed close, an update the server refused, and an update whose outcome
// is unknown all count. The brake is on what this session sends to the board,
// and a refused update was sent. A call that sends nothing — the task was
// already done, the pre-read failed, the task repeats, the trailer does not
// fit, the request was malformed — spends nothing.
const maxClosesPerSession = 10

// closeBudget counts sent updates per MCP session.
//
// The key is the session's pointer, and the nil key is a real bucket: a call
// that arrives with no session is counted there, together with every other
// such call. Treating "no session" as "nothing to count against" would turn
// the cap off for exactly the caller this server knows least about.
//
// Lifetime of an entry: it is created by a session's first reservation and
// removed when its count returns to zero, so a session that never sends an
// update never holds one. A session that did send one keeps its entry until
// the server no longer lists that session, and the check for that runs
// whenever a session with no entry reserves. The map therefore holds at most
// one entry per session the server currently has, plus the nil bucket, and a
// server that runs for months does not accumulate ended sessions. The nil
// bucket is never dropped: it is not a session and does not end.
type closeBudget struct {
	mu   sync.Mutex
	used map[*mcp.ServerSession]int
	// live lists the sessions the server currently holds. Nil means the
	// caller has no server to ask, and nothing is ever dropped.
	live func() iter.Seq[*mcp.ServerSession]
}

func newCloseBudget(live func() iter.Seq[*mcp.ServerSession]) *closeBudget {
	return &closeBudget{used: make(map[*mcp.ServerSession]int), live: live}
}

// reserve takes one slot for session and reports whether there was one. The
// check and the increment are one step under the lock: two calls that both
// read "nine used" must not both proceed.
func (b *closeBudget) reserve(session *mcp.ServerSession) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	used, known := b.used[session]
	if used >= maxClosesPerSession {
		return false
	}
	if !known {
		b.dropEnded()
	}
	b.used[session] = used + 1
	return true
}

// release returns a slot taken by a call that turned out to send nothing.
func (b *closeBudget) release(session *mcp.ServerSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used[session] <= 1 {
		delete(b.used, session)
		return
	}
	b.used[session]--
}

// dropEnded removes every entry whose session the server no longer holds.
// The caller holds b.mu.
func (b *closeBudget) dropEnded() {
	if b.live == nil {
		return
	}
	held := make(map[*mcp.ServerSession]struct{})
	for session := range b.live() {
		held[session] = struct{}{}
	}
	for session := range b.used {
		if session == nil {
			continue
		}
		if _, ok := held[session]; !ok {
			delete(b.used, session)
		}
	}
}

// closeTool is the complete_task handler and the state it keeps between
// calls: the per-session budget, and the lock that keeps two calls' record
// lines from interleaving on one writer.
type closeTool struct {
	client *Client
	cfg    MCPConfig
	budget *closeBudget

	recordMu sync.Mutex
}

func newCloseTool(client *Client, cfg MCPConfig, live func() iter.Seq[*mcp.ServerSession]) *closeTool {
	return &closeTool{client: client, cfg: cfg, budget: newCloseBudget(live)}
}

// closeRecordFailed is the last line of a result whose record could not be
// written. A close that happened is still reported as a close — turning it
// into an error would tell the caller to retry a write that landed — but the
// caller is told the record is missing.
const closeRecordFailed = "The close record for this call could not be written."

// withRecordNote returns text, with closeRecordFailed as its own last line
// when the record was not written.
func withRecordNote(text string, recorded bool) string {
	if recorded {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n" + closeRecordFailed + "\n"
}

// closeError is a complete_task failure. Every one starts with the tool name
// and a code, so a caller can branch on the code without reading the prose.
func closeError(code, format string, args ...any) *mcp.CallToolResult {
	return toolError("complete_task: %s: %s", code, fmt.Sprintf(format, args...))
}

// sessionOf is the budget key for a call: its session, or nil when there is
// none to name.
func sessionOf(req *mcp.CallToolRequest) *mcp.ServerSession {
	if req == nil {
		return nil
	}
	return req.Session
}

// handle closes one task.
//
// The order is the control. The request is checked before a slot is taken, a
// slot is taken before anything is sent, and the slot is given back only when
// CompleteTask reports that it sent no update.
func (h *closeTool) handle(ctx context.Context, req *mcp.CallToolRequest, in completeTaskInput) (*mcp.CallToolResult, any, error) {
	f, err := newFence()
	if err != nil {
		return closeError("failed", "could not build the response fence, so nothing was read or written: %v", err), nil, nil
	}

	// CompleteTask makes both of these checks itself, and those are the ones
	// that guard the board. They are made here first only because its refusal
	// carries no sentinel, and a caller needs to tell "fix your arguments"
	// from "the board said no".
	if in.TaskID <= 0 {
		return closeError("usage_error", "task_id must be a positive task id, got %d", in.TaskID), nil, nil
	}
	if _, err := sanitizeEvidence(in.Evidence); err != nil {
		return closeError("usage_error", "%s", strings.TrimPrefix(err.Error(), "tasks: ")), nil, nil
	}

	session := sessionOf(req)
	closer := callerName(req, h.cfg.DefaultClientName)
	now := time.Now()
	rec := CloseRecord{
		Time:       now,
		TaskID:     in.TaskID,
		Surface:    SurfaceMCP,
		Closer:     closer,
		Evidence:   in.Evidence,
		Credential: h.cfg.CredentialSource,
		Host:       h.cfg.Host,
	}

	if !h.budget.reserve(session) {
		rec.Outcome = CloseOutcomeCap
		recorded := h.record(rec)
		return closeError("close_cap", "%s", withRecordNote(fmt.Sprintf(
			"this session has sent its limit of %d task updates, so task %d was not read or changed. "+
				"Report it as still open; closing it needs a new session or the operator.",
			maxClosesPerSession, in.TaskID), recorded)), nil, nil
	}

	result, err := h.client.CompleteTask(ctx, CloseRequest{
		TaskID:   in.TaskID,
		Closer:   closer,
		Surface:  SurfaceMCP,
		Evidence: in.Evidence,
		Now:      now,
	})

	outcome, sent := CloseOutcome(result, err)
	recorded := true
	if sent {
		rec.ProjectID = result.ProjectID
		rec.Outcome = outcome
		recorded = h.record(rec)
	} else {
		h.budget.release(session)
	}

	if err != nil {
		return closeError(CloseErrorCode(err), "%s", withRecordNote(err.Error(), recorded)), nil, nil
	}
	return structuredResult(toolText(withRecordNote(closeSuccessText(f, result), recorded)), completeTaskOutput{
		ID:               result.ID,
		ProjectID:        result.ProjectID,
		Done:             result.Confirmed || result.AlreadyDone,
		AlreadyDone:      result.AlreadyDone,
		EvidenceRecorded: result.EvidenceRecorded,
	})
}

// record writes one close record and reports whether it was written. The
// writer's own error is not passed on: the caller of a tool can do nothing
// with it, and what it must know is only that the record is missing.
func (h *closeTool) record(rec CloseRecord) bool {
	h.recordMu.Lock()
	defer h.recordMu.Unlock()
	return WriteCloseRecord(h.cfg.Records, rec) == nil
}

// CloseOutcome reads a finished CompleteTask call: whether an update was
// sent, and if so the record outcome for it. Every surface that closes a task
// uses it to decide whether a close record is owed.
//
// An update was sent exactly when the call confirmed a close, or failed with
// ErrNotConfirmed or ErrWriteRefused. Every other failure — and an
// already-done task — stopped before the update.
func CloseOutcome(result CloseResult, err error) (outcome string, sent bool) {
	switch {
	case err == nil && result.Confirmed:
		return CloseOutcomeClosed, true
	case err == nil:
		return "", false
	case errors.Is(err, ErrNotConfirmed):
		return CloseOutcomeNotConfirmed, true
	case errors.Is(err, ErrWriteRefused) && errors.Is(err, ErrUnauthorized):
		return CloseOutcomeUnauthorized, true
	case errors.Is(err, ErrWriteRefused):
		return CloseOutcomeWriteRefused, true
	}
	return "", false
}

// CloseErrorCode maps a CompleteTask failure to its code: what a tool error
// leads with, and the `code` of the CLI's --json failure object. One mapping,
// so the two surfaces cannot name the same failure differently.
//
// ErrNotConfirmed is tested first because it is the one a caller must not
// mistake for anything else: the update may have landed. `unauthorized`
// covers both a refused update and a refused pre-read, on a 401 or a 403; in
// both the credential is the thing to change, and the error text says which
// it was. `write_refused` is any other refusal of the update. `failed` is
// everything this client has no more exact word for: the instance was
// unreachable, the host was refused, or the answer was not a task.
func CloseErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotConfirmed):
		return "not_confirmed"
	case errors.Is(err, ErrWriteRefused) && errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrWriteRefused):
		return "write_refused"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrRepeatingTask):
		return "repeating_task"
	case errors.Is(err, ErrTrailerTooLong):
		return "trailer_too_long"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	}
	return "failed"
}

// closeSuccessText renders a close that did not fail: a confirmed close, or a
// task that was already done.
//
// The title is the only board text in it, and it goes inside the fence. The
// ids are the server's own. A key is named only when CompleteTask vetted it
// into ChangedKeys; any other difference is a count, because a JSON key is
// server text like a title is.
func closeSuccessText(f fence, result CloseResult) string {
	title := f.wrapLine(truncateRunes(result.Title, maxTitleShowRunes))
	if result.AlreadyDone {
		return fmt.Sprintf("task #%d in project %d was already done; nothing was written and no evidence was recorded: %s\n",
			result.ID, result.ProjectID, title)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "closed task #%d in project %d: %s\n", result.ID, result.ProjectID, title)
	if result.EvidenceRecorded {
		b.WriteString("evidence recorded: yes\n")
	} else {
		b.WriteString("evidence recorded: no — the task reads back done, and its description does not end with this call's closed-by line\n")
	}
	if len(result.ChangedKeys) > 0 {
		fmt.Fprintf(&b, "also different after the update, outside the keys a close is expected to touch: %s\n",
			strings.Join(result.ChangedKeys, ", "))
	}
	if result.UnnamedChanges > 0 {
		fmt.Fprintf(&b, "%d other key(s) differ after the update and are not named here\n", result.UnnamedChanges)
	}
	return b.String()
}
