// Package launch is the per-project Claude Code launcher behind
// `forgectl launch`. It resolves a posture from config (see profile.go), runs a
// short guided interview when interactive (interview.go), assembles the claude
// argv for each posture (session, builder, agents), merges environment, and
// execs claude in place. Absorbed from the standalone claunch tool.
package launch

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"
)

// SessionMode selects how an interactive session resumes (or doesn't).
type SessionMode int

const (
	New SessionMode = iota
	Resume
	Fork
)

// SessionArgs builds the full interactive posture: plan-mode default, bypass
// reachable (when allowed), IDE + lean system prompt, the chosen model, the
// resume/fork choice, then each --add-dir.
func SessionArgs(p Profile, model string, mode SessionMode) []string {
	args := []string{"--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	args = append(args, "--ide", "--exclude-dynamic-system-prompt-sections", "--model", model)
	switch mode {
	case Resume:
		args = append(args, "--resume")
	case Fork:
		args = append(args, "--resume", "--fork-session")
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
func BuilderArgs(p Profile, userArgs []string) []string {
	args := []string{"--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	args = append(args, "--model", p.Model)
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
	return append(out, agentArgs[1:]...)
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

// Banner writes the informational "→ claude …" line. It always goes to stderr so
// it never corrupts piped stdout (e.g. `forgectl launch agents --json | jq`).
func Banner(w io.Writer, args []string) {
	_, _ = fmt.Fprintln(w, "→ claude "+strings.Join(args, " "))
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
