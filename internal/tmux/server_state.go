package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

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
)

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
	_, statErr := c.lstat(socketPath)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		return serverFailure{Kind: serverAbsent, SocketPath: socketPath, Cause: err}
	case errors.Is(statErr, os.ErrPermission):
		return serverFailure{Kind: serverSocketPermission, SocketPath: socketPath, Cause: statErr}
	case statErr == nil:
		return serverFailure{Kind: serverStaleSocket, SocketPath: socketPath, Cause: err}
	default:
		return serverFailure{Kind: serverUnknown, SocketPath: socketPath, Cause: statErr}
	}
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
			return "", false, serverUnknown
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
		return false
	}
	return !hasExplicitSocketArg(args[2:])
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
// `--` and long options are excluded because tmux has no long options, so a
// double-dash element is the argument terminator or an operand — never a
// bundled short flag. See TestHasExplicitSocketArgOverMatchesDeliberately.
func hasExplicitSocketArg(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}
		if strings.ContainsAny(arg, "SL") {
			return true
		}
	}
	return false
}

func (c *Client) absentServer(ctx context.Context, args []string, err error) bool {
	return c.classifyServerFailure(ctx, args, err).Kind == serverAbsent
}
