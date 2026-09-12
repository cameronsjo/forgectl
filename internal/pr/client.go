package pr

import (
	"context"
	"os"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/sandbox"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// defaultTmuxSession is the tmux session the review windows live under. A
// window is targeted as "<session>:pr-<owner>-<N>". Overridable for tests via
// WithTmuxSession.
const defaultTmuxSession = "forgectl"

// Client is the ops-layer entry point for `forgectl pr`. It drives gh, git,
// and tmux through the exec.Runner seam and gates every review-post behind an
// injectable human approval function.
type Client struct {
	run        exec.Runner
	tmuxClient *tmux.Client

	// sessionsDir is the forgectl-owned breadcrumb directory
	// (config.PrSessionsDir); the breadcrumb location check enforces that a
	// loaded path resolves to inside it. Injectable for tests.
	sessionsDir string

	// fs is the record writer's filesystem seam (record.go). Production is the
	// real filesystem; tests inject a fault-and-log double.
	fs recordFS

	// lockWait bounds withLifecycleLock's wait for the cross-process lifecycle
	// lock (lifecycle_unix.go). Zero means the default; tests shorten it.
	//
	// The lifecycle lock replaced the former in-process sessionsMu: every site
	// that serialized record reads and writes now serializes them across
	// processes too. It is still NOT protection against a hostile same-uid
	// writer — see the residual note on discardStale.
	lockWait time.Duration

	// onLock, when non-nil, is called with ("acquire"|"release") around every
	// successful lifecycle-lock hold. It exists so an in-package test can prove
	// a composite verb takes ONE hold and does no long work inside it — the
	// ordering property the crash-safety design rests on, which is otherwise
	// invisible from outside. Never set in production.
	onLock func(verb, event string)

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

	// confirmRemoval is the gate on a DESTRUCTIVE repair. It is a separate seam
	// from approve on purpose: approve authorizes posting a review, and its huh
	// form asks "Post this review to the PR?" — a question whose yes must never
	// have meant "delete this clean room". Sharing one field also meant a caller
	// wiring an auto-approver for posting silently auto-approved deletions.
	// Defaults to a huh confirm naming the removal; tests inject a decision.
	confirmRemoval func(prompt string) (bool, error)

	// approvalTheme styles the default gate's huh form. It is a separate field
	// rather than a captured value because New installs the default approver
	// before options run, so a theme supplied by an option would arrive too
	// late to be closed over. Zero value is Artificer dark, which is what the
	// gate rendered before it was themed at all.
	approvalTheme theme.Theme

	// isTTY reports whether an interactive gate can be shown. When false (or
	// --headless), the post path stages only and never auto-posts.
	isTTY func() bool

	// dispatchWait is the one post-dispatch observation delay. Tests inject a
	// zero-cost waiter; production waits long enough to observe delayed harness
	// startup failures without sleeping once per review.
	dispatchWait func(context.Context) error
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

// WithTmuxClient supplies the tmux boundary used by every pr operation. It is
// primarily the injection point for a socket-pinned client; nil leaves the
// runner-backed environmental client in place.
func WithTmuxClient(client *tmux.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.tmuxClient = client
		}
	}
}

// WithApprover overrides the human approval gate — used in tests to supply a
// deterministic approve/deny without a TTY.
func WithApprover(fn func(review string) (bool, error)) Option {
	return func(c *Client) { c.approve = fn }
}

// WithRemovalConfirmer overrides the destructive-repair confirmation gate —
// used in tests to drive the interactive path without a TTY.
//
// Deliberately NOT the same option as WithApprover: a caller that wants
// unattended review posting must not thereby consent to unattended deletion of
// a clean room, so the two decisions are wired separately or not at all.
func WithRemovalConfirmer(fn func(prompt string) (bool, error)) Option {
	return func(c *Client) { c.confirmRemoval = fn }
}

// WithApprovalTheme supplies the resolved theme the default approval gate
// renders with, so `[theme]` preset, mode, and colour overrides reach the one
// form that decides whether a review gets posted. It styles the gate; it never
// changes what the gate decides.
//
// A no-op when WithApprover replaced the gate — an injected approver owns its
// own presentation, and silently restyling someone else's function would be
// the wrong kind of helpful.
func WithApprovalTheme(th theme.Theme) Option {
	return func(c *Client) { c.approvalTheme = th }
}

// WithTTYCheck overrides the interactive-TTY detection — used in tests.
func WithTTYCheck(fn func() bool) Option {
	return func(c *Client) { c.isTTY = fn }
}

// WithDispatchWait overrides the post-dispatch observation delay.
func WithDispatchWait(fn func(context.Context) error) Option {
	return func(c *Client) { c.dispatchWait = fn }
}

// WithRecordFS injects the record writer's filesystem seam — used in tests to
// fail one named step of the atomic write and to log the call order.
func WithRecordFS(rfs recordFS) Option {
	return func(c *Client) {
		if rfs != nil {
			c.fs = rfs
		}
	}
}

// WithLockWait overrides how long withLifecycleLock waits for the lifecycle
// lock before refusing — used in tests to make contention observable in
// milliseconds.
func WithLockWait(d time.Duration) Option {
	return func(c *Client) { c.lockWait = d }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:         run,
		tmuxClient:  tmux.New(run),
		tmuxSession: defaultTmuxSession,
		fs:          osRecordFS{},
		isTTY:       launch.IsInteractiveTTY,
		dispatchWait: func(ctx context.Context) error {
			timer := time.NewTimer(8 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
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
	// The default gate is installed after options so it can read the theme one
	// of them may have supplied. WithApprover still wins: an approver it set is
	// non-nil here and is left alone.
	if c.approve == nil {
		c.approve = func(review string) (bool, error) { return confirmReview(review, c.approvalTheme) }
	}
	if c.confirmRemoval == nil {
		c.confirmRemoval = func(prompt string) (bool, error) { return confirmRemoval(prompt, c.approvalTheme) }
	}
	return c
}

// SessionsDir returns the resolved breadcrumb directory.
func (c *Client) SessionsDir() string { return c.sessionsDir }

// FindingsDir returns the resolved local-review findings directory.
func (c *Client) FindingsDir() string { return c.findingsDir }

// sandboxPrefix is the os.MkdirTemp prefix sandbox uses for every workspace
// ("forgectl-workflow-*"); the breadcrumb content check requires a workspace
// to carry this exact prefix. Narrower than a bare "forgectl-" so it can't
// match unrelated forgectl-owned temp dirs, e.g. "forgectl-findings-*"
// (findings.go) — though that one lives under config.PrFindingsDir(), not
// the OS temp root, so it was never reachable here anyway.
//
// Named for what it is: a sandbox IDENTITY marker, not a location bound.
// validateWorkspace no longer requires a workspace to sit under the OS temp
// root, so nothing here implies one; rejectCleanRoomPath's prefix scan adds
// its own temp-root bound separately (local.go).
// Aliased to sandbox.WorkspacePrefix — the producer (sandbox.Sandbox) and
// both guards (sandbox.Teardown, validateWorkspace) read one constant so they
// cannot disagree about what a workspace looks like.
const sandboxPrefix = sandbox.WorkspacePrefix

// osTempDir is a seam over os.TempDir so the breadcrumb content check is
// testable against a redirected temp root.
var osTempDir = os.TempDir
