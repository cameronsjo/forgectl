package tmux

import (
	"context"
)

// sessionFormat is the -F spec for list-sessions. Fields, in order:
// name, window count, attached(1/0), created(unix), path — joined by FieldSep.
const sessionFormat = "#{session_name}" + FieldSep +
	"#{session_windows}" + FieldSep +
	"#{?session_attached,1,0}" + FieldSep +
	"#{session_created}" + FieldSep +
	"#{session_path}"

// sessionFieldCount is how many fields sessionFormat emits.
const sessionFieldCount = 5

// ListSessions returns all tmux sessions. When no tmux server is running it
// returns an empty slice (not an error) — "no sessions" is a normal state, not
// a failure.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	args := []string{"list-sessions", "-F", sessionFormat}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentDefaultServer(ctx, args, err) {
			return nil, nil
		}
		return nil, err
	}
	return parseSessions(out)
}

// parseSessions turns list-sessions output into Sessions.
//
// EXACT, not >=, for the reason parseWindows is (see the comment there): a
// session NAME may legally carry FieldSep, and under the old `len(f) < 5`
// check `work<sep>pad` split into a row whose Name read "work" with every
// later field shifted one right — so Path read a window count and Tree
// rendered a session that does not exist. A separator anywhere in a row can
// only push the count above 5, so requiring exactly 5 drops the forged row
// instead of misreading it. The blast radius is display (Tree) and
// LastSession's attach target rather than review liveness, which is why the
// window parsers were tightened first — but the defect is the same one.
func parseSessions(out string) ([]Session, error) {
	lines := splitLines(out)
	sessions := make([]Session, 0, len(lines))
	for _, line := range lines {
		f := splitFields(line)
		if len(f) != sessionFieldCount {
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
	return parsedRows(sessions, lines, "list-sessions", sessionFieldCount)
}

// HasSession reports whether a session named name exists. It keys off
// has-session's exit code, never string-matching output — so a session
// literally named "no server running" can't fool it.
func (c *Client) HasSession(ctx context.Context, name string) bool {
	_, err := c.run.Run(ctx, c.tmuxBin, "has-session", "-t", name)
	return err == nil
}

// RenameSession renames a session. Argv order is (old, new): the -t flag
// targets the session to rename, the trailing arg is its new name.
func (c *Client) RenameSession(ctx context.Context, oldName, newName string) error {
	_, err := c.run.Run(ctx, c.tmuxBin, "rename-session", "-t", oldName, newName)
	return err
}

// KillSession kills the named session.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.run.Run(ctx, c.tmuxBin, "kill-session", "-t", name)
	return err
}
