// Package workflow is the ops layer for the forgectl workflow DSL (#9): parse
// a TOML step list, resolve it against CLI params into a Plan, and execute
// each step through the exec.Runner seam. It knows nothing of Cobra — that
// decoupling mirrors internal/tmux and internal/projects.
//
// Pipeline (ADR-0002): parse → resolve → verify → plan → execute.
package workflow

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/BurntSushi/toml"
)

// SupportedDSLVersions is the set of dsl_version values this executor
// understands (ADR-0004). The parser gates on this FIRST, before touching any
// other field, so a file claiming a newer grammar is refused rather than
// partially misread.
var SupportedDSLVersions = map[int]bool{
	1: true,
}

// Workflow is the parsed form of a *.workflow.toml file.
type Workflow struct {
	DSLVersion  int              `toml:"dsl_version"`
	Name        string           `toml:"name"`
	Version     string           `toml:"version"`
	Description string           `toml:"description"`
	Params      map[string]Param `toml:"params"`
	Steps       []Step           `toml:"step"`
}

// Param is one [params.<name>] declaration: a required flag XOR a default,
// plus a help string shown by `workflow list`/help output.
type Param struct {
	Required bool   `toml:"required"`
	Default  string `toml:"default"`
	Help     string `toml:"help"`
}

// Step is one [[step]] table. Uses selects the StepRunner; every other field
// is domain-specific to that runner, so Step keeps them as a raw string map
// (post-interpolation, string values are all a workflow file needs — see
// ADR-0001's "flat file, human-authorable" rationale) plus a couple of
// commonly-typed fields (Globs) that benefit from a real slice.
type Step struct {
	Uses    string   `toml:"uses"`
	Repo    string   `toml:"repo"`
	Ref     string   `toml:"ref"`
	Globs   []string `toml:"globs"`
	Skill   string   `toml:"skill"`
	Posture string   `toml:"posture"`
	Mode    string   `toml:"mode"`
	From    string   `toml:"from"`
	To      string   `toml:"to"`
	Cmd     string   `toml:"cmd"`
	Args    []string `toml:"args"`
}

// UnsupportedDSLVersionError is returned when a workflow file declares a
// dsl_version outside SupportedDSLVersions. It is typed (rather than a bare
// fmt.Errorf) so callers can distinguish "this file speaks a grammar I don't
// understand" from any other parse failure — ADR-0004's "typed refusal before
// planning" security property.
type UnsupportedDSLVersionError struct {
	Got       int
	Supported []int
}

func (e *UnsupportedDSLVersionError) Error() string {
	return fmt.Sprintf("unsupported dsl_version %d (supported: %v)", e.Got, e.Supported)
}

// Parse decodes raw TOML bytes into a Workflow. The dsl_version gate runs
// FIRST — before any other field is trusted — per ADR-0004: an unknown
// version is refused outright, never partially interpreted.
func Parse(data []byte) (Workflow, error) {
	slog.Debug("Preparing to parse workflow from TOML.", "byteLength", len(data))

	// Decode into a minimal shape first so an unsupported dsl_version is
	// caught even if the rest of the file is malformed for the version we'd
	// otherwise assume.
	var versionProbe struct {
		DSLVersion int `toml:"dsl_version"`
	}
	if _, err := toml.Decode(string(data), &versionProbe); err != nil {
		slog.Error("Failed to parse workflow: invalid TOML.", "error", err)
		return Workflow{}, fmt.Errorf("parse workflow: %w", err)
	}
	slog.Debug("Parsed workflow dsl_version probe.", "dslVersion", versionProbe.DSLVersion)

	if !SupportedDSLVersions[versionProbe.DSLVersion] {
		supported := supportedVersionsList()
		slog.Warn("Rejecting workflow: unsupported dsl_version.", "dslVersion", versionProbe.DSLVersion, "supported", supported)
		return Workflow{}, &UnsupportedDSLVersionError{
			Got:       versionProbe.DSLVersion,
			Supported: supported,
		}
	}

	var wf Workflow
	md, err := toml.Decode(string(data), &wf)
	if err != nil {
		slog.Error("Failed to parse workflow: malformed TOML.", "dslVersion", versionProbe.DSLVersion, "error", err)
		return Workflow{}, fmt.Errorf("parse workflow: %w", err)
	}
	// The full decode is strict: an unknown key is refused, not ignored. A
	// typo'd field would otherwise silently no-op — on a `strip` step, a
	// misspelled `globs` means silently falling back to the default strip-list
	// in the one step that is a security control — and a newer grammar's
	// fields must not be silently dropped under an older dsl_version.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		slog.Warn("Rejecting workflow: unknown keys.", "keys", keys)
		return Workflow{}, fmt.Errorf("parse workflow: unknown key(s) %s — a typo, or a field from a newer dsl_version?", strings.Join(keys, ", "))
	}
	slog.Debug("Successfully parsed workflow.", "name", wf.Name, "version", wf.Version, "stepCount", len(wf.Steps))
	return wf, nil
}

// supportedVersionsList returns SupportedDSLVersions as a sorted slice for
// error messages.
func supportedVersionsList() []int {
	out := make([]int, 0, len(SupportedDSLVersions))
	for v := range SupportedDSLVersions {
		out = append(out, v)
	}
	// Simple insertion sort — the set is tiny (one entry today).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
