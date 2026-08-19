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
	socketPath, refusal, ok := c.classifiableSocket(expectedArgs)
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
func (c *Client) classifiableSocket(args []string) (string, serverFailureKind, bool) {
	if c.socket != "" {
		if !c.pinnedArgs(args) {
			return "", serverUnknown, false
		}
		return c.socket, serverUnknown, true
	}
	if c.getenv("TMUX") != "" {
		return "", serverCustomSocket, false
	}
	if hasExplicitSocketArg(args) {
		return "", serverUnknown, false
	}
	root := c.getenv("TMUX_TMPDIR")
	if root == "" {
		root = "/tmp"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", serverUnknown, false
	}
	return filepath.Join(root, "tmux-"+strconv.Itoa(c.getuid()), "default"), serverUnknown, true
}

// pinnedArgs reports whether argv is one this pinned client built: it leads
// with exactly the `-S <socket>` pair tmuxArgs emits, and nothing after that
// pair names a second socket.
//
// Both halves are load-bearing, and the second is the one that is easy to skip.
// A leading pin proves the command reaches this client's server ONLY if no
// later `-L`/`-S` overrides it — tmux takes the last such option, so an argv of
// `-S ours ... -S theirs` runs against theirs while passing a check that looked
// only at the front. Reusing hasExplicitSocketArg for the tail keeps that
// judgment in the one over-matching function that already owns it, and the
// over-match stays safe here for the same reason it is safe there: a false
// positive only withholds the proceed verdict.
func (c *Client) pinnedArgs(args []string) bool {
	if len(args) < 2 || args[0] != "-S" || args[1] != c.socket {
		return false
	}
	return !hasExplicitSocketArg(args[2:])
}

// hasExplicitSocketArg reports whether argv names a socket other than the
// default one — tmux's server options `-L <label>` and `-S <path>`, in both
// their separated and attached (`-Lfoo`, `-S/path`) spellings.
//
// It over-matches on purpose: any element merely beginning with -L or -S counts,
// including an operand tmux would never read as a flag (a session name, a `-c`
// directory). That direction is the safe one — a false positive only downgrades
// the verdict to serverUnknown, which refuses to read an absent default socket
// as "no server, proceed". Tightening it would mean modelling tmux's own option
// grammar, and being wrong there hands a proceed verdict to a command aimed at a
// socket this function never inspected. See
// TestHasExplicitSocketArgOverMatchesDeliberately.
func hasExplicitSocketArg(args []string) bool {
	for _, arg := range args {
		if arg == "-L" || arg == "-S" || strings.HasPrefix(arg, "-L") || strings.HasPrefix(arg, "-S") {
			return true
		}
	}
	return false
}

func (c *Client) absentServer(ctx context.Context, args []string, err error) bool {
	return c.classifyServerFailure(ctx, args, err).Kind == serverAbsent
}
