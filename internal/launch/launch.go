// Package launch is the per-project Claude Code launcher behind
// `forgectl launch`. It resolves a posture from config (see profile.go),
// assembles the claude argv for each posture (session, builder, agents), merges
// environment, and execs claude in place. Absorbed from the standalone claunch
// tool.
//
// `forgectl launch` drops straight into the resolved profile: there is no
// prompt. The resume/fork half of the old interview is served better by
// `forgectl resume` (cross-repo discovery, liveness detection, task restore),
// and its model half by the profile plus a `--model` override.
package launch

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"
)

// SessionArgs builds the full interactive posture: plan-mode default, bypass
// reachable (when allowed), IDE + lean system prompt, the profile's model and
// effort, then each --add-dir.
func SessionArgs(p Profile) []string {
	args := []string{"--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	args = append(args, "--ide", "--exclude-dynamic-system-prompt-sections", "--model", p.Model)
	args = appendEffort(args, p)
	for _, d := range p.AddDir {
		args = append(args, "--add-dir", d)
	}
	return args
}

// appendEffort emits `--effort <level>` immediately after --model, and nothing
// at all when the profile resolved no level. Omitting the flag is meaningful,
// not a fallback: it leaves the user's settings.json effortLevel in charge,
// which is what every launch did before this flag existed.
func appendEffort(args []string, p Profile) []string {
	if p.Effort == "" {
		return args
	}
	return append(args, "--effort", p.Effort)
}

// ResumeArgs builds the interactive posture for resuming one KNOWN session id.
//
// Additive rather than a new SessionArgs mode on purpose: SessionArgs appends a
// bare --resume, which opens Claude Code's own picker, and that is the right
// behavior for `forgectl launch`, which is cwd-bound and has no session in
// hand. `forgectl resume` has already picked one, so it passes the id.
//
// fork maps to --fork-session, branching a new session off the transcript
// instead of continuing it — the safe way into a session you do not want to
// disturb.
func ResumeArgs(p Profile, sessionID string, fork bool) []string {
	args := []string{"--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	args = append(args, "--ide", "--exclude-dynamic-system-prompt-sections", "--model", p.Model)
	args = appendEffort(args, p)
	args = append(args, "--resume", sessionID)
	if fork {
		args = append(args, "--fork-session")
	}
	for _, d := range p.AddDir {
		args = append(args, "--add-dir", d)
	}
	return args
}

// BuilderArgs applies the profile's core posture, then appends the user's claude
// args verbatim. Injected flags go first so a user override (e.g. --model) wins
// under Claude Code's last-flag-wins parsing. Interactive-only flags (--ide,
// --exclude-…, --resume) are intentionally omitted — they break -p/--print.
//
// --strict-mcp-config is GATED on Profile.StrictMCP, never unconditional: this
// function also serves the operator's ordinary `forgectl launch`, which must
// keep its discovered MCP servers.
func BuilderArgs(p Profile, userArgs []string) []string {
	args := []string{"--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if p.StrictMCP {
		args = append(args, "--strict-mcp-config")
	}
	args = append(args, "--model", p.Model)
	args = appendEffort(args, p)
	for _, d := range p.AddDir {
		args = append(args, "--add-dir", d)
	}
	return append(args, userArgs...)
}

// AgentsArgs injects only the agents-valid posture subset between the "agents"
// subcommand and the user's remaining args. agentArgs[0] must be "agents".
func AgentsArgs(p Profile, agentArgs []string) []string {
	out := []string{"agents", "--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		out = append(out, "--allow-dangerously-skip-permissions")
	}
	out = append(out, "--model", p.Model)
	out = appendEffort(out, p)
	return append(out, agentArgs[1:]...)
}

// CodexSessionArgs builds the interactive Codex posture. It carries no resume
// or fork mode: those were reachable only through the removed launch interview,
// and Codex's own `codex resume --last` / `codex fork --last` remain one
// invocation away for anyone who wants them.
//
// No --effort here — the flag is Claude Code's, and Codex has no equivalent.
func CodexSessionArgs(p Profile) []string {
	args := []string{
		"--ask-for-approval", p.ApprovalPolicy,
		"--sandbox", p.Sandbox,
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	for _, d := range p.AddDir {
		args = append(args, "--add-dir", d)
	}
	return args
}

// CodexExecArgs runs a non-interactive Codex session. `codex exec` currently
// accepts approval policy through its native config override rather than the
// interactive `--ask-for-approval` flag.
func CodexExecArgs(p Profile, userArgs []string) []string {
	args := []string{
		"exec",
		"--config", fmt.Sprintf("approval_policy=%q", p.ApprovalPolicy),
		"--sandbox", p.Sandbox,
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	for _, d := range p.AddDir {
		args = append(args, "--add-dir", d)
	}
	return append(args, userArgs...)
}

// IsAgentsPassthrough reports whether `claude agents …` is a scripting/help
// invocation that must reach claude byte-clean: no posture injection, no banner.
func IsAgentsPassthrough(agentArgs []string) bool {
	for _, a := range agentArgs[1:] {
		switch a {
		case "--json", "--help", "-h":
			return true
		}
	}
	return false
}

// MergeEnv overlays extra onto base ("KEY=VALUE" entries). Overridden keys are
// dropped from base and re-appended (sorted) so the result is deterministic.
func MergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		k := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			k = e[:i]
		}
		if _, overridden := extra[k]; overridden {
			continue
		}
		out = append(out, e)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}

// MergeMaps overlays over onto base, returning a new map in which over's keys
// win. Either argument may be nil/empty. Used to layer the profile env over
// injected bench defaults so a user-set profile value beats an injected one.
func MergeMaps(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// Banner writes the informational "→ claude …" line. It always goes to stderr so
// it never corrupts piped stdout (e.g. `forgectl launch agents --json | jq`).
func Banner(w io.Writer, args []string) {
	_, _ = fmt.Fprintln(w, "→ claude "+strings.Join(args, " "))
}

// HarnessBanner writes an informational launch line for either CLI.
func HarnessBanner(w io.Writer, harness string, args []string) {
	_, _ = fmt.Fprintln(w, "→ "+harness+" "+strings.Join(args, " "))
}

// Exec replaces the current process with claude. On success it never returns, so
// Ctrl-C, the TTY, and the exit code pass through untouched. This is the one
// documented exception to routing process execution through internal/exec.Runner
// — Runner spawns a child, whereas the launcher must *become* claude.
func Exec(claudePath string, args, env []string) error {
	argv := append([]string{claudePath}, args...)
	// #nosec G204 -- claudePath is validated by ClaudePath (exists + executable)
	// or resolved via exec.LookPath; replacing this process with claude is the
	// entire purpose of the launcher, not an injection sink.
	return syscall.Exec(claudePath, argv, env)
}
