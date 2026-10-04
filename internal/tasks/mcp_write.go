package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxBoardWritesPerSession is how many writes create_task and add_comment
// together will send for one MCP session.
//
// It bounds a loop, as maxClosesPerSession does. Board text is untrusted, and
// a description that talks an agent into filing "one more" task, or a comment
// on every task it reads, should run out of road after a session's worth of
// real work and not after the board. Twenty is that: an agent filing the
// slices of one plan and commenting on a few of them stays well under it, and
// a runaway loop stops at twenty rows or twenty comments of at most
// maxCommentRunes each, which an operator can clear by hand. The two tools
// share one count because the harm is the same — rows of text on a shared
// board — and a loop that alternates between them must not get twice the road.
//
// It is a brake and not a boundary: a client that opens a new session gets a
// new budget, and what a credential may write at all is decided by the
// projects its bot user is shared. The count is separate from the close cap,
// so a session that has filed its tasks can still close them.
//
// A slot is spent by a write that was SENT, whatever became of it, or that may
// have been sent. A call that sends nothing — refused for its own arguments,
// or stopped by its pre-read — spends nothing.
const maxBoardWritesPerSession = 20

// writeRecordFailed is the last line of a create_task or add_comment result
// whose record could not be written. A write that happened is still reported
// as one; turning it into an error would tell the caller to retry a write that
// landed.
const writeRecordFailed = "The write record for this call could not be written."

// boardWriteTool is the create_task and add_comment handlers and the state
// they share between calls: one per-session budget across both tools.
type boardWriteTool struct {
	client *Client
	cfg    MCPConfig
	budget *sessionBudget
}

func newBoardWriteTool(client *Client, cfg MCPConfig, live func() iter.Seq[*mcp.ServerSession]) *boardWriteTool {
	return &boardWriteTool{client: client, cfg: cfg, budget: newSessionBudget(maxBoardWritesPerSession, live)}
}

// boardWriteOutcome reads a finished CreateTask or AddComment call that got
// past the handler's own checks: whether a write may have been sent, and if
// so the record outcome for it.
//
// The handler makes every local check those methods make before it calls
// them, so a failure here comes from the request path. Of those, only a host
// refusal is known to have sent nothing: the dialer refused the address
// before a byte left. Every other failure is counted as sent. A 401 or 403
// and any other status are the board's answer to a write it received. A
// transport failure or an answer that does not decode may come after the
// board took the write, and nothing here can tell — so it is not_confirmed,
// and it spends its slot, because the cap must not be widened by failures
// this client cannot see through.
func boardWriteOutcome(err error) (outcome string, sent bool) {
	var status statusError
	switch {
	case err == nil:
		return BoardWriteOutcomeWritten, true
	case IsHostRefused(err):
		return "", false
	case errors.Is(err, ErrUnauthorized):
		return BoardWriteOutcomeUnauthorized, true
	case errors.As(err, &status):
		return BoardWriteOutcomeWriteRefused, true
	}
	return BoardWriteOutcomeNotConfirmed, true
}

// capRefusal is the tool error for a call past the write cap. It names the
// tool and the code first, like every complete_task failure, so a caller can
// branch on the code without reading the prose.
func capRefusal(tool, what string, recorded bool) *mcp.CallToolResult {
	return toolError("%s: write_cap: %s", tool, withNote(fmt.Sprintf(
		"this session has %d board writes sent or in flight across create_task and add_comment, which is its limit, so %s. "+
			"Writing it is now the operator's call.",
		maxBoardWritesPerSession, what), writeRecordFailed, recorded))
}

