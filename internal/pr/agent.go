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
	// CodexExec runs a non-interactive Codex clean-room review under Codex's
	// native sandbox. It is NOT equivalent to InlineSeeded, and the difference
	// is a security one, not a plumbing one.
	//
	// Agent A confines the reviewer with a deny-by-default Claude Code
	// allowlist (see allowlist.go): four read tools plus eight literal
	// read-only Bash prefixes, under plan mode. It grants no command-execution
	// primitive at all — `rg` is excluded by name because `rg --pre <cmd>` is
	// one.
	//
	// Codex has no allowlist equivalent. Its `--sandbox read-only` scopes
	// filesystem WRITES and network egress; it does not scope which commands
	// run. So this path permits the reviewer arbitrary shell execution with
	// host-wide READ — `~/.ssh`, `~/.aws/credentials`, `~/.codex/auth.json`
	// are all in scope — and everything read is transmitted to the model
	// provider as tool output, since the sandbox does not sit between Codex
	// and its own API. Verified against codex-cli 0.146.0: `codex exec
	// --config approval_policy="never" --sandbox read-only` ran a prompted
	// shell command and returned the full contents of ~/.ssh.
	//
	// Two confinements were attempted and are NOT reachable from `codex exec`
	// on 0.146.0: `-c shell_environment_policy.inherit=none` (honored by
	// `codex sandbox`, silently ignored by `codex exec` — the parent env
	// passes through), and a readable-root restriction (the
	// `[permissions.<name>] filesystem = { "<path>" = "read" }` profile works,
	// but `codex exec` accepts no `--permission-profile`, and neither
	// `default_permissions`, `permissions_profile`, nor a $CODEX_HOME
	// requirements.toml selects one).
	//
	// Consequence: a prompt injection in a reviewed third-party PR diff buys a
	// shell here where it buys nothing under agent A. Prefer agent A for
	// untrusted heads.
	CodexExec
)

// String renders a LaunchPath for logs and errors.
func (p LaunchPath) String() string {
	switch p {
	case BareTUIEscalation:
		return "bare-tui-escalation"
	case CodexExec:
		return "codex-exec"
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
	"codex":      CodexExec,
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
