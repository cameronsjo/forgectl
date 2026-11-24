// Package notify posts a desktop notification on macOS. It knows nothing of
// forgectl's own types: a caller hands it a [Message] of plain strings, and on
// any other platform Notify is a no-op.
//
// # Senders
//
// macOS credits a notification to the app that posted it, and a click opens
// that app. osascript's banners belong to Script Editor, so a click on one
// opens Script Editor's file panel. When terminal-notifier is on PATH it posts
// instead: its banner's click can run a command (forgectl herdr
// _focus-terminal, which shows the herdr-app tab of the terminal that raised
// it) and bring herdr-app forward, and a group lets a later call replace or
// remove the banner. When
// terminal-notifier is missing, or fails (its notification permission is
// off: it exits 3), osascript posts the same title and body, so the signal
// is not lost; only the click target is.
//
// # ARGV-PASSING IS LOAD-BEARING
//
// The title and body reach both senders as argv elements, never spliced into
// a script or a shell string. For osascript the AppleScript source is the
// constant notifyScript and the strings arrive as `on run argv` arguments,
// so a body carrying quotes, `& do shell script "…"`, or `$(...)` stays a
// string. internal/clip's file_darwin.go follows the same rule.
//
// terminal-notifier's -execute value is the one shell string here (its click
// runs it through /bin/sh). It is built by clickCommand from only three
// parts: forgectl's own executable path, the fixed verb, and ids that passed
// idPattern, each single-quoted by shellQuote. The title and body never
// enter it. Never build it with fmt.Sprintf over caller text.
package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Sender names, as Sent.Sender and Client.Sender report them.
const (
	SenderTerminalNotifier = "terminal-notifier"
	SenderOsascript        = "osascript"
)

// herdrAppBundle is herdr-app's bundle id: a click brings it forward.
const herdrAppBundle = "dev.herdr.app"

// herdrAppBinary is the herdr-app CLI the click command drives.
const herdrAppBinary = "herdr-app"

// notifyScript shows argv[1] as the notification body under argv[0] as its
// title. Both arrive as positional arguments; see the package doc.
const notifyScript = "on run argv\n\tdisplay notification (item 2 of argv) with title (item 1 of argv)\nend run"

// Caps on the two fields, in runes of termsafe.SafeLineMax output. A
// notification banner shows far less than either; the caps bound what a
// hostile or runaway value can put on a command line.
const (
	maxTitleRunes = 64
	maxBodyRunes  = 256
)

// maxIDLen caps a group or terminal id; idPattern enforces it.
const maxIDLen = 64

// idPattern is every id that may reach terminal-notifier's -group or the
// click command: a group, and a herdr-app terminal address.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// defaultTimeout bounds one sender invocation. A notification is a courtesy:
// a hung sender must not hold up the caller that asked for it.
const defaultTimeout = 5 * time.Second

// Message is one notification.
type Message struct {
	Title string
	Body  string
	// Group names the banner so a later Notify with the same group replaces
	// it and Remove clears it. Empty, or not matching idPattern: no group.
	// GroupID builds a valid one from any key.
	Group string
	// Terminal is the herdr-app terminal (HC_TERMINAL_ID) a click shows.
	// Empty, or not matching idPattern: a click only brings herdr-app forward.
	Terminal string
}

// Sent says how a notification went out.
type Sent struct {
	// Sender is the sender that posted it: SenderTerminalNotifier or
	// SenderOsascript. Empty when nothing was posted (off darwin).
	Sender string
	// Fallback is terminal-notifier's failure when osascript posted in its
	// place, nil otherwise.
	Fallback error
}

// Client posts notifications through exec.Runner (never os/exec directly).
type Client struct {
	run exec.Runner

	// goos is runtime.GOOS by default; WithGOOS overrides it for tests.
	goos string

	// lookPath finds terminal-notifier and herdr-app; WithLookPath overrides
	// it so tests never see the host's binaries.
	lookPath func(string) (string, error)

	// executable is forgectl's own path, for the click command.
	executable func() (string, error)

	// timeout bounds each sender call.
	timeout time.Duration
}

