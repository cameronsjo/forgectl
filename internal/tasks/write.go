package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// maxTitleRunes and maxCommentRunes bound what this client will SEND. The
// board is the estate's shared surface and an agent is the caller, so an
// unbounded title is a way for one confused tool call to make a row nobody
// can read past. The limits are generous relative to any real task.
const (
	maxTitleRunes   = 500
	maxCommentRunes = 10_000
	// maxDescriptionSendRunes bounds a description on the WRITE path. The
	// separate read-path truncation (maxDescriptionRunes in mcp.go) is about
	// what an agent is shown; this is about what reaches the board.
	maxDescriptionSendRunes = 20_000
)

// Comment is the subset of Vikunja's task-comment shape this client reads
// back after a write. Comment text is UNTRUSTED INPUT on the way out, exactly
// like a task Title — a caller returning it to an agent must fence it.
type Comment struct {
	ID      int    `json:"id"`
	Comment string `json:"comment"`
}

// unauthorizedOnWrite is appended to a 401/403 raised by a write. It is the
// status an out-of-scope write gets AND the status a dead token gets on
// everything (ADR 0009), and the difference is not visible from here — a
// caller reading only "unauthorized" will otherwise conclude the credential is
// revoked when its scope is simply narrower than the call.
const unauthorizedOnWrite = " (an out-of-scope write and a revoked token both answer this; a passing READ is what tells them apart)"

// put performs one bounded, redacting PUT against path with a JSON body,
// through the same credentialed request path every read uses (Client.do), so
// the deadline, the auth header, the host-pin escape hatch, the bounded read,
// and the status-before-decode assertion cannot drift between verbs.
//
// PUT, not POST, is deliberate and is Vikunja's own shape: PUT
// /projects/{id}/tasks creates a task, and POST /tasks/{id} updates one. A
// client that reached for POST here would be issuing an UPDATE against a
// route that does not exist.
func (c *Client) put(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.do(ctx, http.MethodPut, path, nil, payload, unauthorizedOnWrite)
}

// CreateTask creates a task in projectID via PUT /projects/{id}/tasks and
// returns the created task as the server reports it.
//
// A blank title is refused LOCALLY, before any request: Vikunja accepts one
// and produces an untitled row, which is a mess only a human can clear from
// the UI. The bounds below are refusals too, not truncations — silently
// shortening a caller's title would put something on the shared board that
// nobody asked for.
func (c *Client) CreateTask(ctx context.Context, projectID int, title, description string) (Task, error) {
	if projectID <= 0 {
		return Task{}, fmt.Errorf("tasks: create: project_id must be a positive project id, got %d", projectID)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, fmt.Errorf("tasks: create: title is required and must not be blank")
	}
	if n := len([]rune(title)); n > maxTitleRunes {
		return Task{}, fmt.Errorf("tasks: create: title is %d characters, over the %d limit", n, maxTitleRunes)
	}
	if n := len([]rune(description)); n > maxDescriptionSendRunes {
		return Task{}, fmt.Errorf("tasks: create: description is %d characters, over the %d limit", n, maxDescriptionSendRunes)
	}

	payload := map[string]any{"title": title}
	if description != "" {
		payload["description"] = description
	}
	body, err := c.put(ctx, fmt.Sprintf("/projects/%d/tasks", projectID), payload)
	if err != nil {
		return Task{}, err
	}
	var created Task
	if err := json.Unmarshal(body, &created); err != nil {
		return Task{}, fmt.Errorf("%w: create in project %d: decode response: %v", ErrUnexpectedStatus, projectID, err)
	}
	return created, nil
}

// AddComment posts a comment on taskID via PUT /tasks/{id}/comments.
func (c *Client) AddComment(ctx context.Context, taskID int, comment string) (Comment, error) {
	if taskID <= 0 {
		return Comment{}, fmt.Errorf("tasks: comment: task_id must be a positive task id, got %d", taskID)
	}
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return Comment{}, fmt.Errorf("tasks: comment: body is required and must not be blank")
	}
	if n := len([]rune(comment)); n > maxCommentRunes {
		return Comment{}, fmt.Errorf("tasks: comment: body is %d characters, over the %d limit", n, maxCommentRunes)
	}

	body, err := c.put(ctx, fmt.Sprintf("/tasks/%d/comments", taskID), map[string]any{"comment": comment})
	if err != nil {
		return Comment{}, err
	}
	var created Comment
	if err := json.Unmarshal(body, &created); err != nil {
		return Comment{}, fmt.Errorf("%w: comment on task %d: decode response: %v", ErrUnexpectedStatus, taskID, err)
	}
	return created, nil
}

// FetchTask returns one task by id via GET /tasks/{id}.
func (c *Client) FetchTask(ctx context.Context, taskID int) (Task, error) {
	if taskID <= 0 {
		return Task{}, fmt.Errorf("tasks: get: task_id must be a positive task id, got %d", taskID)
	}
	body, err := c.get(ctx, fmt.Sprintf("/tasks/%d", taskID), nil)
	if err != nil {
		return Task{}, err
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return Task{}, fmt.Errorf("%w: task %d: decode response: %v", ErrUnexpectedStatus, taskID, err)
	}
	return task, nil
}

// FetchProject returns one project by id via GET /projects/{id}. It is the
// pre-read create_task performs before a write: a project the credential
// cannot even read is one it must not write to, and finding that out from the
// write's own 401 is indistinguishable from a revoked token.
func (c *Client) FetchProject(ctx context.Context, projectID int) (Project, error) {
	if projectID <= 0 {
		return Project{}, fmt.Errorf("tasks: project: project_id must be a positive project id, got %d", projectID)
	}
	body, err := c.get(ctx, fmt.Sprintf("/projects/%d", projectID), nil)
	if err != nil {
		return Project{}, err
	}
	var project Project
	if err := json.Unmarshal(body, &project); err != nil {
		return Project{}, fmt.Errorf("%w: project %d: decode response: %v", ErrUnexpectedStatus, projectID, err)
	}
	return project, nil
}

// AssertVikunja proves the host is a Vikunja API and not something that
// answers 200 to everything.
//
// The failure this exists for is loud in its consequences and silent in its
// symptoms: the estate's own reverse proxy answers an unmatched host with a
// 200 and an HTML meta-refresh, so a mis-pointed client gets a clean status
// code, a clean TLS handshake, and a decode error it will read as "the API
// changed". Asserting a `version` field in a JSON body from /api/v1/info is
// the cheapest thing that cannot pass against a landing page.
//
// It is called at startup, before the server accepts a single tool call, so
// the operator learns from a refusal to start rather than from an agent's
// confusing tool error an hour later.
func (c *Client) AssertVikunja(ctx context.Context) error {
	body, err := c.get(ctx, "/info", nil)
	if err != nil {
		return err
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("%w: %s/info did not return JSON with a version field — this host is answering with something that is not the Vikunja API",
			ErrUnexpectedStatus, c.baseURL)
	}
	if strings.TrimSpace(info.Version) == "" {
		return fmt.Errorf("%w: %s/info returned JSON with no version field — this host is answering with something that is not the Vikunja API",
			ErrUnexpectedStatus, c.baseURL)
	}
	return nil
}