// createTask files one task.
//
// The order is the control, as in complete_task. Everything the caller sent
// is checked before a slot is taken, a slot is taken before the pre-read, and
// the slot is given back only when no write was sent.
func (h *boardWriteTool) createTask(ctx context.Context, req *mcp.CallToolRequest, in createTaskInput) (*mcp.CallToolResult, any, error) {
	f, errResult := fenceOrError()
	if errResult != nil {
		return errResult, nil, nil
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return toolError("create_task: title is required and must not be blank"), nil, nil
	}
	if err := checkTitle(title); err != nil {
		return toolError("create_task: %s", strings.TrimPrefix(err.Error(), "tasks: create: ")), nil, nil
	}
	// Bound the CALLER's description against the caller's own budget, before
	// the trailer is appended. CreateTask's bound runs on the combined text,
	// so leaving this to it reports a length that includes the server's
	// trailer — a refusal that names a number the caller cannot reconcile
	// with what it sent.
	if n := len([]rune(in.Description)); n > maxDescriptionSendRunes-maxTrailerRunes {
		return toolError("create_task: description is %d characters, over the %d limit (the remaining %d are reserved for the created-by trailer)",
			n, maxDescriptionSendRunes-maxTrailerRunes, maxTrailerRunes), nil, nil
	}
	caller := callerName(req, h.cfg.DefaultClientName)
	description, err := createDescription(in.Description, caller)
	if err != nil {
		return toolError("create_task: %v", err), nil, nil
	}

	session := sessionOf(req)
	rec := BoardWriteRecord{
		Time:       time.Now(),
		Event:      BoardWriteEventTaskCreated,
		Caller:     caller,
		Credential: h.cfg.CredentialSource,
		Host:       h.cfg.Host,
	}
	if !h.budget.reserve(session) {
		rec.Outcome = BoardWriteOutcomeCap
		return capRefusal("create_task", fmt.Sprintf("no task was filed in project %d", in.ProjectID), h.record(rec)), nil, nil
	}

	// Pre-read, fail closed. A project this credential cannot READ is one
	// it must not write to, and the write's own 401 cannot distinguish
	// "out of scope" from "revoked token" (ADR 0009). A passing read is
	// what makes the write's outcome mean anything.
	if _, err := h.client.FetchProject(ctx, in.ProjectID); err != nil {
		h.budget.release(session)
		return toolError("create_task: refusing to write to project %d because the pre-read of that project failed: %v",
			in.ProjectID, err), nil, nil
	}
	created, err := h.client.CreateTask(ctx, in.ProjectID, title, description)
	outcome, sent := boardWriteOutcome(err)
	if !sent {
		h.budget.release(session)
		return toolError("create_task: %v", err), nil, nil
	}
	rec.ProjectID, rec.Outcome = in.ProjectID, outcome
	if err == nil {
		rec.TaskID = created.ID
	}
	recorded := h.record(rec)
	if err != nil {
		return toolError("create_task: %s", withNote(err.Error(), writeRecordFailed, recorded)), nil, nil
	}
	// The id is what a caller later hands to complete_task, so it is
	// returned as a number too and not only inside a sentence.
	return structuredResult(toolText(withNote(fmt.Sprintf("created task #%d in project %d: %s",
		created.ID, in.ProjectID, f.wrapLine(truncateRunes(created.Title, maxTitleShowRunes))), writeRecordFailed, recorded)),
		createTaskOutput{ID: created.ID, ProjectID: in.ProjectID})
}

// addComment adds one comment, in the order createTask keeps.
func (h *boardWriteTool) addComment(ctx context.Context, req *mcp.CallToolRequest, in addCommentInput) (*mcp.CallToolResult, any, error) {
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return toolError("add_comment: body is required and must not be blank"), nil, nil
	}
	if err := checkComment(body); err != nil {
		return toolError("add_comment: %s", strings.TrimPrefix(err.Error(), "tasks: comment: ")), nil, nil
	}

	session := sessionOf(req)
	rec := BoardWriteRecord{
		Time:       time.Now(),
		Event:      BoardWriteEventCommentAdded,
		TaskID:     in.TaskID,
		Caller:     callerName(req, h.cfg.DefaultClientName),
		Credential: h.cfg.CredentialSource,
		Host:       h.cfg.Host,
	}
	if !h.budget.reserve(session) {
		rec.Outcome = BoardWriteOutcomeCap
		return capRefusal("add_comment", fmt.Sprintf("no comment was added to task %d", in.TaskID), h.record(rec)), nil, nil
	}

	// The same fail-closed pre-read create_task performs, for the same
	// reason: a task this credential cannot READ is one it must not
	// comment on, and the write's own 401 cannot distinguish "out of
	// scope" from "revoked token". Applying the rule to only one of two
	// write verbs would leave the ADR stating a general decision that the
	// code half-keeps.
	task, err := h.client.FetchTask(ctx, in.TaskID)
	if err != nil {
		h.budget.release(session)
		return toolError("add_comment: refusing to comment on task %d because the pre-read of that task failed: %v",
			in.TaskID, err), nil, nil
	}
	comment, err := h.client.AddComment(ctx, in.TaskID, body)
	outcome, sent := boardWriteOutcome(err)
	if !sent {
		h.budget.release(session)
		return toolError("add_comment: %v", err), nil, nil
	}
	rec.ProjectID, rec.Outcome = task.ProjectID, outcome
	recorded := h.record(rec)
	if err != nil {
		return toolError("add_comment: %s", withNote(err.Error(), writeRecordFailed, recorded)), nil, nil
	}
	return toolText(withNote(fmt.Sprintf("added comment #%d to task #%d", comment.ID, in.TaskID), writeRecordFailed, recorded)), nil, nil
}

// record writes one board-write line and reports whether it was written. As
// with a close record, the writer's own error is not passed on: the caller of
// a tool can do nothing with it.
func (h *boardWriteTool) record(rec BoardWriteRecord) bool {
	return WriteBoardWriteRecord(h.cfg.Records, rec) == nil
}

// serialWriter makes each Write to w one step under a lock. Every tool on a
// server writes its record lines to the same MCPConfig.Records, from calls
// that run at once, and a writer such as a buffer or a fan-out to several
// sinks is not safe for that by itself.
type serialWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *serialWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
