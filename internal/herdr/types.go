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
	PaneID                string        `json:"pane_id"`
	TabID                 string        `json:"tab_id"`
	WorkspaceID           string        `json:"workspace_id"`
	TerminalID            string        `json:"terminal_id"`
	Agent                 string        `json:"agent"`
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
