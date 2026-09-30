// Package launch is the per-project harness launcher behind
// `forgectl launch`. It resolves a posture from config (see profile.go),
// assembles harness-native argv for each posture, merges environment, and
// execs the selected binary in place. Absorbed from the standalone claunch
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

	"github.com/cameronsjo/forgectl/internal/termsafe"
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
// --exclude-…, --resume) are intentionally omitted — they break -p/--print,
// which `forgectl launch` routes to PrintArgs instead but the `forgectl pr`
// review dispatch still sends here.
//
// Every --add-dir is emitted BEFORE --model, never last. --add-dir is variadic
// (`<directories...>`), so an add-dir directly ahead of the user's args
// swallows a bare prompt as one more directory: `claude -p --add-dir /tmp hi`
// fails "Input must be provided", while `--add-dir /tmp --model sonnet hi`
// runs the prompt (Claude Code 2.1.285). --model is unconditional, so it
// always closes the list.
//
// --strict-mcp-config is GATED on Profile.StrictMCP, never unconditional: this
// function also serves the operator's ordinary `forgectl launch`, which must
// keep its discovered MCP servers.
//
// --allow-dangerously-skip-permissions follows p.AllowDanger as given. When
// `forgectl launch` routes a run here with stdout off a terminal, selectPosture
// clears AllowDanger first (forgectl#812).
func BuilderArgs(p Profile, userArgs []string) []string {
	args := []string{"--permission-mode", p.PermissionMode}
	if p.AllowDanger {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if p.StrictMCP {
		args = append(args, "--strict-mcp-config")
	}
	for _, d := range p.AddDir {
		args = append(args, "--add-dir", d)
	}
	args = append(args, "--model", p.Model)
	args = appendEffort(args, p)
	return append(args, userArgs...)
}

// PrintArgs is the print-mode posture (`-p`, `--print`, or `--output-format`
// off a terminal; see IsClaudePrintMode):
// the profile's permission mode, and nothing else, ahead of the user's args.
//
// The permission mode stays because it keeps launch's invariant that it always
// starts in a posture that cannot write (builtinPermissionMode). An unattended
// `forgectl launch -p …` is the case that invariant exists for, and without
// the flag it would fall back to settings.json's defaultMode. It takes one
// value and is valid with -p, and it goes first, so a later user
// --permission-mode still wins. Everything else is dropped, so a script gets
// the model, effort, and directories it asks for, as with plain `claude -p`,
// and allow_danger never makes bypass reachable in an unattended run.
func PrintArgs(p Profile, userArgs []string) []string {
	return append([]string{"--permission-mode", p.PermissionMode}, userArgs...)
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

// PiArgs applies Pi's provider/model selection before the operator's args.
// Pi accepts the same flag shape for interactive and non-interactive runs, so
// one builder owns both. User args remain last, preserving launch's established
// override rule (a later --provider or --model wins in Pi 0.82.1).
//
// Cadence environment belongs to Profile.Env, not argv: BuildInvocation merges
// that map for every harness, including Pi, without hard-coding machine-local
// directories or credentials here.
func PiArgs(p Profile, userArgs []string) []string {
	var args []string
	if p.Provider != "" {
		args = append(args, "--provider", p.Provider)
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	return append(args, userArgs...)
}

// agentsBooleanFlags are the `claude agents` options that take no value
// (`claude agents --help`, Claude Code 2.1.285). IsAgentsPassthrough uses them
// to tell a flag in flag position from one sitting in a value slot. If this
// list drifts, the result is the safe one: an unknown flag is assumed to take
// a value, and the posture is injected.
var agentsBooleanFlags = map[string]bool{
	"--all":                                true,
	"--allow-dangerously-skip-permissions": true,
	"--dangerously-skip-permissions":       true,
	"--restricted":                         true,
	"--strict-mcp-config":                  true,
	"--json":                               true,
	"--help":                               true,
	"-h":                                   true,
}

// IsAgentsPassthrough reports whether `claude agents …` is a scripting/help
// invocation that must reach claude byte-clean: no posture injection, no banner.
//
// `--json`, `--help`, or `-h` counts only in flag position, never as the
// value of a preceding option. `agents --settings --help` hands `--help` to
// --settings, and a match there would strip the posture from a session-
// dispatching agents run. A token is in flag position when it follows
// `agents` itself, a `--flag=value`, a known boolean flag, or a bare value
// (agents takes no positionals, so a bare token is always some flag's value).
//
// The scan stops at claude's own `--`, as IsClaudePrintMode does. Everything
// after it is an operand, so `agents -- x --json` is not a JSON listing:
// claude fails it "too many arguments for 'agents'" (2.1.285). It keeps the
// posture-injecting branch.
//
// A `--` in a value slot is that option's value, not the end of options:
// `claude agents --settings -- --json` reads "--" as the settings file
// (2.1.285), so the scan goes on past it (forgectl#766). Unlike the top-level
// print scan this needs no list of value flags. If a flag assumed to take a
// value is really boolean, claude reads the `--` as the end of options and
// fails the run with "too many arguments", so no session starts either way.
func IsAgentsPassthrough(agentArgs []string) bool {
	for i := 1; i < len(agentArgs); i++ {
		switch agentArgs[i] {
		case "--":
			if i > 1 && !inFlagPosition(agentArgs[i-1], agentsBooleanFlags) {
				continue // the option's value, not the end of options
			}
			return false
		case "--json", "--help", "-h":
		default:
			continue
		}
		if i == 1 || inFlagPosition(agentArgs[i-1], agentsBooleanFlags) {
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

// StripEnv returns base without any entry whose key is in keys. It is the
// removal MergeEnv cannot express: a map of overrides can only assign, and
// assigning the empty string is not the same as removing the variable for a
// consumer that tests presence rather than truthiness — or for NO_PROXY, whose
// empty value means "no bypass exceptions" rather than "no proxy".
func StripEnv(base []string, keys []string) []string {
	if len(keys) == 0 {
		return base
	}
	drop := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		drop[k] = struct{}{}
	}
	out := make([]string, 0, len(base))
	for _, e := range base {
		k := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			k = e[:i]
		}
		if _, dropped := drop[k]; dropped {
			continue
		}
		out = append(out, e)
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
//
// The argv is config-derived and only partly allowlisted — Profile.Validate
// constrains effort and the Codex fields, but model, permission_mode, and
// add_dir reach the banner verbatim — so the whole line goes through
// termsafe.SafeLine before it reaches a terminal. Otherwise an escape sequence in
// config.toml could clear the line and forge a different posture than the one
// about to exec.
//
// The line is an informational record, not a copy-pasteable command:
// strings.Join does no shell quoting, so an add_dir containing spaces renders
// as two ambiguous tokens.
func Banner(w io.Writer, args []string) {
	_, _ = fmt.Fprintln(w, termsafe.SafeLine("→ claude "+strings.Join(args, " ")))
}

// HarnessBanner writes an informational launch line for any supported CLI. Sanitized
// on the same grounds as Banner, and with the same no-shell-quoting caveat.
func HarnessBanner(w io.Writer, harness string, args []string) {
	line := "→ " + harness
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	_, _ = fmt.Fprintln(w, termsafe.SafeLine(line))
}

// Exec replaces the current process with the selected harness. On success it never returns, so
// Ctrl-C, the TTY, and the exit code pass through untouched. This is the one
// documented exception to routing process execution through internal/exec.Runner
// — Runner spawns a child, whereas the launcher must *become* the harness.
func Exec(harnessPath string, args, env []string) error {
	argv := append([]string{harnessPath}, args...)
	// #nosec G204 -- harnessPath is validated by ResolveBinary (exists + executable)
	// or resolved via exec.LookPath; replacing this process with the harness is the
	// entire purpose of the launcher, not an injection sink.
	return syscall.Exec(harnessPath, argv, env)
}
