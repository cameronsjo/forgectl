package herdr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Environment variables herdr exports into every pane it hosts (measured).
const (
	envSession = "HERDR_ENV"
	envSocket  = "HERDR_SOCKET_PATH"
)

var (
	// ErrNotInSession means the process is not inside a herdr pane, or the
	// pane's socket is gone (herdr restarted, or the shell outlived its server).
	ErrNotInSession = errors.New("not inside a herdr session")
	// ErrForkRequired means the herdr CLI lacks `tab move`, which only the
	// cameronsjo/herdr fork provides.
	ErrForkRequired = errors.New("herdr has no `tab move`; this needs the cameronsjo/herdr fork")
)

// tabMoveUsage matches the usage line herdr prints for `tab move`. The fork
// prints "usage: herdr tab move <tab_id> --index N" on stderr and exits 2
// (measured); a clap-style rewrite would print "Usage: herdr tab move ..." and
// exit 0. It must sit at the start of a line: the fork's response to an
// unknown subcommand also lists `herdr tab move ...` inside a command list,
// and only a usage line proves the verb is real.
var tabMoveUsage = regexp.MustCompile(`(?im)^\s*usage:\s+herdr tab move\b`)

// Probe reports whether the herdr this process would talk to can do what the
// client needs: the process is in a herdr pane whose socket exists, and the
// CLI has `tab move`.
//
// It inspects the CLI only. A fork CLI talking to a server still running an
// older binary passes; that failure surfaces at [Client.MoveTab] as a typed
// *[Error]. Gate failures are reported before the capability check.
func Probe(ctx context.Context, r exec.Runner, lookupEnv func(string) (string, bool)) error {
	return probe(ctx, r, lookupEnv, os.Stat)
}

func probe(ctx context.Context, r exec.Runner, lookupEnv func(string) (string, bool), stat func(string) (fs.FileInfo, error)) error {
	if v, ok := lookupEnv(envSession); !ok || v != "1" {
		return fmt.Errorf("%w: %s is %s, want 1; run this from a herdr pane", ErrNotInSession, envSession, describe(v, ok))
	}
	sock, ok := lookupEnv(envSocket)
	if !ok || sock == "" {
		return fmt.Errorf("%w: %s is not set", ErrNotInSession, envSocket)
	}
	info, err := stat(sock)
	if err != nil {
		return fmt.Errorf("%w: %s names %s, which cannot be read: %v", ErrNotInSession, envSocket, sock, err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%w: %s names %s, which is not a socket", ErrNotInSession, envSocket, sock)
	}

	out, err := r.Run(ctx, Binary, "tab", "move", "--help")
	if err == nil {
		if tabMoveUsage.MatchString(out) {
			return nil
		}
		return fmt.Errorf("%w: `herdr tab move --help` printed no usage line", ErrForkRequired)
	}
	var ce *exec.CommandError
	if !errors.As(err, &ce) || ce.ExitCode < 0 {
		return fmt.Errorf("cannot run %s: %w", Binary, err)
	}
	// herdr's usage error: exit 2, text on stderr. Other exit codes are a
	// different failure and are not evidence about the verb either way.
	if ce.ExitCode == 2 {
		if tabMoveUsage.MatchString(ce.Stderr) || tabMoveUsage.MatchString(ce.Output) {
			return nil
		}
		return fmt.Errorf("%w: `herdr tab move --help` exited 2 without a usage line", ErrForkRequired)
	}
	return fmt.Errorf("`%s tab move --help` failed with exit %d: %s", Binary, ce.ExitCode, strings.TrimSpace(ce.Stderr))
}

func describe(v string, set bool) string {
	switch {
	case !set:
		return "not set"
	case v == "":
		return "empty"
	}
	return fmt.Sprintf("%q", v)
}