// Option configures a Client at construction.
type Option func(*Client)

// WithGOOS overrides the platform Notify checks against: a test-only hook,
// as in internal/clip.
func WithGOOS(goos string) Option {
	return func(c *Client) { c.goos = goos }
}

// WithLookPath overrides how terminal-notifier and herdr-app are found.
func WithLookPath(fn func(string) (string, error)) Option {
	return func(c *Client) { c.lookPath = fn }
}

// WithExecutable overrides how forgectl's own path is found.
func WithExecutable(fn func() (string, error)) Option {
	return func(c *Client) { c.executable = fn }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:        run,
		goos:       runtime.GOOS,
		lookPath:   osexec.LookPath,
		executable: os.Executable,
		timeout:    defaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Supported reports whether Notify posts anything on this platform: only
// darwin does. Elsewhere Notify is a no-op that returns nil, so a caller that
// wants to know whether anyone was told asks this first.
func (c *Client) Supported() bool { return c.goos == "darwin" }

// Sender names the sender Notify tries first here: terminal-notifier when it
// is on PATH, else osascript. Empty off darwin.
func (c *Client) Sender() string {
	if !c.Supported() {
		return ""
	}
	if _, ok := c.terminalNotifier(); ok {
		return SenderTerminalNotifier
	}
	return SenderOsascript
}

// SenderPath is where terminal-notifier is, when Sender would use it.
func (c *Client) SenderPath() (string, bool) {
	if !c.Supported() {
		return "", false
	}
	return c.terminalNotifier()
}

func (c *Client) terminalNotifier() (string, bool) {
	p, err := c.lookPath(SenderTerminalNotifier)
	return p, err == nil && p != ""
}

// Notify posts one desktop notification. Off darwin it returns a zero Sent
// and nil without spawning anything. Title and body are rendered inert and
// capped with termsafe.SafeLineMax first, and a returned error is sanitized,
// because *exec.CommandError echoes its argv verbatim.
//
// terminal-notifier posts when it is on PATH. If it fails, osascript posts
// the same text and Sent.Fallback carries terminal-notifier's error; the
// returned error is non-nil only when no sender posted.
func (c *Client) Notify(ctx context.Context, m Message) (Sent, error) {
	if !c.Supported() {
		return Sent{}, nil
	}
	title := termsafe.SafeLineMax(m.Title, maxTitleRunes)
	body := termsafe.SafeLineMax(m.Body, maxBodyRunes)

	var fallback error
	if tn, ok := c.terminalNotifier(); ok {
		err := c.runBounded(ctx, c.firstSenderBudget(ctx), tn, c.terminalNotifierArgs(title, body, m))
		if err == nil {
			return Sent{Sender: SenderTerminalNotifier}, nil
		}
		fallback = fmt.Errorf("terminal-notifier notification: %w", termsafe.Error(err))
		if ctx.Err() != nil {
			// The caller's own deadline is spent: osascript would start
			// already cancelled.
			return Sent{}, fallback
		}
	}
	if err := c.runBounded(ctx, c.timeout, SenderOsascript, []string{"-e", notifyScript, "--", title, body}); err != nil {
		err = fmt.Errorf("osascript notification: %w", termsafe.Error(err))
		return Sent{}, errors.Join(fallback, err)
	}
	return Sent{Sender: SenderOsascript, Fallback: fallback}, nil
}

// firstSenderBudget bounds terminal-notifier when osascript may still have
// to follow it: at most half of what the caller's deadline leaves, so a slow
// or hung terminal-notifier (a cold start, a value it mis-parses) cannot use
// the whole budget and leave the fallback none.
func (c *Client) firstSenderBudget(ctx context.Context) time.Duration {
	d := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		if half := time.Until(dl) / 2; half < d {
			d = half
		}
	}
	return d
}

