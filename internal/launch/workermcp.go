package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A claude worker starts where nobody can answer a dialog. Claude Code asks
// about every server in a project-scope .mcp.json it has no answer for yet
// ("New MCP servers found in this project"), and it keeps those answers per
// project directory, so every new worktree asks again. It reads a .mcp.json
// in the directory it starts in and in every directory above it, so a worker
// at <repo>/.claude/worktrees/<name> asks about <repo>/.mcp.json and
// <repo>/.claude/.mcp.json too; one worker waited five hours on that dialog
// (forgectl#1456).
//
// A worker therefore gets, in its --settings, a disabledMcpjsonServers list
// naming every such server the operator has not already approved. That is
// the setting the dialog itself writes for "no", Claude Code checks it before
// any approval, and its settings merge takes the union of every layer's
// list, so it reaches the dialog from the --settings layer: measured on
// Claude Code 2.1.327, `claude --settings '{"disabledMcpjsonServers":["x"]}'
// mcp list` stops listing a parent directory's server x as "Pending
// approval", and leaves every user and plugin server as it was.
//
// --strict-mcp-config with an explicit --mcp-config was the other way, and
// was declined: it drops every MCP server but the ones passed, the operator's
// user and plugin servers included, which a worker is meant to keep (ADR-0010,
// 2026-10-08 amendment).
//
// A server the operator approved stays: enableAllProjectMcpServers, or its
// name in enabledMcpjsonServers, in the worker's user settings or in the
// worktree's own project or local settings. debt: managed (policy) settings
// are not read, so a server only a policy approves is turned off; read them if
// a managed-settings estate ever runs workers.

// maxMCPConfigBytes caps one .mcp.json or settings file read for server
// names. Each is a few kilobytes; a larger one is read as having none.
const maxMCPConfigBytes = 1 << 20

// mcpApproval is what the settings files a worker loads approve.
type mcpApproval struct {
	all   bool
	names []string
}

// pendingProjectMCPServers returns the names in found the operator has not
// approved, sorted and without duplicates: the servers Claude Code would ask
// about at a worker's start.
func pendingProjectMCPServers(found []string, approved mcpApproval) []string {
	if approved.all {
		return nil
	}
	var out []string
	for _, name := range found {
		if strings.TrimSpace(name) == "" || slices.Contains(approved.names, name) || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// mcpFiles reads a file for the MCP lookups; a variable so tests never read
// the host's files.
var mcpFiles = readMCPFile

// readMCPFile returns the file at path, or nil when it is missing, not a
// regular file, unreadable, or larger than maxMCPConfigBytes. Every caller
// reads a nil file as naming nothing.
func readMCPFile(path string) []byte {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // G304: Claude Code's own config locations, read for names only
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxMCPConfigBytes+1))
	if err != nil || len(raw) > maxMCPConfigBytes {
		return nil
	}
	return raw
}

// projectMCPServerNames returns the server names of every .mcp.json from dir
// up to the filesystem root, in the order found. A file that does not parse
// names nothing; Claude Code skips it too.
func projectMCPServerNames(dir string) []string {
	var out []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if raw := mcpFiles(filepath.Join(d, ".mcp.json")); raw != nil && json.Unmarshal(raw, &doc) == nil {
			names := make([]string, 0, len(doc.MCPServers))
			for name := range doc.MCPServers {
				names = append(names, name)
			}
			slices.Sort(names)
			out = append(out, names...)
		}
		if filepath.Dir(d) == d {
			return out
		}
	}
}

// projectMCPApproval reads the approvals in the settings files a worker
// started in dir loads: the user settings in configDir ("" for none known),
// and the project and local settings in dir/.claude.
func projectMCPApproval(dir, configDir string) mcpApproval {
	files := []string{filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json")}
	if configDir != "" {
		files = append([]string{filepath.Join(configDir, "settings.json")}, files...)
	}
	var a mcpApproval
	for _, path := range files {
		var doc struct {
			EnableAll bool     `json:"enableAllProjectMcpServers"`
			Enabled   []string `json:"enabledMcpjsonServers"`
		}
		raw := mcpFiles(path)
		if raw == nil || json.Unmarshal(raw, &doc) != nil {
			continue
		}
		a.all = a.all || doc.EnableAll
		a.names = append(a.names, doc.Enabled...)
	}
	return a
}

// workerClaudeConfigDir is the user settings directory a claude worker with
// env loads: CLAUDE_CONFIG_DIR, else $HOME/.claude, else "" (unknown). It
// reads the worker's own environment, not forgectl's.
func workerClaudeConfigDir(env []string) string {
	if dir := envLookup(env, "CLAUDE_CONFIG_DIR"); filepath.IsAbs(dir) {
		return dir
	}
	if home := envLookup(env, "HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, ".claude")
	}
	return ""
}

// envLookup returns the last value of key in env, or "".
func envLookup(env []string, key string) string {
	v := ""
	for _, e := range env {
		if k, val, ok := strings.Cut(e, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// withMCPOff sets disabledMcpjsonServers to names in the worker settings
// JSON settings. Neither the built-in settings nor a [launch.worker] file can
// set that key, so it is never overwritten. No names leaves settings as is.
func withMCPOff(settings string, names []string) (string, error) {
	if len(names) == 0 {
		return settings, nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(settings), &doc); err != nil {
		return "", fmt.Errorf("launch: worker settings do not parse: %w", err)
	}
	doc["disabledMcpjsonServers"] = names
	// termsafe:allow-raw-json harness --settings argument, never printed
	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("launch: worker settings do not encode: %w", err)
	}
	return string(out), nil
}

// workerMCPOffNote is the operator note naming the servers a worker starts
// with turned off.
func workerMCPOffNote(names []string) string {
	return "project MCP servers turned off for this worker, since nobody is there to approve them: " + strings.Join(names, ", ") +
		" (to keep one, approve it in your user settings: enabledMcpjsonServers, or enableAllProjectMcpServers)"
}
