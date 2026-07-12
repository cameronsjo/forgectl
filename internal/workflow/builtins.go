package workflow

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/config"
)

// builtinFS embeds the shipped reference workflow(s). A new built-in is added
// by dropping a *.workflow.toml file in builtins/ — go:embed picks it up with
// no other code change.
//
//go:embed builtins/*.workflow.toml
var builtinFS embed.FS

// Resolve locates a workflow by name: a user file under
// <config-dir>/workflows/<name>.workflow.toml (config.WorkflowsDir — macOS:
// ~/Library/Application Support/forgectl, Linux: ~/.config/forgectl) takes
// precedence over an embedded built-in of the same name (design doc: "user
// dir overriding a built-in of the same name"), and parses whichever is found.
func Resolve(name string) (Workflow, error) {
	slog.Debug("Resolving workflow by name.", "workflowName", name)

	if path, ok := userWorkflowPath(name); ok {
		slog.Debug("Loading workflow from user directory.", "workflowName", name, "path", path)
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("Failed to read user workflow file.", "workflowName", name, "path", path, "error", err)
			return Workflow{}, fmt.Errorf("read workflow %q: %w", name, err)
		}
		return Parse(data)
	}

	slog.Debug("User workflow not found, checking built-ins.", "workflowName", name)
	data, err := builtinFS.ReadFile("builtins/" + name + ".workflow.toml")
	if err != nil {
		userDir := userWorkflowDir()
		slog.Warn("Workflow not found in user directory or built-ins.", "workflowName", name, "userDir", userDir)
		return Workflow{}, fmt.Errorf("workflow %q not found (checked %s and built-ins): %w", name, userDir, err)
	}
	slog.Debug("Loaded workflow from built-ins.", "workflowName", name)
	return Parse(data)
}

// userWorkflowPath returns the expected user-config path for name and
// whether a file actually exists there.
func userWorkflowPath(name string) (string, bool) {
	dir := userWorkflowDir()
	if dir == "" {
		return "", false
	}
	path := filepath.Join(dir, name+".workflow.toml")
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

// userWorkflowDir returns the user workflow directory (the OS config dir's
// forgectl/workflows), or "" if undeterminable. It defers to
// config.WorkflowsDir so the path is defined in exactly one place
// (internal/config).
func userWorkflowDir() string {
	dir, err := config.WorkflowsDir()
	if err != nil {
		return ""
	}
	return dir
}

// ResolveBuiltin parses an embedded built-in by name, bypassing the user
// directory — the data-plane coverage test uses it so a user override on the
// developer's machine can't mask a builtin's vocabulary. Resolve remains the
// runtime path.
func ResolveBuiltin(name string) (Workflow, error) {
	data, err := builtinFS.ReadFile("builtins/" + name + ".workflow.toml")
	if err != nil {
		return Workflow{}, fmt.Errorf("builtin workflow %q: %w", name, err)
	}
	return Parse(data)
}

// ListBuiltins returns the names of every embedded built-in workflow (without
// the .workflow.toml suffix) — used by `workflow list`.
func ListBuiltins() ([]string, error) {
	slog.Debug("Listing built-in workflows.")
	entries, err := builtinFS.ReadDir("builtins")
	if err != nil {
		slog.Error("Failed to list built-in workflows.", "error", err)
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		const suffix = ".workflow.toml"
		if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
			names = append(names, name[:len(name)-len(suffix)])
		}
	}
	slog.Debug("Listed built-in workflows.", "count", len(names))
	return names, nil
}
