package pr

import (
	"os"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
)

// defaultTmuxSession is the tmux session the review windows live under. A
// window is targeted as "<session>:pr-<owner>-<N>". Overridable for tests via
// WithTmuxSession.
const defaultTmuxSession = "forgectl"

// Client is the ops-layer entry point for `forgectl pr`. It drives gh, git,
// and tmux through the exec.Runner seam and gates every review-post behind an
// injectable human approval function.
type Client struct {
	run exec.Runner

	// sessionsDir is the forgectl-owned breadcrumb directory
	// (config.PrSessionsDir); the breadcrumb location check enforces that a
	// loaded path resolves to inside it. Injectable for tests.
	sessionsDir string

	// findingsDir is the forgectl-owned directory (config.PrFindingsDir) that
	// holds `forgectl pr local` findings — the deliverable of a local
	// clean-room review, which must outlive the disposable workspace.
	// Injectable for tests.
	findingsDir string

	// tmuxSession is the session under which review windows are created.
	tmuxSession string

	// approve is the human approval gate. It receives the drafted review and
	// returns whether a post is authorized. No review-post argv reaches the
	// Runner unless approve returns true. Defaults to a huh confirm; tests
	// inject a deterministic decision.
	approve func(review string) (bool, error)

	// isTTY reports whether an interactive gate can be shown. When false (or
	// --headless), the post path stages only and never auto-posts.
	isTTY func() bool
}

// Option configures a Client at construction.
type Option func(*Client)

// WithSessionsDir overrides the breadcrumb directory (default:
// config.PrSessionsDir()) — used in tests to point at a temp dir.
func WithSessionsDir(dir string) Option {
	return func(c *Client) { c.sessionsDir = dir }
}

// WithFindingsDir overrides the local-review findings directory (default:
// config.PrFindingsDir()) — used in tests to point at a temp dir.
func WithFindingsDir(dir string) Option {
	return func(c *Client) { c.findingsDir = dir }
}

// WithTmuxSession overrides the tmux session review windows are created under.
func WithTmuxSession(name string) Option {
	return func(c *Client) { c.tmuxSession = name }
}

// WithApprover overrides the human approval gate — used in tests to supply a
// deterministic approve/deny without a TTY.
func WithApprover(fn func(review string) (bool, error)) Option {
	return func(c *Client) { c.approve = fn }
}

// WithTTYCheck overrides the interactive-TTY detection — used in tests.
func WithTTYCheck(fn func() bool) Option {
	return func(c *Client) { c.isTTY = fn }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:         run,
		tmuxSession: defaultTmuxSession,
		approve:     confirmReview,
		isTTY:       launch.IsInteractiveTTY,
	}
	if dir, err := config.PrSessionsDir(); err == nil {
		c.sessionsDir = dir
	}
	if dir, err := config.PrFindingsDir(); err == nil {
		c.findingsDir = dir
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SessionsDir returns the resolved breadcrumb directory.
func (c *Client) SessionsDir() string { return c.sessionsDir }

// FindingsDir returns the resolved local-review findings directory.
func (c *Client) FindingsDir() string { return c.findingsDir }

// tempPrefix is the os.MkdirTemp prefix sandbox uses for every workspace
// ("forgectl-workflow-*"); the breadcrumb content check requires a workspace
// to live under the OS temp dir with the "forgectl-" prefix.
const tempPrefix = "forgectl-"

// osTempDir is a seam over os.TempDir so the breadcrumb content check is
// testable against a redirected temp root.
var osTempDir = os.TempDir
