package pr

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// allowReadOnly is the ONLY set of permitted actions: read the tree and run
// read-only inspection. Every entry is inspection, never mutation or posting.
var allowReadOnly = []string{
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
	"Bash(rg:*)",
	"Bash(gh pr view:*)",
	"Bash(gh pr diff:*)",
	"Bash(gh pr checks:*)",
}

// denyPosting explicitly denies every write/post/merge/push surface, so that
// even if a permission mode is relaxed the agent still cannot post a review,
// comment, merge, push, or fetch arbitrary URLs. Deny takes precedence over
// allow in Claude Code's permission model, so these are hard blocks.
var denyPosting = []string{
	"Bash(gh pr review:*)",
	"Bash(gh pr comment:*)",
	"Bash(gh pr merge:*)",
	"Bash(gh pr close:*)",
	"Bash(gh pr edit:*)",
	"Bash(gh api:*)",
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
func writeAllowlist(workspace string) (string, error) {
	slog.Debug("Preparing to write clean-room allowlist.", "workspace", workspace)
	dir := filepath.Join(workspace, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("Failed to create allowlist dir.", "dir", dir, "error", err)
		return "", fmt.Errorf("create allowlist dir: %w", err)
	}
	settings := allowlistSettings{
		Permissions: permissions{
			DefaultMode: "plan",
			Allow:       allowReadOnly,
			Deny:        denyPosting,
		},
	}
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
