package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

type serverFailureKind uint8

const (
	serverUnknown serverFailureKind = iota
	serverAbsent
	serverCustomSocket
	serverStaleSocket
	serverSocketPermission
	serverCanceled
	// serverPinMismatch means a pinned client saw an argv it did not build —
	// a call site that skipped tmuxArgs, so the command went to the
	// environmental server. It is separated from serverUnknown because the two
	// send an operator to opposite places: unknown says "your tmux is
	// unreadable", this says "forgectl aimed the command wrong". Folded
	// together, the only trace of a missing pin was a Debug line.
	serverPinMismatch
	// serverSocketDirMissing means the pinned socket's PARENT directory does not
	// exist. It is not serverAbsent, because tmux creates the default socket's
	// directory but never an explicit `-S` one — so "absent, go create it" would
	// send the caller into a bind failure against a path nothing can bind.
	serverSocketDirMissing
	// serverDeadSocket means the socket file exists, is a socket, and a
	// connect to it was refused: nothing listens there (forgectl#786). tmux
	// 3.4 leaves its socket behind whenever the server exits, cleanly or not,
	// so this is the ordinary state after the last session closes. It is NOT
	// serverAbsent: it maps to ErrServerUnreadable wrapped with
	// ErrServerExited, which only opted-in callers read as "no server".
	serverDeadSocket
)

// String renders a serverFailureKind for logging — the log line at the bottom
// of classifyServerFailure is the only consumer, and a bare uint8 there would
// read as noise.
func (k serverFailureKind) String() string {
	switch k {
	case serverAbsent:
		return "absent"
	case serverCustomSocket:
		return "custom_socket"
	case serverStaleSocket:
		return "stale_socket"
	case serverSocketPermission:
		return "socket_permission"
	case serverCanceled:
		return "canceled"
	case serverPinMismatch:
		return "pin_mismatch"
	case serverSocketDirMissing:
		return "socket_dir_missing"
	case serverDeadSocket:
		return "dead_socket"
	default:
		return "unknown"
	}
}

type serverFailure struct {
	Kind       serverFailureKind
	SocketPath string
	Cause      error
}

func (c *Client) classifyServerFailure(ctx context.Context, expectedArgs []string, err error) serverFailure {
	if ctx.Err() != nil {
		return serverFailure{Kind: serverCanceled, Cause: ctx.Err()}
	}
	var commandErr *internalexec.CommandError
	if !errors.As(err, &commandErr) || commandErr.Name != c.tmuxBin || commandErr.ExitCode != 1 || !reflect.DeepEqual(commandErr.Args, expectedArgs) {
		return serverFailure{Kind: serverUnknown, Cause: err}
	}
	socketPath, ok, refusal := c.classifiableSocket(expectedArgs)
	if !ok {
		return serverFailure{Kind: refusal, Cause: err}
	}
	info, statErr := c.lstat(socketPath)
	var failure serverFailure
	switch {
	case errors.Is(statErr, os.ErrNotExist) && !c.socketDirUsable(socketPath):
		// Absent socket AND no directory to put one in. Only reachable under a
		// pin: the environmental socket's directory is one tmux makes itself.
		failure = serverFailure{Kind: serverSocketDirMissing, SocketPath: socketPath, Cause: err}
	case errors.Is(statErr, os.ErrNotExist):
		failure = serverFailure{Kind: serverAbsent, SocketPath: socketPath, Cause: err}
	case errors.Is(statErr, os.ErrPermission):
		failure = serverFailure{Kind: serverSocketPermission, SocketPath: socketPath, Cause: statErr}
	case statErr == nil && c.socketRefusesConnect(ctx, socketPath, info):
		failure = serverFailure{Kind: serverDeadSocket, SocketPath: socketPath, Cause: err}
	case statErr == nil:
		failure = serverFailure{Kind: serverStaleSocket, SocketPath: socketPath, Cause: err}
	default:
		failure = serverFailure{Kind: serverUnknown, SocketPath: socketPath, Cause: statErr}
	}
	// This is the classifier's decisive verdict — serverAbsent is the ONE kind
	// that permits a caller to create a server, so a confusing "why did it
	// create/refuse" report is diagnosed from this line, not from the caller's
	// own (already-typed) error.
	slog.Debug("Classified tmux server failure.",
		"kind", failure.Kind, "socket", socketPath, "pinned", c.socket != "")
	return failure
}

