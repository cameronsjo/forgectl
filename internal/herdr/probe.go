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
	"github.com/cameronsjo/forgectl/internal/redact"
)

// Environment variables herdr exports into every pane it hosts (measured).
const (
	envSession = "HERDR_ENV"
	envSocket  = "HERDR_SOCKET_PATH"
	sessionOn  = "1" // the value of HERDR_ENV inside a pane
)

// exitUsage is the status herdr exits with for a usage error, which is how the
// fork answers `tab move --help` (measured).
const exitUsage = 2

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
// exit 0. It must sit at the start of a line and end the verb: the fork's
// response to an unknown subcommand also lists `herdr tab move ...` inside a
// command list, and only a usage line proves the verb is real.
var tabMoveUsage = regexp.MustCompile(`(?im)^\s*usage:\s+herdr tab move(?:\s|$)`)

// Probe reports whether the herdr this process would talk to can do what the
// client needs: the process is in a herdr pane whose socket exists, and the
// CLI has `tab move`.
//
// lookupEnv must read the environment the Runner's calls will run in. A caller
// that pins a different server by setting HERDR_SOCKET_PATH on its Runner
// passes a lookup that reflects that pin, or the gate checks the wrong socket.
//
// Probe inspects the CLI only. A fork CLI talking to a server still running an
// older binary passes; that failure surfaces at [Client.MoveTab] as a typed
// *[Error]. Gate failures are reported before the capability check, and herdr
// is not run at all when the gate fails. The check spawns herdr once, and the
// answer cannot change within a process, so call it once.
func Probe(ctx context.Context, r exec.Runner, lookupEnv func(string) (string, bool)) error {
	return probe(ctx, r, lookupEnv, os.Stat)
}

func probe(ctx context.Context, r exec.Runner, lookupEnv func(string) (string, bool), stat func(string) (fs.FileInfo, error)) error {
	if err := checkSession(lookupEnv, stat); err != nil {
		return err
	}
	return CheckFork(ctx, r)
}

// CheckSession reports whether the process is inside a herdr pane whose socket
// exists. It runs no herdr command, so a read-only caller can use it without
// needing the fork. See [Probe] for what lookupEnv must read.
func CheckSession(lookupEnv func(string) (string, bool)) error {
	return checkSession(lookupEnv, os.Stat)
}

func checkSession(lookupEnv func(string) (string, bool), stat func(string) (fs.FileInfo, error)) error {
	if v, ok := lookupEnv(envSession); !ok {
		return fmt.Errorf("%w: %s is not set; run this from a herdr pane", ErrNotInSession, envSession)
	} else if v != sessionOn {
		return fmt.Errorf("%w: %s is %q, want %q; run this from a herdr pane", ErrNotInSession, envSession, v, sessionOn)
	}
	sock, ok := lookupEnv(envSocket)
	if !ok || sock == "" {
		return fmt.Errorf("%w: %s is not set", ErrNotInSession, envSocket)
	}
	info, err := stat(sock)
	if err != nil {
		return fmt.Errorf("%w: %s names %s, which cannot be read: %w", ErrNotInSession, envSocket, sock, err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%w: %s names %s, which is not a socket", ErrNotInSession, envSocket, sock)
	}
	return nil
}

// CheckFork reports whether the herdr CLI has `tab move`, which only the
// cameronsjo/herdr fork provides. It spawns herdr once. Run [CheckSession]
// first: this does not check that a session exists.
func CheckFork(ctx context.Context, r exec.Runner) error {
	// The text to search: stdout when herdr answers (exit 0; a herdr that put its
	// usage on stderr and exited 0 would read as missing, which is not the
	// measured fork behaviour), stderr and stdout when it exits with its usage
	// status. Any other failure is not evidence about the verb either way.
	text, code := "", 0
	out, err := r.Run(ctx, Binary, "tab", "move", "--help")
	if err == nil {
		text = out
	} else {
		var ce *exec.CommandError
		if !errors.As(err, &ce) || ce.ExitCode < 0 {
			return fmt.Errorf("cannot run %s: %w", Binary, err)
		}
		if ce.ExitCode != exitUsage {
			return fmt.Errorf("`%s tab move --help` failed with exit %d: %s", Binary, ce.ExitCode, printable(redact.Text(strings.TrimSpace(ce.Stderr))))
		}
		text, code = ce.Stderr+"\n"+ce.Output, ce.ExitCode
	}
	if tabMoveUsage.MatchString(text) {
		return nil
	}
	return fmt.Errorf("%w: `herdr tab move --help` (exit %d) printed no usage line", ErrForkRequired, code)
}
