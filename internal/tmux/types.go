package tmux

import "time"

// Session is a tmux session as listed by list-sessions.
type Session struct {
	Name     string
	Windows  int
	Attached bool
	Created  time.Time
	Path     string
}

// Window is a tmux window. Target is pre-built as "session:index" so callers
// can jump without re-assembling it.
type Window struct {
	Session string
	Index   int
	Name    string
	Active  bool
	Panes   int
	Target  string // "session:index"
}

// Pane is a tmux pane. Target is pre-built as "session:windowIndex.paneIndex".
type Pane struct {
	Session string
	Window  int
	Index   int
	Title   string
	Command string
	Active  bool
	Target  string // "session:window.pane"
}
