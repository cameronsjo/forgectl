package pr

// LaunchPath selects how a review agent is dispatched into its clean-room
// workspace.
type LaunchPath int

const (
	// InlineSeeded runs `claude -p <reviewPrompt>` non-interactively with the
	// deny-by-default allowlist already written into the workspace — agent A,
	// the only wired path (D4).
	InlineSeeded LaunchPath = iota
	// BareTUIEscalation would drive an interactive TUI agent that escalates
	// permissions by typing — agent B. NOT YET WIRED: LaunchPathFor returns it
	// for the named entry, but the dispatch path guards it with a clear error.
	BareTUIEscalation
)

// String renders a LaunchPath for logs and errors.
func (p LaunchPath) String() string {
	switch p {
	case BareTUIEscalation:
		return "bare-tui-escalation"
	default:
		return "inline-seeded"
	}
}

// agentPaths maps a known agent name to its launch path. The table is the
// single extension point: adding an agent is one entry here plus (for a new
// path) the dispatch wiring in launch.go. Agent B ("escalation") is present so
// the routing is testable, but its dispatch is guarded as not-yet-wired.
var agentPaths = map[string]LaunchPath{
	"":           InlineSeeded, // default → agent A
	"claude":     InlineSeeded, // agent A, explicit
	"escalation": BareTUIEscalation,
}

// LaunchPathFor returns the launch path for an agent name. Unknown agents fall
// back to InlineSeeded (agent A) — the only wired path — so a typo never
// silently selects the unwired escalation path. Pure and table-driven.
func LaunchPathFor(agent string) LaunchPath {
	if p, ok := agentPaths[agent]; ok {
		return p
	}
	return InlineSeeded
}
