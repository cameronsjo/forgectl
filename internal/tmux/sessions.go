package tmux

import (
	"context"
	"errors"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// sessionFormat is the -F spec for list-sessions. Fields, in order:
// name, window count, attached(1/0), created(unix), path — joined by fieldSep.
const sessionFormat = "#{session_name}" + fieldSep +
	"#{session_windows}" + fieldSep +
	"#{?session_attached,1,0}" + fieldSep +
	"#{session_created}" + fieldSep +
	"#{session_path}"

// ListSessions returns all tmux sessions. When no tmux server is running it
// returns an empty slice (not an error) — "no sessions" is a normal state, not
// a failure.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	out, err := c.run.Run(ctx, c.tmuxBin, "list-sessions", "-F", sessionFormat)
	if err != nil {
		if isNoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseSessions(out), nil
}

// parseSessions turns list-sessions output into Sessions. Rows with too few
// fields are skipped defensively rather than panicking.
func parseSessions(out string) []Session {
	lines := splitLines(out)
	sessions := make([]Session, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		if len(f) < 5 {
			continue
		}
		sessions = append(sessions, Session{
			Name:     f[0],
			Windows:  atoi(f[1]),
			Attached: f[2] == "1",
			Created:  parseUnix(f[3]),
			Path:     f[4],
		})
	}
	return sessions
}

// isNoServer reports whether err is tmux complaining that no server is running.
func isNoServer(err error) bool {
	var cmdErr *exec.CommandError
	if errors.As(err, &cmdErr) {
		return strings.Contains(cmdErr.Stderr, "no server running")
	}
	return strings.Contains(err.Error(), "no server running")
}