// runBounded runs one sender under its own timeout, inside the caller's ctx.
func (c *Client) runBounded(ctx context.Context, d time.Duration, name string, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return c.runSender(ctx, name, args)
}

func (c *Client) runSender(ctx context.Context, name string, args []string) error {
	_, err := c.run.Run(ctx, name, args...)
	return err
}

// terminalNotifierArgs is terminal-notifier's argv. Title and body are
// separate elements; only clickCommand's output is a shell string. Every
// free-text value goes through tnValue.
func (c *Client) terminalNotifierArgs(title, body string, m Message) []string {
	args := []string{"-title", tnValue(title), "-message", tnValue(body)}
	if idPattern.MatchString(m.Group) {
		args = append(args, "-group", m.Group)
	}
	herdrApp, ok := c.herdrApp()
	if !ok {
		return args
	}
	if cmd, ok := c.clickCommand(herdrApp, m.Terminal); ok {
		args = append(args, "-execute", tnValue(cmd))
	}
	return append(args, "-activate", herdrAppBundle)
}

// tnValue escapes one option value for terminal-notifier. It reads values
// through NSUserDefaults, which parses each as a property list: one starting
// with [, (, { or a quote is misread, and the notification fails (exit 2) or
// hangs until the timeout. terminal-notifier strips one leading backslash
// from every string value (AppDelegate.m, objectForKeyedSubscript:), so a
// backslash in front of every value is exact for all of them.
func tnValue(s string) string { return "\\" + s }

// herdrApp is the absolute path of herdr-app, when it is installed. A click
// runs from launchd's bare PATH, so the click command names it in full.
func (c *Client) herdrApp() (string, bool) {
	p, err := c.lookPath(herdrAppBinary)
	if err != nil || p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	return abs, true
}

// clickCommand is the shell command a click runs: forgectl herdr
// _focus-terminal for
// one herdr-app terminal. It returns false, so the click only brings
// herdr-app forward, when the terminal id is not one idPattern accepts or
// forgectl's own path is unknown.
//
// The executable path is not resolved through symlinks: brew's link
// (/opt/homebrew/bin/forgectl) outlives an upgrade, a versioned Caskroom path
// does not, and a banner can sit in Notification Center across one.
func (c *Client) clickCommand(herdrApp, terminal string) (string, bool) {
	if !idPattern.MatchString(terminal) {
		return "", false
	}
	self, err := c.executable()
	if err != nil || self == "" {
		return "", false
	}
	self, err = filepath.Abs(self)
	if err != nil {
		return "", false
	}
	parts := []string{self, "herdr", "_focus-terminal", "--terminal", terminal, "--herdr-app", herdrApp}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	return strings.Join(quoted, " "), true
}

// shellQuote single-quotes s for /bin/sh. Each single quote in s closes the
// quoted run, is written as a backslash-escaped quote, and reopens it, so no
// character inside is special to the shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Remove clears the banner of a group terminal-notifier posted. It does
// nothing off darwin, without terminal-notifier (osascript banners have no
// group), or for a group idPattern rejects. Best effort: the caller drops the
// error or reports it, never fails on it.
func (c *Client) Remove(ctx context.Context, group string) error {
	if !idPattern.MatchString(group) {
		return nil
	}
	tn, ok := c.SenderPath()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.runSender(ctx, tn, []string{"-remove", group}); err != nil {
		return fmt.Errorf("terminal-notifier remove: %w", termsafe.Error(err))
	}
	return nil
}

// GroupID builds a group id from a kind ("desk", "drain") and any key: the
// key's runes outside idPattern's set become '-', and the whole is capped
// at the pattern's limit. Two keys that differ only in those runes share a
// group; callers key by names that are already plain.
func GroupID(kind, key string) string {
	var b strings.Builder
	for _, r := range "forgectl-" + kind + "-" + key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= maxIDLen {
			break
		}
	}
	return b.String()
}

// ValidID reports whether id may be a group or a click target.
func ValidID(id string) bool { return idPattern.MatchString(id) }
