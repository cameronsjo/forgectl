package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Validate rejects harness-native settings Codex itself would refuse.
func (p Profile) Validate() error {
	if p.Harness != "claude" && p.Harness != "codex" {
		return fmt.Errorf("unsupported launch harness %q: want claude or codex", p.Harness)
	}
	if p.Harness == "codex" {
		if oneOf(p.Model, "opus", "sonnet", "haiku") || strings.HasPrefix(p.Model, "claude-") {
			return fmt.Errorf(
				"Claude model %q cannot be used with Codex; remove model to use the Codex default or set a Codex model id",
				p.Model,
			)
		}
		if !oneOf(p.ApprovalPolicy, "untrusted", "on-request", "never") {
			return fmt.Errorf("unsupported Codex approval_policy %q", p.ApprovalPolicy)
		}
		if !oneOf(p.Sandbox, "read-only", "workspace-write", "danger-full-access") {
			return fmt.Errorf("unsupported Codex sandbox %q", p.Sandbox)
		}
	}
	return nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

// Profile is the fully resolved posture for one working directory — the on-disk
// schema (config.LaunchConfig) reduced against the cwd.
type Profile struct {
	Harness        string
	Model          string
	PermissionMode string
	AllowDanger    bool
	ApprovalPolicy string
	Sandbox        string
	Env            map[string]string
	AddDir         []string
	Match          string // original `match` of the winning project; "" when defaults-only
}

// Built-in fallbacks applied when a value is set neither by a project nor by
// [launch.defaults] — and the entire posture when no config exists.
//
// builtinSandbox is "read-only" because it is the Codex analogue of
// builtinPermissionMode's "plan": launch always starts in a posture that
// cannot write. A bare `harness = "codex"` with nothing else set must not hand
// `forgectl launch "<prompt>"` unattended write access to the whole checkout —
// "workspace-write" is an explicit opt-up, exactly like allow_danger.
const (
	builtinHarness        = "claude"
	builtinModel          = "opus"
	builtinPermissionMode = "plan"
	builtinAllowDanger    = true
	builtinApprovalPolicy = "on-request"
	builtinSandbox        = "read-only"
)

// Resolve picks the profile for cwd: it resolves symlinks best-effort, makes the
// path absolute, then applies the pure resolution against the launch config.
func Resolve(lc config.LaunchConfig, cwd string) Profile {
	home, _ := os.UserHomeDir()
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	return resolve(lc, filepath.Clean(resolved), home)
}

// DefaultsProfile resolves [launch.defaults] alone (no project matching), for
// display by `forgectl config`. Built-in fallbacks are applied.
func DefaultsProfile(lc config.LaunchConfig) Profile {
	home, _ := os.UserHomeDir()
	return defaultsProfile(lc.Defaults, home)
}

// resolve is the pure resolution: fixed home, already-clean absolute cwd. It is
// the risk-bearing core and is exercised directly by the tests.
func resolve(lc config.LaunchConfig, cwd, home string) Profile {
	p := defaultsProfile(lc.Defaults, home)

	best := -1
	var win *config.LaunchProject
	for i := range lc.Projects {
		if lc.Projects[i].Match == "" {
			continue
		}
		m := filepath.Clean(expandTilde(lc.Projects[i].Match, home))
		if cwd == m || strings.HasPrefix(cwd, m+string(filepath.Separator)) {
			if len(m) > best {
				best = len(m)
				win = &lc.Projects[i]
			}
		}
	}

	if win != nil {
		if win.Harness != "" {
			if win.Harness != p.Harness && win.Model == "" {
				p.Model = builtinModelForHarness(win.Harness)
			}
			p.Harness = win.Harness
		}
		if win.Model != "" {
			p.Model = win.Model
		}
		if win.PermissionMode != "" {
			p.PermissionMode = win.PermissionMode
		}
		if win.AllowDanger != nil {
			p.AllowDanger = *win.AllowDanger
		}
		if win.ApprovalPolicy != "" {
			p.ApprovalPolicy = win.ApprovalPolicy
		}
		if win.Sandbox != "" {
			p.Sandbox = win.Sandbox
		}
		p.Env = mergeEnv(p.Env, win.Env)
		p.AddDir = dedupe(append(p.AddDir, expandAll(win.AddDir, home)...))
		p.Match = win.Match
	}

	return p
}

// defaultsProfile applies built-in fallbacks over [launch.defaults], with no
// project matching. Shared by resolve and DefaultsProfile.
func defaultsProfile(d config.LaunchDefaults, home string) Profile {
	harness := firstNonEmpty(d.Harness, builtinHarness)
	return Profile{
		Harness:        harness,
		Model:          firstNonEmpty(d.Model, builtinModelForHarness(harness)),
		PermissionMode: firstNonEmpty(d.PermissionMode, builtinPermissionMode),
		AllowDanger:    boolOr(d.AllowDanger, builtinAllowDanger),
		ApprovalPolicy: firstNonEmpty(d.ApprovalPolicy, builtinApprovalPolicy),
		Sandbox:        firstNonEmpty(d.Sandbox, builtinSandbox),
		Env:            mergeEnv(nil, d.Env),
		AddDir:         expandAll(d.AddDir, home),
	}
}

func builtinModelForHarness(harness string) string {
	if harness == "codex" {
		return ""
	}
	return builtinModel
}

// DefaultModelFor exposes the built-in model for a harness, for callers that
// must re-derive it after forcing a harness — the clean-room review, which
// dispatches claude regardless of the user's ambient profile and so cannot
// carry a Codex model id across.
func DefaultModelFor(harness string) string { return builtinModelForHarness(harness) }

// expandTilde expands a leading ~ or ~/ to the home directory. A bare "~user"
// form is left untouched (the launcher does not resolve other users' homes).
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// expandAll tilde-expands and cleans each entry.
func expandAll(dirs []string, home string) []string {
	if len(dirs) == 0 {
		return nil
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = filepath.Clean(expandTilde(d, home))
	}
	return out
}

// dedupe drops repeats while preserving first-seen order.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// mergeEnv returns base with over layered on top (over wins on collision).
func mergeEnv(base, over map[string]string) map[string]string {
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

// SortedEnvKeys returns the profile env keys in deterministic order, for display
// and for stable argv/env assembly.
func SortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