// socketRefusesConnect reports whether the socket file at path is proven
// dead: lstat saw a socket (not a symlink, not a regular file) and a unix
// connect to it failed with ECONNREFUSED, which is the kernel saying no
// process is listening on it. Every other outcome — a connect that succeeds
// (a live server that failed the command for some other reason), a
// permission error, a timeout, a canceled context — is not proof, and the
// caller keeps the fail-closed serverStaleSocket.
//
// The refusal must be seen TWICE, deadSocketRecheck apart (forgectl#806). On
// Darwin and the BSDs a unix socket whose listen backlog is full also answers
// ECONNREFUSED, so one refused connect to a live but saturated server would
// read as dead, and EnsureSession would start a second server over it,
// orphaning the live one. A second refusal after a pause narrows that to a
// backlog that stays full for the whole pause; it is a cheap hardening, not
// a proof, and the pause is paid only on the path that already saw a refusal.
// (Linux answers a full backlog with EAGAIN, which was never proof.)
func (c *Client) socketRefusesConnect(ctx context.Context, path string, info os.FileInfo) bool {
	if info == nil || info.Mode().Type() != os.ModeSocket {
		return false
	}
	if !errors.Is(c.dialSocket(ctx, path), syscall.ECONNREFUSED) {
		return false
	}
	pause := time.NewTimer(deadSocketRecheck)
	defer pause.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-pause.C:
	}
	return errors.Is(c.dialSocket(ctx, path), syscall.ECONNREFUSED)
}

// deadSocketRecheck is the pause between socketRefusesConnect's two probes.
const deadSocketRecheck = 50 * time.Millisecond

// dialUnixSocket is the production dialSocket: one bounded unix connect,
// closed at once. A live tmux server sees a client that connects and leaves,
// which it handles like any client that disconnects.
func dialUnixSocket(ctx context.Context, path string) error {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	return conn.Close()
}

