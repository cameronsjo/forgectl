package pr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// reviewSettingSources is the --setting-sources value the Claude reviewer
// runs with: empty, so Claude Code loads NO settings file — not the user's,
// not the project's `.claude/settings.json`, not `.claude/settings.local.json`
// — and the --settings document (reviewSettingsJSON) is the reviewer's whole
// configuration, below managed settings only.
//
// This is the structural half of forgectl#694. The workspace is the PR head,
// and a settings file there is PR-authored: array keys such as
// sandbox.filesystem.allowWrite or network.allowedDomains MERGE across every
// source the session loads, so --settings outranking a file decides booleans
// only; hooks in a loaded file run unsandboxed at session start; and `env`
// and `apiKeyHelper` are honored in -p mode. Quarantine renames a PR's
// `.claude/` aside before dispatch, but a name list is a denylist, and this
// removes the whole class instead. Measured on Claude Code 2.1.284: with
// `--setting-sources ""`, a SessionStart hook in either `.claude/settings.json`
// or `.claude/settings.local.json` did not run; without it, both did.
//
// Excluding user settings is deliberate too. The operator's own sandbox
// arrays (excludedCommands, allowWrite) would otherwise merge in and widen
// this posture, and their hooks and plugins have no place in a clean room.
// Authentication does not live in settings files, so it is unaffected; an
// operator whose user settings carry an `apiKeyHelper` or an `env` block the
// reviewer needs will find those absent here.
const reviewSettingSources = ""

// sandboxSettings is the `sandbox` block of the Claude reviewer's settings:
// Claude Code's OS sandbox (Seatbelt on macOS, bubblewrap on Linux) around
// every Bash command the reviewer runs, and every child process of one.
//
// It exists because the allow-list cannot bound what an allowed command DOES
// (forgectl#694): a read-only git subcommand can take a flag that writes to
// an arbitrary path, and git later runs commands its configuration names. A
// deny rule matches command text and cannot close that; the sandbox is
// enforced by the OS on the running process. What it enforces is exactly
// this block and no more — write denies on the workspace and a linked
// worktree's shared git dir, and egress limited to the PR's gh hosts. It
// does NOT narrow reads: a sandboxed command can read anything the operator
// can, and what it reads reaches the model provider as tool output.
//
// Every field is emitted explicitly, false included: the posture must not
// depend on a default Claude Code may change.
//
//   - FailIfUnavailable: without it Claude Code prints a warning and runs
//     commands UNSANDBOXED when the sandbox cannot start. The review runs in
//     a detached tmux window where nobody reads that warning.
//   - AllowUnsandboxedCommands false: ignore the model's
//     `dangerouslyDisableSandbox` retry, which is exactly what an injected
//     reviewer would ask for.
//   - AutoAllowBashIfSandboxed false: keep the allow-list the gate. Auto-allow
//     approves any command that runs sandboxed, and before Claude Code
//     v2.1.212 it did so in plan mode too, which would have admitted every
//     command the allow-list refuses (`rg --pre`, for one).
//   - The boolean weakening switches (EnableWeakerNestedSandbox,
//     EnableWeakerNetworkIsolation, AllowAppleEvents, and network
//     AllowLocalBinding and AllowAllUnixSockets) are written false. That is
//     not every widening key: the array ones (allowUnixSockets,
//     allowMachLookup, allowWrite, excludedCommands) are simply not emitted,
//     and with no settings file loaded nothing else supplies them — only
//     managed settings could.
//   - No excludedCommands: an excluded command runs outside the sandbox.
type sandboxSettings struct {
	Enabled                      bool              `json:"enabled"`
	FailIfUnavailable            bool              `json:"failIfUnavailable"`
	AllowUnsandboxedCommands     bool              `json:"allowUnsandboxedCommands"`
	AutoAllowBashIfSandboxed     bool              `json:"autoAllowBashIfSandboxed"`
	EnableWeakerNestedSandbox    bool              `json:"enableWeakerNestedSandbox"`
	EnableWeakerNetworkIsolation bool              `json:"enableWeakerNetworkIsolation"`
	AllowAppleEvents             bool              `json:"allowAppleEvents"`
	Filesystem                   sandboxFilesystem `json:"filesystem"`
	Network                      sandboxNetwork    `json:"network"`
}

