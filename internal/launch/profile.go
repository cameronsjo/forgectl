package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// EffortLevels are the values Claude Code's `--effort` accepts, in ascending
// order. Read off `claude --help` on 2.1.221: "(low, medium, high, xhigh,
// max)". Exported so the error message and any future display share one list.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// Validate rejects harness-native settings Codex itself would refuse, and an
// effort level Claude Code would refuse.
//
// Effort is validated even for Codex and Pi profiles, where it is inert (only
// Claude's builders emit it). A typo that silently does nothing today becomes
// a live wrong value the moment a profile flips harness, and `effort` is new
// enough that no existing config can regress.
func (p Profile) Validate() error {
	if p.Harness != "claude" && p.Harness != "codex" && p.Harness != "pi" {
		return fmt.Errorf("unsupported launch harness %s: want claude, codex, or pi", termsafe.QuoteArgMax(p.Harness, 0))
	}
	if p.Effort != "" && !oneOf(p.Effort, EffortLevels...) {
		return fmt.Errorf(
			"unsupported effort %s: want one of %s",
			termsafe.QuoteArgMax(p.Effort, 0), strings.Join(EffortLevels, ", "),
		)
	}
	// An empty permission_mode is a profile that does not set one (doctor's
	// effort check builds such a profile); only a value Claude Code would not
	// accept is refused here. The worker floor refuses empty too.
	if p.Harness == "claude" && p.PermissionMode != "" {
		if err := claudePermissionRank.check(p.PermissionMode); err != nil {
			return err
		}
	}
	if p.Harness == "codex" {
		if oneOf(p.Model, "opus", "sonnet", "haiku") || strings.HasPrefix(p.Model, "claude-") {
			return fmt.Errorf(
				"Claude model %s cannot be used with Codex; remove model to use the Codex default or set a Codex model id",
				termsafe.QuoteArgMax(p.Model, 0),
			)
		}
		if err := codexApprovalRank.check(p.ApprovalPolicy); err != nil {
			return err
		}
		if err := codexSandboxRank.check(p.Sandbox); err != nil {
			return err
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
	Harness string
	Model   string
	// Provider is Pi's provider selector. Empty deliberately emits no flag and
	// leaves Pi's own configured/default provider in charge.
	Provider string
	// Effort is the resolved `--effort` level, or "" to emit no flag at all and
	// let Claude Code's own default (settings.json effortLevel) apply. It is
	// derived from Model when neither layer of config sets it — see
	// EffortForModel.
	Effort         string
	PermissionMode string
	AllowDanger    bool
	ApprovalPolicy string
	Sandbox        string
	Env            map[string]string
	AddDir         []string
	Match          string // original `match` of the winning project; "" when defaults-only

	// StrictMCP emits `--strict-mcp-config`, which makes Claude Code use ONLY
	// MCP servers passed via --mcp-config and ignore every discovered MCP
	// configuration. It is a security control for the clean-room review, where
	// the workspace holds a third party's checkout: a `.mcp.json` there is
	// executed at session start, before the agent calls any tool, so plan mode
	// and the workspace allowlist are both downstream of it.
	//
	// Deliberately NOT surfaced in config and NOT defaulted on: it is set by the
	// review dispatch alone (internal/pr launchInline). An operator's ordinary
	// `forgectl launch` must keep its MCP servers — flipping this globally would
	// strip every user's tools with no error message.
	//
	// Claude-only. Codex has no equivalent flag and launchCodex builds a fresh
	// Profile that never sets this, so it cannot silently no-op there.
	StrictMCP bool

	// Detached omits `--ide`, so the session connects to no editor and none of
	// its MCP tools (which can run code there). Set by the worker floor alone,
	// and not surfaced in config: a worker has no editor of its own.
	Detached bool
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

// ErrHomeUnresolved reports that the launch config uses a home-relative path
// ("~" or "~/...") but the home directory cannot be determined.
var ErrHomeUnresolved = errors.New("launch: the home directory cannot be resolved")

// Resolve picks the profile for cwd: it resolves symlinks best-effort, makes the
// path absolute, then applies the pure resolution against the launch config.
//
// The home directory is looked up only when the config needs it (a project
// match or an add_dir that starts with "~"). Where it is needed and cannot be
// found, Resolve fails rather than matching with an empty home: a project
// block that silently stops matching would launch under the defaults instead,
// and the defaults can be the looser posture. A config with no home-relative
// paths launches exactly as before, home or no home.
func Resolve(lc config.LaunchConfig, cwd string) (Profile, error) {
	return resolveWithHome(lc, cwd, os.UserHomeDir)
}

// MatchedProject returns the [[launch.project]] block that Resolve would apply
// to cwd, or nil when none matches. The worker profile reads its own values,
// not the merged profile, so a field the block leaves unset is told apart
// from one [launch.defaults] filled.
func MatchedProject(lc config.LaunchConfig, cwd string) (*config.LaunchProject, error) {
	home, err := homeIfNeeded(os.UserHomeDir, projectsUseHome(lc.Projects))
	if err != nil {
		return nil, err
	}
	return matchProject(lc, normalizeCWD(cwd), home), nil
}

// matchProject is the longest project match for an already normalized cwd.
func matchProject(lc config.LaunchConfig, cwd, home string) *config.LaunchProject {
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
	return win
}

// normalizeCWD resolves cwd's symlinks best-effort and makes it absolute and
// clean, as project matching expects.
func normalizeCWD(cwd string) string {
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	return filepath.Clean(resolved)
}

// resolveWithHome is Resolve with the home lookup injected.
func resolveWithHome(lc config.LaunchConfig, cwd string, userHome func() (string, error)) (Profile, error) {
	home, err := homeIfNeeded(userHome, usesHome(lc.Defaults.AddDir) || projectsUseHome(lc.Projects))
	if err != nil {
		return Profile{}, err
	}
	return resolve(lc, normalizeCWD(cwd), home), nil
}

// DefaultsProfile resolves [launch.defaults] alone (no project matching), for
// display by `forgectl config`. Built-in fallbacks are applied. Only the
// defaults' own add_dir can need home; on ErrHomeUnresolved the returned
// profile is still the defaults with those entries left unexpanded, good for
// display and not for launching.
func DefaultsProfile(lc config.LaunchConfig) (Profile, error) {
	return defaultsProfileWithHome(lc, os.UserHomeDir)
}

func defaultsProfileWithHome(lc config.LaunchConfig, userHome func() (string, error)) (Profile, error) {
	home, err := homeIfNeeded(userHome, usesHome(lc.Defaults.AddDir))
	return defaultsProfile(lc.Defaults, home), err
}

// CheckHome reports ErrHomeUnresolved when any home-relative path in lc (a
// defaults or project add_dir, or a project match) cannot be expanded. It is
// for display surfaces that show a profile without resolving one.
func CheckHome(lc config.LaunchConfig) error {
	_, err := homeIfNeeded(os.UserHomeDir, usesHome(lc.Defaults.AddDir) || projectsUseHome(lc.Projects))
	return err
}

// homeIfNeeded looks the home directory up only when needed, so a config
// without home-relative paths never depends on it.
func homeIfNeeded(userHome func() (string, error), needed bool) (string, error) {
	if !needed {
		return "", nil
	}
	home, err := userHome()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrHomeUnresolved, err)
	}
	return home, nil
}

func usesHome(paths []string) bool {
	for _, p := range paths {
		if p == "~" || strings.HasPrefix(p, "~/") {
			return true
		}
	}
	return false
}

func projectsUseHome(ps []config.LaunchProject) bool {
	for i := range ps {
		if usesHome([]string{ps[i].Match}) || usesHome(ps[i].AddDir) {
			return true
		}
	}
	return false
}

// resolve is the pure resolution: fixed home, already-clean absolute cwd. It is
// the risk-bearing core and is exercised directly by the tests.
func resolve(lc config.LaunchConfig, cwd, home string) Profile {
	p := defaultsProfile(lc.Defaults, home)
	win := matchProject(lc, cwd, home)

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
		if win.Provider != "" {
			p.Provider = win.Provider
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

	// Effort derivation is the LAST statement, and it runs against the FINAL
	// model — so a project block overriding only `model` re-derives its level
	// instead of inheriting one chosen for the defaults' model. The explicit
	// value is recomputed from the raw config rather than read back off
	// p.Effort, because defaultsProfile has already derived into that field and
	// a derived "medium" is indistinguishable from a configured one.
	explicitEffort := lc.Defaults.Effort
	if win != nil && win.Effort != "" {
		explicitEffort = win.Effort
	}
	p.Effort = firstNonEmpty(explicitEffort, EffortForModel(p.Model))

	return p
}

// defaultsProfile applies built-in fallbacks over [launch.defaults], with no
// project matching. Shared by resolve and DefaultsProfile.
func defaultsProfile(d config.LaunchDefaults, home string) Profile {
	harness := firstNonEmpty(d.Harness, builtinHarness)
	p := Profile{
		Harness:        harness,
		Model:          firstNonEmpty(d.Model, builtinModelForHarness(harness)),
		Provider:       d.Provider,
		PermissionMode: firstNonEmpty(d.PermissionMode, builtinPermissionMode),
		AllowDanger:    boolOr(d.AllowDanger, builtinAllowDanger),
		ApprovalPolicy: firstNonEmpty(d.ApprovalPolicy, builtinApprovalPolicy),
		Sandbox:        firstNonEmpty(d.Sandbox, builtinSandbox),
		Env:            mergeEnv(nil, d.Env),
		AddDir:         expandAll(d.AddDir, home),
	}
	// Last statement, against the final model — the same ordering resolve
	// relies on, and the reason both derive here rather than in the exported
	// Resolve/DefaultsProfile wrappers: this is the core the tests exercise
	// directly, so deriving one level up would leave every merge test seeing
	// an empty Effort.
	//
	// resolve DISCARDS this line's result unconditionally, recomputing the same
	// formula against the final model — it merely lands on the same value when
	// no project block matched. That is deliberate,
	// not redundancy to collapse: Effort is a plain string, so a value read back
	// off the struct cannot say whether it was configured or derived — and once
	// a project overrides Model, only the raw config can answer that. The
	// alternative is a provenance flag on Profile, which buys two saved string
	// comparisons per launch for a permanently wider data model.
	p.Effort = firstNonEmpty(d.Effort, EffortForModel(p.Model))
	return p
}

// EffortForModel maps a model alias to the reasoning effort that suits it, or
// "" when the model is unmapped. It is pure, and it is the last word in both
// resolve and defaultsProfile — so a project block overriding only `model`
// re-derives its level, rather than inheriting one chosen for a different
// model.
//
// The mapping is deliberately alias-only and deliberately incomplete. Claude
// Code accepts a full model id (claude-opus-5) and other aliases (haiku,
// opusplan) here too, but the effort semantics of those are unverified, and
// guessing a level is worse than emitting no flag: "" leaves the user's
// settings.json effortLevel in charge, which is exactly the pre-existing
// behavior. Adding an entry is a claim that the level was measured on that
// model. opusplan is excluded on purpose — it runs opus for planning and
// sonnet for execution, and this mapping puts those two at DIFFERENT levels,
// so there is no single honest answer for the pair.
//
// The "[1m]" suffix is stripped first. Claude Code 2.1.221 accepts opus[1m],
// sonnet[1m], fable[1m], and opusplan[1m]; the suffix selects the 1M-token
// context window, not a different model, so effort carries over unchanged.
// Matching the bare alias only would silently drop a 1M-context launch to no
// flag at all — the one failure mode that looks identical to working.
func EffortForModel(model string) string {
	switch strings.TrimSuffix(model, "[1m]") {
	case "sonnet":
		return "high"
	case "opus", "fable":
		return "medium"
	default:
		return ""
	}
}

func builtinModelForHarness(harness string) string {
	if harness == "claude" {
		return builtinModel
	}
	return ""
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

// SortedEnvKeys returns the profile env keys in deterministic order, for
// display. Go randomises map iteration, so `forgectl launch which` would
// otherwise reorder its env row run to run. Env assembly for the exec does not
// come through here — MergeEnv sorts its own output.
func SortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
