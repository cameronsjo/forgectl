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
	serverAbsentDefault
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
	if c.getenv("TMUX") != "" {
		return serverFailure{Kind: serverCustomSocket, Cause: err}
	}
	// Everything below derives the socket THIS argv would use, and
	// serverAbsentDefault is the one classification that means "proceed" — a
	// caller may go on to create the first server. That derivation is only valid
	// for a command with no explicit socket: `-L <label>` and `-S <path>` both
	// move the socket somewhere this function does not look, so an absent
	// default would be read as "no server" for a command aimed elsewhere. No
	// caller passes them today; refuse rather than let a future one inherit the
	// proceed verdict silently.
	if hasExplicitSocketArg(expectedArgs) {
		return serverFailure{Kind: serverUnknown, Cause: err}
	}
	root := c.getenv("TMUX_TMPDIR")
	if root == "" {
		root = "/tmp"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return serverFailure{Kind: serverUnknown, Cause: err}
	}
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(c.getuid()), "default")
	_, statErr := c.lstat(socketPath)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		return serverFailure{Kind: serverAbsentDefault, SocketPath: socketPath, Cause: err}
	case errors.Is(statErr, os.ErrPermission):
		return serverFailure{Kind: serverSocketPermission, SocketPath: socketPath, Cause: statErr}
	case statErr == nil:
		return serverFailure{Kind: serverStaleSocket, SocketPath: socketPath, Cause: err}
	default:
		return serverFailure{Kind: serverUnknown, SocketPath: socketPath, Cause: statErr}
	}
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

func (c *Client) absentDefaultServer(ctx context.Context, args []string, err error) bool {
	return c.classifyServerFailure(ctx, args, err).Kind == serverAbsentDefault
}