// sandboxFilesystem narrows the sandbox's default write set, which is the
// working directory (the workspace), the per-user temp directory, and every
// --add-dir root. DenyWrite takes the workspace back out.
//
// Claude Code's built-in protected paths already refuse `.git/config` and
// `.git/hooks`, but not the rest of the git directory, and git follows
// pointers in it (`commondir`, for one) to configuration elsewhere. Writes
// to the whole workspace are denied instead: a reviewer has nothing to
// write there. What stays writable is the findings dir that `pr local` adds
// with --add-dir, outside the workspace, and the per-user temp directory.
type sandboxFilesystem struct {
	DenyWrite []string `json:"denyWrite"`
}

// sandboxNetwork is the reviewer's egress. AllowedDomains names the hosts the
// allow-listed gh reads reach, and nothing else; a local review gets none.
// StrictAllowlist refuses any other host instead of prompting; Claude Code
// honors it only from user, managed, and --settings, which is where this
// block lives. The two boolean socket and binding switches are written false.
//
// Every key here must carry the TYPE Claude Code's schema gives it. One
// wrong-typed value makes Claude Code drop the whole --settings document
// without a word in -p mode — permissions, sandbox, and disableAllHooks
// together — and the reviewer then runs unconfined. That happened once:
// allowMachLookup is an array of macOS service names, not a boolean, and
// `false` voided the document. It is left out (absent means no extra
// lookups). TestReviewSettingsJSON_ValidatesAgainstTheSettingsSchema checks
// every emitted key against the vendored schema, and
// TestReviewSettingsJSON_ClaudeDoctorAcceptsIt against the installed claude.
type sandboxNetwork struct {
	AllowedDomains      []string `json:"allowedDomains"`
	StrictAllowlist     bool     `json:"strictAllowlist"`
	AllowLocalBinding   bool     `json:"allowLocalBinding"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
}

// reviewSandbox builds the sandbox block for a review of workspace. ghHost is
// the PR's host for a remote review, or "" for a local one, which reaches no
// network at all.
func reviewSandbox(workspace, ghHost string) (sandboxSettings, error) {
	denyWrite, err := reviewDenyWrite(workspace)
	if err != nil {
		return sandboxSettings{}, err
	}
	domains := []string{}
	if ghHost != "" {
		domains = ghAPIDomains(ghHost)
	}
	return sandboxSettings{
		Enabled:                  true,
		FailIfUnavailable:        true,
		AllowUnsandboxedCommands: false,
		AutoAllowBashIfSandboxed: false,
		Filesystem:               sandboxFilesystem{DenyWrite: denyWrite},
		Network:                  sandboxNetwork{AllowedDomains: domains, StrictAllowlist: true},
	}, nil
}

// reviewSettingsJSON renders the reviewer's whole configuration. A value of
// the wrong type anywhere in it voids the whole document (see
// sandboxNetwork); the schema test is what stands between an edit here and
// an unconfined reviewer.
//
// reviewSettingsJSON renders the reviewer's whole configuration — perms, the
// sandbox block, and disableAllHooks — as the inline JSON `claude --settings`
// takes. It is passed with --setting-sources reviewSettingSources, so nothing
// else is merged into it but managed settings; see reviewSettingSources.
func reviewSettingsJSON(workspace, ghHost string, perms permissions) (string, error) {
	sb, err := reviewSandbox(workspace, ghHost)
	if err != nil {
		return "", err
	}
	// termsafe:allow-raw-json a claude argv value, never command output
	data, err := json.Marshal(reviewSettings{Permissions: perms, Sandbox: sb, DisableAllHooks: true})
	if err != nil {
		return "", fmt.Errorf("marshal review settings: %w", err)
	}
	return string(data), nil
}

// ghAPIDomains are the hosts gh contacts to read a PR on host: the API host
// for github.com and GHE.com tenancies (api.HOST), and the host itself, which
// is where a GitHub Enterprise Server answers /api/v3.
func ghAPIDomains(host string) []string {
	if host == defaultGitHubHost || strings.HasSuffix(host, ".ghe.com") {
		return []string{host, "api." + host}
	}
	return []string{host}
}

// reviewDenyWrite returns the paths a reviewer must not write: the workspace
// and, when the workspace is a linked worktree (`pr local` makes one), the
// repository's shared git directory. Claude Code's sandbox opens that shared
// directory for writes on its own so git works in a worktree, and it is the
// operator's real repository.
//
// Each path is listed as given and, where it differs, symlink-resolved: macOS
// temp paths live under /var, a symlink to /private/var, and a deny that
// names only one spelling must not depend on which one the sandbox compares.
// A path holding a glob character is refused rather than listed.
func reviewDenyWrite(workspace string) ([]string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil, errors.New("refusing to build the review sandbox: the workspace path is not absolute")
	}
	paths := []string{filepath.Clean(workspace)}
	shared, err := linkedWorktreeGitDir(workspace)
	if err != nil {
		return nil, err
	}
	if shared != "" {
		paths = append(paths, shared)
	}
	out := make([]string, 0, 2*len(paths))
	seen := make(map[string]bool, 2*len(paths))
	for _, p := range paths {
		for _, spelling := range []string{p, resolvedOrSelf(p)} {
			// On Linux the sandbox mounts concrete paths and SKIPS a write
			// entry holding a glob character, so such a deny would be
			// dropped without a word. Refuse instead.
			if strings.ContainsAny(spelling, "*?[") {
				return nil, fmt.Errorf("refusing to build the review sandbox: %q holds a glob character, which the sandbox would ignore as a write deny", spelling)
			}
			if !seen[spelling] {
				seen[spelling] = true
				out = append(out, spelling)
			}
		}
	}
	return out, nil
}

// resolvedOrSelf is filepath.EvalSymlinks, falling back to p when p cannot
// be resolved: the unresolved spelling is always listed as well, so a failed
// resolution drops nothing.
func resolvedOrSelf(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// linkedWorktreeGitDir returns the shared (common) git directory of a linked
// worktree at workspace, or "" when workspace/.git is a directory (a clone)
// or absent. A worktree's `.git` is a file, `gitdir: <admin dir>`, and the
// admin dir's `commondir` names the shared directory relative to itself.
//
// It reads files forgectl's own `git worktree add` wrote, before the review
// agent has run: git refuses to check a `.git` path out of a commit, so the
// PR head cannot supply them. A `.git` file it cannot parse is an error
// rather than a skipped deny.
func linkedWorktreeGitDir(workspace string) (string, error) {
	dotGit := filepath.Join(workspace, ".git")
	info, err := os.Lstat(dotGit)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect the workspace's .git: %w", err)
	}
	if info.IsDir() {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Clean(dotGit))
	if err != nil {
		return "", fmt.Errorf("read the workspace's .git file: %w", err)
	}
	admin, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	admin = strings.TrimSpace(admin)
	if !ok || admin == "" {
		return "", errors.New("the workspace's .git file does not name a git directory")
	}
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(workspace, admin)
	}
	admin = filepath.Clean(admin)
	common, err := os.ReadFile(filepath.Clean(filepath.Join(admin, "commondir")))
	if errors.Is(err, os.ErrNotExist) {
		return admin, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the worktree's commondir: %w", err)
	}
	dir := strings.TrimSpace(string(common))
	if dir == "" {
		return admin, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(admin, dir)
	}
	return filepath.Clean(dir), nil
}

// Seams for claudeSandboxSupported, so its platform table is testable on one
// machine. Production values only.
var (
	sandboxGOOS     = runtime.GOOS
	sandboxLookPath = osexec.LookPath
	sandboxStat     = os.Stat
)

// seatbeltPath is the Seatbelt front end Claude Code's macOS sandbox runs.
const seatbeltPath = "/usr/bin/sandbox-exec"

// claudeSandboxSupported reports whether Claude Code's sandbox can run here,
// by the same dependency table Claude Code's own check uses: Seatbelt on
// macOS, bubblewrap and socat on Linux, and nothing elsewhere.
//
// It is the clear-message half of fail-closed. failIfUnavailable in the
// sandbox block is the authoritative half, since this cannot see every
// reason bubblewrap might fail to start (an AppArmor user-namespace
// restriction, an unprivileged container). But when failIfUnavailable fires,
// claude exits inside a detached window, where the refusal is an empty pane.
// Refusing here puts the reason in front of the operator instead.
func claudeSandboxSupported() error {
	switch sandboxGOOS {
	case "darwin":
		if _, err := sandboxStat(seatbeltPath); err != nil {
			return fmt.Errorf("the Claude reviewer runs under Claude Code's sandbox, which needs %s: %w", seatbeltPath, err)
		}
		return nil
	case "linux":
		var missing []string
		for _, bin := range []string{"bwrap", "socat"} {
			if _, err := sandboxLookPath(bin); err != nil {
				missing = append(missing, bin)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("the Claude reviewer runs under Claude Code's sandbox, which needs %s on PATH "+
				"(install bubblewrap and socat; see https://code.claude.com/docs/en/sandboxing)", strings.Join(missing, " and "))
		}
		return nil
	default:
		return fmt.Errorf("the Claude reviewer runs under Claude Code's sandbox, which does not support %s", sandboxGOOS)
	}
}

// reviewGitEnv pins git configuration for every git process in a review
// window, the reviewer's and anything it starts. It is defence in depth under
// the sandbox, not a boundary, and it narrows rather than closes:
//
//   - GIT_CONFIG_NOSYSTEM drops the system-wide config file.
//   - GIT_CONFIG_COUNT/KEY/VALUE set configuration at command scope, which
//     outranks the repository's own .git/config. core.fsmonitor=false and
//     core.hooksPath=/dev/null turn off the two settings that make an
//     ordinary read (`git status`, `git diff`) run a configured command.
//
// Other configured commands remain (diff and filter drivers, gpg.program,
// among others). Enumerating them is the deny-by-text approach forgectl#694
// rejects; the sandbox narrows what they can do. GIT_CONFIG_GLOBAL and
// GIT_CONFIG_PARAMETERS are left alone: they are operator-controlled (the
// operator's own global config, and whatever the tmux server's environment
// carries), not PR-supplied, and tmux cannot unset a variable anyway.
var reviewGitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_COUNT=2",
	"GIT_CONFIG_KEY_0=core.fsmonitor",
	"GIT_CONFIG_VALUE_0=false",
	"GIT_CONFIG_KEY_1=core.hooksPath",
	"GIT_CONFIG_VALUE_1=/dev/null",
}

// pinReviewGitEnv returns env with reviewGitEnv applied: any entry env carries
// for one of those variables is dropped, then the pins are appended. env is
// not modified.
func pinReviewGitEnv(env []string) []string {
	drop := make(map[string]bool, len(reviewGitEnv))
	for _, e := range reviewGitEnv {
		key, _, _ := strings.Cut(e, "=")
		drop[key] = true
	}
	out := make([]string, 0, len(env)+len(reviewGitEnv))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		// Every indexed pair goes, not only the ones reviewGitEnv sets: a
		// stale GIT_CONFIG_KEY_5 is inert under COUNT=2 today, and should
		// not become live if the pin list grows.
		if drop[key] || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, e)
	}
	return append(out, reviewGitEnv...)
}
