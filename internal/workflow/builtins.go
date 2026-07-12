package workflow

import (
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// ErrInvalidWorkflowName is returned for a name that is not a single, clean path
// element. A name is joined onto a directory (<dir>/<name>.workflow.toml), so a
// separator or a ".." element would let `workflow run ../../x` read an arbitrary
// file and `workflow bless ../../x` drop a .blessing sidecar anywhere the user
// can write. Neither is a bypass — a run still needs a valid sidecar, a bless
// still needs the presence ceremony — but a name is a NAME, so it is refused up
// front, before any filesystem access.
var ErrInvalidWorkflowName = errors.New("invalid workflow name")

// builtinFS embeds the shipped reference workflow(s). A new built-in is added
// by dropping a *.workflow.toml file in builtins/ — go:embed picks it up with
// no other code change.
//
//go:embed builtins/*.workflow.toml
var builtinFS embed.FS

// Source is a located workflow's raw bytes plus its provenance. Path is the
// on-disk location for a user file ("" for a built-in). Builtin is BYTE
// provenance — true ONLY when Data came from the embed FS; a user file that
// shadows a built-in name is Builtin=false. The run and bless paths key their
// policy on this flag (built-ins are exempt from blessing), so it must reflect
// where the bytes came from, never a name match.
type Source struct {
	Name    string
	Path    string
	Data    []byte
	Builtin bool
}

// Load locates a workflow by name and returns its bytes without parsing: a user
// file under <config-dir>/workflows/<name>.workflow.toml (config.WorkflowsDir —
// macOS: ~/Library/Application Support/forgectl, Linux: ~/.config/forgectl)
// wins over an embedded built-in of the same name; else the embedded built-in;
// else a not-found error. The bytes are read exactly once here so the caller
// can verify and parse the SAME buffer — TOCTOU is closed by construction.
func Load(name string) (Source, error) {
	slog.Debug("Loading workflow by name.", "workflowName", name)

	if err := validateWorkflowName(name); err != nil {
		slog.Warn("Rejecting workflow name.", "workflowName", name, "error", err)
		return Source{}, err
	}

	if path, ok := userWorkflowPath(name); ok {
		slog.Debug("Loading workflow from user directory.", "workflowName", name, "path", path)
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("Failed to read user workflow file.", "workflowName", name, "path", path, "error", err)
			return Source{}, fmt.Errorf("read workflow %q: %w", name, err)
		}
		return Source{Name: name, Path: path, Data: data, Builtin: false}, nil
	}

	slog.Debug("User workflow not found, checking built-ins.", "workflowName", name)
	data, err := builtinFS.ReadFile("builtins/" + name + ".workflow.toml")
	if err != nil {
		userDir := userWorkflowDir()
		slog.Warn("Workflow not found in user directory or built-ins.", "workflowName", name, "userDir", userDir)
		return Source{}, fmt.Errorf("workflow %q not found (checked %s and built-ins): %w", name, userDir, err)
	}
	slog.Debug("Loaded workflow from built-ins.", "workflowName", name)
	return Source{Name: name, Path: "", Data: data, Builtin: true}, nil
}

// Resolve locates a workflow by name and parses it — a thin Load+Parse wrapper
// retained for callers (and tests) that want the parsed Workflow directly and
// need no byte-level provenance. The blessing-aware run path uses Load so it can
// authenticate the raw bytes before parsing them.
func Resolve(name string) (Workflow, error) {
	src, err := Load(name)
	if err != nil {
		return Workflow{}, err
	}
	return Parse(src.Data)
}

// validateWorkflowName enforces that a workflow name is a single, clean path
// element: no separator, no ".." (or ".") element, nothing filepath.Clean would
// rewrite. It runs before any path is built or any file is touched.
func validateWorkflowName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidWorkflowName)
	}
	// Both separators are rejected on every OS: a "..\\x" name must not depend on
	// which platform's filepath rules happen to be compiled in.
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: %q contains a path separator — a workflow name is a single name, not a path", ErrInvalidWorkflowName, name)
	}
	if name == "." || name == ".." || filepath.Clean(name) != name {
		return fmt.Errorf("%w: %q is not a plain workflow name", ErrInvalidWorkflowName, name)
	}
	return nil
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
	if err := validateWorkflowName(name); err != nil {
		return Workflow{}, err
	}
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
