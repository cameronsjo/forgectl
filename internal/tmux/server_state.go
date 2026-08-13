package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

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
	root := c.getenv("TMUX_TMPDIR")
	if root == "" {
		root = "/tmp"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return serverFailure{Kind: serverUnknown, Cause: err}
	}
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(c.geteuid()), "default")
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

func (c *Client) absentDefaultServer(ctx context.Context, args []string, err error) bool {
	return c.classifyServerFailure(ctx, args, err).Kind == serverAbsentDefault
}
