package herdr

// Workspace is one row of `herdr workspace list`.
type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	ActiveTabID string `json:"active_tab_id"`
	AgentStatus string `json:"agent_status"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	TabCount    int    `json:"tab_count"`
}

// Tab is one row of `herdr tab list`, or the result of `herdr tab get`.
type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	AgentStatus string `json:"agent_status"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
}

// AgentSession identifies the agent's own session (for resuming it).
type AgentSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

// Pane is one row of `herdr pane list`, or the result of `herdr pane get`.
// Agent, AgentSession, and the cwd fields are empty for a pane that has none
// (herdr reports them as null).
type Pane struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	TerminalID  string `json:"terminal_id"`
	Agent       string `json:"agent"`
	// Name is the agent's herdr name (`herdr agent rename`); empty when it has
	// none. Untrusted text, like every herdr field.
	Name                  string        `json:"name"`
	AgentSession          *AgentSession `json:"agent_session"`
	AgentStatus           string        `json:"agent_status"`
	CWD                   string        `json:"cwd"`
	ForegroundCWD         string        `json:"foreground_cwd"`
	Focused               bool          `json:"focused"`
	Revision              int64         `json:"revision"`
	TerminalTitle         string        `json:"terminal_title"`
	TerminalTitleStripped string        `json:"terminal_title_stripped"`
}

// Agent is one row of `herdr agent list`: a pane that hosts a recognized
// coding agent.
type Agent struct {
	Pane
	StateChangeSeq int64 `json:"state_change_seq"`
	// Tokens is nil when herdr reports none.
	Tokens *AgentTokens `json:"tokens"`
}

// AgentTokens are the display tokens `herdr agent list` reports for an
// agent's pane. CWD is the agent's own working directory and Worktree the
// git worktree it is in; the pane's CWD is where the pane started, which can
// be a parent of both. Session is the agent's session id as a reporter put it
// in the pane's `session` token (the herdr-bridge mod does, for Claude Code).
// herdr keeps one token map per pane, not one per reporter, so any program
// that sets display tokens can set `session` too. Untrusted text, like every
// herdr field.
type AgentTokens struct {
	CWD      string `json:"cwd"`
	Worktree string `json:"worktree"`
	Session  string `json:"session"`
}

// ReadSource selects which text `herdr pane read` returns.
type ReadSource string

// The sources herdr accepts (measured from `herdr pane read --help`).
const (
	ReadVisible         ReadSource = "visible"
	ReadRecent          ReadSource = "recent"
	ReadRecentUnwrapped ReadSource = "recent-unwrapped"
	ReadDetection       ReadSource = "detection"
)

func (s ReadSource) valid() bool {
	switch s {
	case ReadVisible, ReadRecent, ReadRecentUnwrapped, ReadDetection:
		return true
	}
	return false
}
