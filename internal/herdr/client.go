package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Binary is the herdr executable the client runs.
const Binary = "herdr"

// maxIDLen bounds an id or label passed as a herdr operand. Real ids are a
// dozen bytes; the bound only stops a runaway value reaching the argv.
const maxIDLen = 256

// Client talks to the herdr session the process runs in.
type Client struct {
	runner exec.Runner
}

// New returns a Client over r. It performs no session check; see [CheckSession]
// and [CheckFork].
func New(r exec.Runner) *Client {
	return &Client{runner: r}
}

// Workspaces lists every workspace in the session.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	args := []string{"workspace", "list"}
	r, err := read[struct {
		Workspaces *[]Workspace `json:"workspaces"`
	}](ctx, c, args...)
	if err != nil {
		return nil, err
	}
	return need(r.Workspaces, "workspaces", args)
}

// Tabs lists the tabs of one workspace.
func (c *Client) Tabs(ctx context.Context, workspaceID string) ([]Tab, error) {
	if err := checkID("workspace id", workspaceID); err != nil {
		return nil, err
	}
	args := []string{"tab", "list", "--workspace", workspaceID}
	r, err := read[struct {
		Tabs *[]Tab `json:"tabs"`
	}](ctx, c, args...)
	if err != nil {
		return nil, err
	}
	return need(r.Tabs, "tabs", args)
}

// Panes lists every pane in the session. Each pane carries its tab_id and
// terminal_id, which is how a caller finds a pane again after a move.
func (c *Client) Panes(ctx context.Context) ([]Pane, error) {
	args := []string{"pane", "list"}
	r, err := read[struct {
		Panes *[]Pane `json:"panes"`
	}](ctx, c, args...)
	if err != nil {
		return nil, err
	}
	return need(r.Panes, "panes", args)
}

// Agents lists every pane that hosts a recognized coding agent.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	args := []string{"agent", "list"}
	r, err := read[struct {
		Agents *[]Agent `json:"agents"`
	}](ctx, c, args...)
	if err != nil {
		return nil, err
	}
	return need(r.Agents, "agents", args)
}

// PaneGet fetches one pane by id.
func (c *Client) PaneGet(ctx context.Context, paneID string) (Pane, error) {
	if err := checkID("pane id", paneID); err != nil {
		return Pane{}, err
	}
	args := []string{"pane", "get", paneID}
	r, err := read[struct {
		Pane *Pane `json:"pane"`
	}](ctx, c, args...)
	if err != nil {
		return Pane{}, err
	}
	return need(r.Pane, "pane", args)
}

// TabGet fetches one tab by id.
func (c *Client) TabGet(ctx context.Context, tabID string) (Tab, error) {
	if err := checkID("tab id", tabID); err != nil {
		return Tab{}, err
	}
	args := []string{"tab", "get", tabID}
	r, err := read[struct {
		Tab *Tab `json:"tab"`
	}](ctx, c, args...)
	if err != nil {
		return Tab{}, err
	}
	return need(r.Tab, "tab", args)
}

// ReadPane returns the pane's text. Unlike every other call, `pane read`
// prints raw terminal text on success (measured), so there is no envelope to
// decode; only the failure path carries herdr's JSON error. The text is what
// the runner captured: [exec.Runner] trims trailing newlines, so trailing
// blank terminal rows are not preserved. lines <= 0 leaves herdr's default.
//
// The text is whatever another pane displays: it can hold secrets typed or
// printed there, and terminal control sequences. Do not log it or render it to
// a terminal unfiltered.
func (c *Client) ReadPane(ctx context.Context, paneID string, src ReadSource, lines int) (string, error) {
	if err := checkID("pane id", paneID); err != nil {
		return "", err
	}
	if !src.valid() {
		return "", fmt.Errorf("herdr: unknown read source %q; want visible, recent, recent-unwrapped, or detection", src)
	}
	args := []string{"pane", "read", paneID, "--source", string(src), "--format", "text"}
	if lines > 0 {
		args = append(args, "--lines", strconv.Itoa(lines))
	}
	return c.run(ctx, args...)
}

// act runs a mutation whose reply is not decoded, after checking the id it
// names. The id is passed separately so it is validated once, in one place.
func (c *Client) act(ctx context.Context, what, id string, args ...string) error {
	if err := checkID(what, id); err != nil {
		return err
	}
	_, err := c.run(ctx, args...)
	return err
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	out, err := c.runner.Run(ctx, Binary, args...)
	if err != nil {
		return "", classify(args, err)
	}
	return out, nil
}

// read runs a herdr command and decodes the "result" member of its
// {"id","result"} envelope into T in one pass. The envelope's id is ignored,
// unknown fields are tolerated, and a missing or null result fails closed.
// Fields that must be present are pointers in T; see [need].
func read[T any](ctx context.Context, c *Client, args ...string) (T, error) {
	var zero T
	out, err := c.run(ctx, args...)
	if err != nil {
		return zero, err
	}
	var env struct {
		Result *T `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return zero, fmt.Errorf("herdr %s: decode response: %w", argvText(args), err)
	}
	if env.Result == nil {
		return zero, fmt.Errorf("herdr %s: response has no result", argvText(args))
	}
	return *env.Result, nil
}

// need returns *p, or an error when the member was absent or null. An empty
// list is present (`[]`); a renamed or missing key is not, and must not read
// as an empty session.
func need[P any](p *P, key string, args []string) (P, error) {
	if p == nil {
		var zero P
		return zero, fmt.Errorf("herdr %s: response has no %q", argvText(args), key)
	}
	return *p, nil
}

// checkID refuses an empty id, one that herdr would parse as a flag, one with
// a control character, and one longer than [maxIDLen].
func checkID(what, id string) error {
	if id == "" {
		return errors.New("herdr: empty " + what)
	}
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("herdr: %s %q starts with '-'", what, id)
	}
	if len(id) > maxIDLen {
		return fmt.Errorf("herdr: %s is %d bytes, over the %d limit", what, len(id), maxIDLen)
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return fmt.Errorf("herdr: %s %q has a control character", what, id)
	}
	return nil
}