// socketDirUsable reports whether the socket's parent directory exists, so an
// absent socket can be told apart from an unbindable path.
//
// It answers true for an environmental client without looking: `tmux` creates
// its own default-socket directory, so absence there is genuinely "no server
// yet". An explicit `-S` path gets no such treatment from tmux, which is why
// only the pinned mode needs the check.
//
// A stat error other than "not exist" answers true — the check must not turn
// its own inability to run into a refusal of an otherwise-valid create.
func (c *Client) socketDirUsable(socketPath string) bool {
	if c.socket == "" {
		return true
	}
	if _, err := c.lstat(filepath.Dir(socketPath)); errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

// classifiableSocket decides whether this argv's server absence may be read
// from the filesystem at all, and if so, which socket to inspect. A true return
// is the gateway to the ONE verdict that means "proceed, you may create the
// first server", so every path that is not provably about this client's own
// socket refuses instead.
//
// The two modes refuse for different reasons, and neither reason covers the
// other:
//
//   - ENVIRONMENTAL. $TMUX set means the operator's client is on some socket
//     this function cannot derive, so absence of the default one says nothing
//     (serverCustomSocket). An explicit `-L`/`-S` in the argv moves the target
//     somewhere the derivation does not look, so the derived default's absence
//     would be read as "no server" for a command aimed elsewhere.
//   - PINNED. The pin IS the answer, so no derivation happens and $TMUX is
//     irrelevant. What must be proven instead is that the argv actually carries
//     the pin — see pinnedArgs.
//
// path is meaningful only when ok; refusal only when !ok. Nearly every refusal
// is serverUnknown — refusal exists to carry the one case that is not
// (serverCustomSocket), which the caller reports differently.
func (c *Client) classifiableSocket(args []string) (path string, ok bool, refusal serverFailureKind) {
	if c.socket != "" {
		if !c.pinnedArgs(args) {
			return "", false, serverPinMismatch
		}
		return c.socket, true, serverUnknown
	}
	if c.getenv("TMUX") != "" {
		return "", false, serverCustomSocket
	}
	if hasExplicitSocketArg(args) {
		return "", false, serverUnknown
	}
	root := c.getenv("TMUX_TMPDIR")
	if root == "" {
		root = "/tmp"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", false, serverUnknown
	}
	return filepath.Join(root, "tmux-"+strconv.Itoa(c.getuid()), "default"), true, serverUnknown
}

// pinnedArgs reports whether argv is one this pinned client built: it leads
// with exactly the `-S <socket>` pair tmuxArgs emits, and nothing after that
// pair names a second socket.
//
// Both halves are load-bearing. The tail check earns its place on the case
// measured against tmux 3.7b:
//
//	tmux -S /a -S /b list-sessions   -> connects to /b   (last leading -S wins)
//	tmux -S /a list-sessions -S /b   -> connects to /a   (tail -S is INERT)
//
// So a SECOND leading socket option genuinely overrides the pin, and it lands
// in args[2:] where hasExplicitSocketArg catches it. A socket option after the
// command name is inert, because tmux's global options stop at the first
// non-option argument — but that is a fact about one tmux version's grammar,
// and refusing it costs nothing, so this does not try to distinguish the two.
// Reusing hasExplicitSocketArg keeps that judgment in the one over-matching
// function that owns it, and the over-match stays safe here for the same reason
// it is safe there: a false positive only withholds the proceed verdict.
func (c *Client) pinnedArgs(args []string) bool {
	if len(args) < 2 || args[0] != "-S" || args[1] != c.socket {
		// The length, never the argv: a refused argv can carry
		// `new-window -e KEY=VALUE`, whose value the Runner's per-call mask
		// would hide but this log line cannot see (forgectl#775).
		slog.Debug("Refusing argv this pinned client did not build.",
			"pin", c.socket, "argc", len(args), "reason", "the argv does not lead with the pin")
		return false
	}
	if hasExplicitSocketArg(args[2:]) {
		slog.Debug("Refusing argv naming a second socket after the pin.",
			"pin", c.socket, "argc", len(args), "reason", "a socket option follows the pin")
		return false
	}
	return true
}

// hasExplicitSocketArg reports whether argv names a socket other than the
// default one — tmux's server options `-L <label>` and `-S <path>`, in every
// spelling getopt accepts: separated (`-S /path`), attached (`-S/path`), and
// BUNDLED behind other short flags (`-2S/path`, verified to set the socket on
// tmux 3.7b). The bundled form is why this cannot be a prefix test: `-2S/tmp/x`
// begins with neither -L nor -S and moves the socket anyway.
//
// So the rule is any single-dash element carrying an uppercase S or L anywhere
// in it. It over-matches enormously — an operand tmux would read as a plain
// value (a session name, a `-c` directory) counts too. That direction is the
// safe one: a false positive only downgrades the verdict to serverUnknown,
// which refuses to read an absent socket as "no server, proceed". Being wrong
// the other way hands a proceed verdict to a command aimed at a socket this
// function never inspected. That holds at both call sites — the environmental
// derivation, where the socket at stake is the default one, and pinnedArgs's
// tail check, where it is the pin.
//
// `--` is NOT excluded. tmux has no long options today, so excluding it would
// be defensible on the option grammar alone — but this function also scans argv
// TAILS (pinnedArgs's args[2:]), where a `--` element is an operand rather than
// an option, and a future tmux long option would land in the same blind spot.
// Including it costs only more false-positive refusals, which the paragraph
// above already argues are free. See TestHasExplicitSocketArgOverMatchesDeliberately.
func hasExplicitSocketArg(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.ContainsAny(arg, "SL") {
			return true
		}
	}
	return false
}

