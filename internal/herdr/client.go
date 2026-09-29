package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Binary is the herdr executable the client runs.
const Binary = "herdr"

// Client talks to the herdr session the process runs in.
type Client struct {
	runner exec.Runner
}

// New returns a Client over r. It performs no session check; see [Probe].
func New(r exec.Runner) *Client {
	return &Client{runner: r}
}

// Workspaces lists every workspace in the session.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	r, err := read[struct {
		Workspaces []Workspace `json:"workspaces"`
	}](ctx, c, "workspace", "list")
	return r.Workspaces, err
}

// Tabs lists the tabs of one workspace.
func (c *Client) Tabs(ctx context.Context, workspaceID string) ([]Tab, error) {
	if err := checkID("workspace id", workspaceID); err != nil {
		return nil, err
	}
	r, err := read[struct {
		Tabs []Tab `json:"tabs"`
	}](ctx, c, "tab", "list", "--workspace", workspaceID)
	return r.Tabs, err
}

// Panes lists every pane in the session.
func (c *Client) Panes(ctx context.Context) ([]Pane, error) {
	r, err := read[struct {
		Panes []Pane `json:"panes"`
	}](ctx, c, "pane", "list")
	return r.Panes, err
}

// Agents lists every pane that hosts a recognized coding agent.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	r, err := read[struct {
		Agents []Agent `json:"agents"`
	}](ctx, c, "agent", "list")
	return r.Agents, err
}

// PaneGet fetches one pane by id.
func (c *Client) PaneGet(ctx context.Context, paneID string) (Pane, error) {
	if err := checkID("pane id", paneID); err != nil {
		return Pane{}, err
	}
	r, err := read[struct {
		Pane Pane `json:"pane"`
	}](ctx, c, "pane", "get", paneID)
	return r.Pane, err
}

// TabGet fetches one tab by id.
func (c *Client) TabGet(ctx context.Context, tabID string) (Tab, error) {
	if err := checkID("tab id", tabID); err != nil {
		return Tab{}, err
	}
	r, err := read[struct {
		Tab Tab `json:"tab"`
	}](ctx, c, "tab", "get", tabID)
	return r.Tab, err
}

// ReadPane returns the pane's text. Unlike every other call, `pane read`
// prints raw terminal text on success (measured), so the result is returned
// as-is and only the failure path carries herdr's JSON envelope. lines <= 0
// leaves herdr's default.
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

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	out, err := c.runner.Run(ctx, Binary, args...)
	if err != nil {
		return "", classify(args, err)
	}
	return out, nil
}

// read runs a herdr command and decodes the "result" member of its
// {"id","result"} envelope into T. The envelope's id is ignored, unknown
// fields are tolerated, and a missing or null result fails closed.
func read[T any](ctx context.Context, c *Client, args ...string) (T, error) {
	var zero T
	out, err := c.run(ctx, args...)
	if err != nil {
		return zero, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return zero, fmt.Errorf("herdr %s: decode envelope: %w", strings.Join(args, " "), err)
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return zero, fmt.Errorf("herdr %s: response has no result", strings.Join(args, " "))
	}
	var v T
	if err := json.Unmarshal(env.Result, &v); err != nil {
		return zero, fmt.Errorf("herdr %s: decode result: %w", strings.Join(args, " "), err)
	}
	return v, nil
}

// checkID refuses an empty id or one that herdr would parse as a flag.
func checkID(what, id string) error {
	if id == "" {
		return errors.New("herdr: empty " + what)
	}
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("herdr: %s %q starts with '-'", what, id)
	}
	return nil
}
