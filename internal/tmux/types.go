package tmux

import "time"

// Session is a tmux session as listed by list-sessions.
//
// ServerPID/ServerStart identify the server incarnation the row came from, and
// ID is the native "$N" — together they are everything Identity() needs, which
// is why the listing carries them rather than a second probe re-deriving them
// (and possibly disagreeing with the rows it is qualifying).
type Session struct {
	ServerPID   string
	ServerStart string
	ID          string
	Name        string
	Windows     int
	Attached    bool
	Created     time.Time
	Path        string
}

// Identity binds the row to the selector the client is currently pointed at.
// The selector is the caller's, not the row's: a listing cannot tell you which
// socket produced it, so the binding has to come from the side that chose the
// socket.
func (s Session) Identity(selector ServerSelector) SessionIdentity {
	return SessionIdentity{
		Generation: ServerGeneration{Selector: selector, PID: s.ServerPID, StartTime: s.ServerStart},
		ID:         s.ID,
		Name:       s.Name,
	}
}

// Window is a tmux window. SessionID is the native id of its parent session —
// the parentage half of every window action's revalidation.
type Window struct {
	ServerPID   string
	ServerStart string
	ID          string
	SessionID   string
	Session     string
	Index       int
	Name        string
	Active      bool
	Panes       int
}

// Identity binds the window row to the current selector, parent id included.
func (w Window) Identity(selector ServerSelector) WindowIdentity {
	return WindowIdentity{
		Generation: ServerGeneration{Selector: selector, PID: w.ServerPID, StartTime: w.ServerStart},
		ID:         w.ID,
		SessionID:  w.SessionID,
		Name:       w.Name,
	}
}

// Pane is a tmux pane. WindowID is its parent window's native id, which is what
// groups panes under windows — the old "session name + window index" composite
// re-joined on two mutable values and could group a pane under the wrong window
// whenever a session was renamed or a window moved mid-listing.
type Pane struct {
	ServerPID   string
	ServerStart string
	ID          string
	WindowID    string
	Index       int
	Title       string
	Command     string
	Active      bool
}

// Identity binds the pane row to the current selector, parent id included.
func (p Pane) Identity(selector ServerSelector) PaneIdentity {
	return PaneIdentity{
		Generation: ServerGeneration{Selector: selector, PID: p.ServerPID, StartTime: p.ServerStart},
		ID:         p.ID,
		WindowID:   p.WindowID,
	}
}
