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
	// shell here where it buys nothing under agent A. Since the confinement is
	// unreachable, the boundary is drawn by USE instead — CheckAgentForReview
	// (provenance.go) opens this path only for code the operator has explicitly
	// stated they wrote. That refusal is enforced in the dispatch path, not
	// documented and hoped for.
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

// CheckAgentForRef is GONE — deliberately, and it should not come back.
//
// It took a Ref and asked whether the head was local or remote, then used that
// answer to decide an AUTHORSHIP question. The two coincide often enough to look
// like one predicate and diverge on the single most ordinary review workflow
// there is: `gh pr checkout 123` puts a third party's commit in the operator's
// own repository, where locality reports "yours" and authorship is "theirs"
// (forgectl#232).
//
// Its replacement is CheckAgentForReview in provenance.go, which takes the
// authorship value directly. Ref.IsLocal() keeps its own meaning — path and
// workspace ownership, and whether PostReview may fire — and is read on the
// provenance path as a NEGATIVE signal only (EffectiveProvenance), never as
// evidence that the operator wrote anything.
//
// Two guards named in the retired doc are still load-bearing and still enforced
// elsewhere: Ref.local is unforgeable (only newLocalRef sets it, see ref.go), and
// rejectCleanRoomPath refuses a `pr local` path inside a forgectl workspace
// (local.go), so a fetched hostile head cannot be laundered into "their own
// tree". Provenance is a third control beside those, not a replacement for
// either.
