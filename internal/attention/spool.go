package attention

import (
	"errors"
	"path/filepath"
	"time"
)

// DefaultTTL is how long a record no herdr pane vouches for is kept.
const DefaultTTL = 24 * time.Hour

var (
	// ErrNoSpool reports a spool directory that does not exist yet: nothing
	// has been published on this machine.
	ErrNoSpool = errors.New("attention: no spool yet")
	// ErrUnsafeSpool reports a spool directory or record name refused as
	// unsafe: a symlink, another user's, or wider than 0700.
	ErrUnsafeSpool = errors.New("attention: spool refused")
)

// Liveness is what herdr says about the caller's server. Known is false when
// herdr could not be asked; Socket is the caller's HERDR_SOCKET_PATH ("" when
// the caller is outside herdr); Panes holds every pane `herdr agent list`
// reported on that server.
type Liveness struct {
	Known  bool
	Socket string
	Panes  map[string]bool
	// Keep is a session whose record is never pruned: the one `put` just
	// wrote, whose pane herdr may not have detected yet.
	Keep string
}

// shouldPrune decides whether a record goes. A record on the caller's own
// herdr server (same socket) is judged by herdr alone: it stays while its
// pane is listed, whatever its age, and goes as soon as the pane is gone.
// Any other record, including every record when herdr is unavailable or the
// caller is outside herdr, goes only once older than ttl. live.Keep's record
// always stays.
func shouldPrune(r Record, age, ttl time.Duration, live Liveness) bool {
	if live.Keep != "" && r.Session == live.Keep {
		return false
	}
	if live.Known && live.Socket != "" && r.Socket == live.Socket && r.Pane != "" {
		return !live.Panes[r.Pane]
	}
	return age > ttl
}

// StateBase is the directory the spool and sweep.json live under:
// $XDG_STATE_HOME when it is absolute (a relative one is ignored, as the XDG
// spec requires), else <home>/.local/state.
func StateBase(getenv func(string) string, home string) (string, error) {
	if v := getenv("XDG_STATE_HOME"); v != "" && filepath.IsAbs(v) {
		return filepath.Clean(v), nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("attention: no home directory; set XDG_STATE_HOME")
	}
	return filepath.Join(home, ".local", "state"), nil
}

// SweepPath is where `forgectl sweep` writes its last result.
func SweepPath(stateBase string) string {
	return filepath.Join(stateBase, "cadence", "sweep.json")
}