// DisplaySessions is ListSessions for a listing shown to an operator: a server
// that has exited and left its socket behind (ErrServerExited) reads as no
// sessions, exactly like a server that never ran (forgectl#786). It is for
// display only. A "gone" verdict must come from ListSessions or a
// revalidation, which keep failing closed on that state.
func (c *Client) DisplaySessions(ctx context.Context) ([]Session, error) {
	sessions, _, err := c.DisplaySessionListing(ctx)
	return sessions, err
}

// DisplaySessionListing is DisplaySessions plus the number of session rows
// tmux returned that could not be read, most likely because a name carries
// the field separator (forgectl#806). Such a session is real but cannot be
// resolved, renamed or killed through forgectl, so a listing should say it
// exists rather than show one fewer session with no sign of it.
func (c *Client) DisplaySessionListing(ctx context.Context) (sessions []Session, unreadable int, err error) {
	sessions, unreadable, err = c.listSessions(ctx)
	if errors.Is(err, ErrServerExited) {
		return nil, 0, nil
	}
	return sessions, unreadable, err
}

// DisplayWindows is ListWindows under DisplaySessions' rule.
func (c *Client) DisplayWindows(ctx context.Context) ([]Window, error) {
	windows, _, err := c.DisplayWindowListing(ctx)
	return windows, err
}

// DisplayWindowListing is DisplayWindows plus the number of window rows tmux
// returned that could not be read (forgectl#815), for DisplaySessionListing's
// reason.
func (c *Client) DisplayWindowListing(ctx context.Context) (windows []Window, unreadable int, err error) {
	windows, unreadable, err = c.listWindows(ctx)
	if errors.Is(err, ErrServerExited) {
		return nil, 0, nil
	}
	return windows, unreadable, err
}

// UnreadableRows counts the rows an operator-facing listing could not read,
// per kind (forgectl#806, forgectl#815).
type UnreadableRows struct {
	Sessions, Windows int
}

// Note is the one-line notice a listing prints when any row was unreadable,
// and "" when none was. It is one line on purpose: the TUI shows it in a
// single-line footer, and the CLI prints it on stderr so --json output keeps
// its shape.
func (u UnreadableRows) Note() string {
	var parts []string
	if u.Sessions > 0 {
		parts = append(parts, fmt.Sprintf("%d session(s)", u.Sessions))
	}
	if u.Windows > 0 {
		parts = append(parts, fmt.Sprintf("%d window(s)", u.Windows))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " and ") + " could not be read and are not listed — " +
		"a name carrying the 0x1F field separator hides its row; rename or kill it with tmux itself"
}

// DisplayPanes is ListPanes under DisplaySessions' rule.
func (c *Client) DisplayPanes(ctx context.Context) ([]Pane, error) {
	return exitedIsEmpty(c.ListPanes(ctx))
}

func exitedIsEmpty[T any](rows []T, err error) ([]T, error) {
	if errors.Is(err, ErrServerExited) {
		return nil, nil
	}
	return rows, err
}

// absentServer reports whether a failed command proves no server is running
// on this client's socket. It never reads tmux's message: the proof is exit 1
// plus the socket file being absent (classifyServerFailure). "no server
// running on <socket>" and "server exited unexpectedly" are therefore
// classified alike. On tmux 3.4 both come with the socket file still present
// (a dead server does not unlink it), which is stale, not absent. Only the
// file being gone ("error connecting to <socket> (No such file or
// directory)") reads as absent, whichever message came with it
// (forgectl#765).
func (c *Client) absentServer(ctx context.Context, args []string, err error) bool {
	return c.classifyServerFailure(ctx, args, err).Kind == serverAbsent
}
