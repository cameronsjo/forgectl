package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Role environment variables exported to every harness started with `--role`.
// Consumers such as cadence-hooks and cadence skills read this structured
// contract instead of matching prompt prefixes (#1515).
const (
	RoleEnv    = "FORGECTL_ROLE"
	PostureEnv = "FORGECTL_POSTURE"
)

// Supported [launch.roles.<name>] posture values.
const (
	RolePostureAutonomous  = "autonomous"
	RolePostureInteractive = "interactive"
)

// RolePostures lists the valid values for [launch.roles.<name>] posture.
var RolePostures = []string{RolePostureAutonomous, RolePostureInteractive}

var (
	// ErrRole reports an invalid [launch.roles.<name>] configuration.
	ErrRole = errors.New("launch: role")
	// ErrRoleNotFound reports a --role name that is not defined under [launch.roles].
	ErrRoleNotFound = errors.New("launch: unknown role")
	// ErrRoleUnsupportedHarness reports a role using Claude-only settings on a
	// non-Claude harness.
	ErrRoleUnsupportedHarness = errors.New("launch: role requires claude harness")
)

// quietHookEvents are the Claude Code hook events that cannot block an action
// (from cadence writing-hooks references/events.md and scripts/chief-of-staff.sh).
// A plugin declaring a hook on any other event stays enabled unless the
// operator explicitly opts into AllowUnsafePluginDisable.
var quietHookEvents = map[string]bool{
	"SessionStart":    true,
	"SessionEnd":      true,
	"SubagentStart":   true,
	"Notification":    true,
	"PostModelSwitch": true,
	"DirectoryAdded":  true,
	"WorktreeRemove":  true,
	"MessageDisplay":  true,
}

// remoteMarketplaceSources are the marketplace source types whose installed
// copy is guaranteed to be what Claude Code loads (scripts/chief-of-staff.sh).
var remoteMarketplaceSources = map[string]bool{
	"github": true,
	"git":    true,
	"url":    true,
}

// pluginKeyRegexp matches installed_plugins.json keys ("plugin@marketplace").
var pluginKeyRegexp = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+$`)

// maxPluginMetaBytes bounds plugin registry and hook JSON files read by the
// guard-hook safety check. Any larger file fails closed.
const maxPluginMetaBytes = 4 << 20

// ResolvedRole is the validated, filesystem-checked result of applying a
// [launch.roles.<name>] profile for one invocation.
type ResolvedRole struct {
	Name                     string
	Posture                  string
	CharterFile              string
	SessionName              string
	DisablePlugins           []string
	DisabledPlugins          []string
	GuardedPlugins           []string
	ExcludeClaudeMD          bool
	ClaudeMDExcludes         []string
	AllowUnsafePluginDisable bool
	Notes                    []string
}

// ResolveRole validates and resolves roleName from lc for harness, runDir, and
// env. allowGuardedOverride bypasses the fail-closed guard-hook safety check
// when the caller explicitly requested it (e.g. --allow-guarded-plugin-disable).
func ResolveRole(lc config.LaunchConfig, roleName, harness, runDir string, env []string, allowGuardedOverride bool) (ResolvedRole, error) {
	return resolveRoleWithHome(lc, roleName, harness, runDir, env, allowGuardedOverride, os.UserHomeDir)
}

func resolveRoleWithHome(lc config.LaunchConfig, roleName, harness, runDir string, env []string, allowGuardedOverride bool, userHome func() (string, error)) (ResolvedRole, error) {
	if strings.TrimSpace(roleName) == "" {
		return ResolvedRole{}, fmt.Errorf("%w: role name must not be empty", ErrRole)
	}
	role, ok := lc.Roles[roleName]
	if !ok {
		return ResolvedRole{}, fmt.Errorf("%w %s%s", ErrRoleNotFound, termsafe.QuoteArgMax(roleName, 0), availableRolesHint(lc.Roles))
	}
	if role.Posture != "" && !oneOf(role.Posture, RolePostures...) {
		return ResolvedRole{}, fmt.Errorf("%w %s: unsupported posture %s: want one of %s",
			ErrRole, termsafe.QuoteArgMax(roleName, 0), termsafe.QuoteArgMax(role.Posture, 0), strings.Join(RolePostures, ", "))
	}
	if role.SessionName != "" && !ValidSessionName(role.SessionName) {
		return ResolvedRole{}, fmt.Errorf("%w %s: session_name %s is not 1-%d of a-z 0-9 - starting with a letter or digit",
			ErrRole, termsafe.QuoteArgMax(roleName, 0), termsafe.QuoteArgMax(role.SessionName, 0), maxSessionNameLen)
	}

	hasClaudeOnly := role.CharterFile != "" || len(role.DisablePlugins) > 0 || role.ExcludeClaudeMD || len(role.ClaudeMDExcludes) > 0 || role.SessionName != ""
	if hasClaudeOnly && harness != "claude" {
		return ResolvedRole{}, fmt.Errorf("%w: role %s sets claude-only options (charter_file, disable_plugins, exclude_claude_md, claude_md_excludes, or session_name), which cannot be used with harness %s",
			ErrRoleUnsupportedHarness, termsafe.QuoteArgMax(roleName, 0), termsafe.QuoteArgMax(harness, 0))
	}

	needHome := usesHome([]string{role.CharterFile}) || usesHome(role.ClaudeMDExcludes)
	home, err := homeIfNeeded(userHome, needHome)
	if err != nil {
		return ResolvedRole{}, fmt.Errorf("%w %s: %w", ErrRole, termsafe.QuoteArgMax(roleName, 0), err)
	}

	res := ResolvedRole{
		Name:                     roleName,
		Posture:                  role.Posture,
		SessionName:              role.SessionName,
		DisablePlugins:           cloneStrings(role.DisablePlugins),
		ExcludeClaudeMD:          role.ExcludeClaudeMD,
		AllowUnsafePluginDisable: role.AllowUnsafePluginDisable || allowGuardedOverride,
	}

	if role.CharterFile != "" {
		charter := filepath.Clean(expandTilde(role.CharterFile, home))
		if !filepath.IsAbs(charter) {
			return ResolvedRole{}, fmt.Errorf("%w %s: charter_file %s is not an absolute path (a leading ~/ is expanded)",
				ErrRole, termsafe.QuoteArgMax(roleName, 0), termsafe.QuoteArgMax(role.CharterFile, 0))
		}
		if err := checkRegularReadableFile(charter); err != nil {
			return ResolvedRole{}, fmt.Errorf("%w %s: charter_file %s: %w",
				ErrRole, termsafe.QuoteArgMax(roleName, 0), termsafe.QuotePath(charter), err)
		}
		res.CharterFile = charter
	}

	if harness != "claude" {
		return res, nil
	}

	configDir := roleClaudeConfigDir(env, runDir, userHome)

	if len(role.DisablePlugins) > 0 {
		for _, name := range role.DisablePlugins {
			if err := checkSettingName("disable_plugins", name); err != nil {
				return ResolvedRole{}, fmt.Errorf("%w %s: %w", ErrRole, termsafe.QuoteArgMax(roleName, 0), err)
			}
		}
		disabled, guarded, notes := resolveDisabledPlugins(configDir, role.DisablePlugins, res.AllowUnsafePluginDisable)
		res.DisabledPlugins = disabled
		res.GuardedPlugins = guarded
		res.Notes = append(res.Notes, notes...)
	}

	var excludes []string
	if role.ExcludeClaudeMD {
		if configDir != "" {
			excludes = append(excludes,
				filepath.Join(configDir, "CLAUDE.md"),
				filepath.Join(configDir, "rules", "**"),
			)
		}
		if runDir != "" {
			excludes = append(excludes,
				filepath.Join(runDir, "CLAUDE.md"),
				filepath.Join(runDir, "AGENTS.md"),
			)
		}
	}
	for _, pat := range role.ClaudeMDExcludes {
		if err := checkSettingName("claude_md_excludes", pat); err != nil {
			return ResolvedRole{}, fmt.Errorf("%w %s: %w", ErrRole, termsafe.QuoteArgMax(roleName, 0), err)
		}
		if pat == "~" || strings.HasPrefix(pat, "~/") {
			pat = filepath.Clean(expandTilde(pat, home))
		}
		excludes = append(excludes, pat)
	}
	if len(excludes) > 0 {
		res.ClaudeMDExcludes = dedupe(excludes)
	}

	return res, nil
}

func availableRolesHint(roles map[string]config.LaunchRole) string {
	if len(roles) == 0 {
		return " (no roles configured under [launch.roles])"
	}
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return " (configured roles: " + strings.Join(names, ", ") + ")"
}

// checkRegularReadableFile verifies path exists, is a regular file, and can be
// opened for reading.
func checkRegularReadableFile(path string) error {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // G304: operator's config.toml names this charter file
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

// roleClaudeConfigDir resolves the Claude config directory for a role launch:
// CLAUDE_CONFIG_DIR from env (resolved against runDir if relative), else
// $HOME/.claude from env, else userHome()/.claude.
func roleClaudeConfigDir(env []string, runDir string, userHome func() (string, error)) string {
	if dir := envLookup(env, "CLAUDE_CONFIG_DIR"); dir != "" {
		dir = strings.TrimRight(dir, string(filepath.Separator))
		if !filepath.IsAbs(dir) && runDir != "" {
			dir = filepath.Join(runDir, dir)
		}
		if filepath.IsAbs(dir) {
			return filepath.Clean(dir)
		}
		return ""
	}
	if home := envLookup(env, "HOME"); filepath.IsAbs(home) {
		return filepath.Join(filepath.Clean(home), ".claude")
	}
	if userHome != nil {
		if home, err := userHome(); err == nil && filepath.IsAbs(home) {
			return filepath.Join(filepath.Clean(home), ".claude")
		}
	}
	return ""
}

// resolveDisabledPlugins implements the fail-closed guard-hook safety check
// ported from cadence-ecosystem/scripts/chief-of-staff.sh:79-153.
//
// A requested plugin stays ON (guarded) unless:
//   - allowUnsafe is explicitly true, OR
//   - its marketplace in known_marketplaces.json is remote (github, git, url)
//     with no source.path and an absolute installLocation, AND
//   - <installLocation>/.claude-plugin/marketplace.json has exactly one entry
//     for the plugin with neither a "hooks" key nor strict == false, AND
//   - every recorded installPath for the plugin is absolute, AND
//   - every hook declared in <installPath>/hooks/hooks.json and
//     <installPath>/.claude-plugin/plugin.json is on a non-blocking (quiet)
//     event, with hooks.json containing only "description"/"hooks" keys.
//
// Any unreadable, non-regular, or malformed file fails CLOSED (keeps the
// plugin enabled).
func resolveDisabledPlugins(configDir string, requested []string, allowUnsafe bool) (disabled, guarded, notes []string) {
	if configDir == "" {
		notes = append(notes, "could not determine Claude config dir, so no plugin is turned off")
		return nil, nil, notes
	}
	installedPath := filepath.Join(configDir, "plugins", "installed_plugins.json")
	rawInstalled, err := readRegularBounded(installedPath, maxPluginMetaBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			notes = append(notes, fmt.Sprintf("no %s, so no plugin is turned off", installedPath))
		} else {
			notes = append(notes, fmt.Sprintf("could not read %s, so no plugin is turned off", installedPath))
		}
		return nil, nil, notes
	}

	var installedDoc struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(rawInstalled, &installedDoc); err != nil {
		notes = append(notes, fmt.Sprintf("could not read %s, so no plugin is turned off", installedPath))
		return nil, nil, notes
	}

	reqSet := make(map[string]bool, len(requested))
	for _, name := range requested {
		reqSet[name] = true
	}

	var matched []string
	for key := range installedDoc.Plugins {
		if !pluginKeyRegexp.MatchString(key) {
			continue
		}
		pluginName, _, _ := strings.Cut(key, "@")
		if reqSet[pluginName] {
			matched = append(matched, key)
		}
	}
	sort.Strings(matched)
	if len(matched) == 0 {
		return nil, nil, nil
	}
	if allowUnsafe {
		return matched, nil, nil
	}

	knownPath := filepath.Join(configDir, "plugins", "known_marketplaces.json")
	knownMap := loadKnownMarketplaces(knownPath)

	for _, key := range matched {
		pluginName, marketplace, _ := strings.Cut(key, "@")
		paths, ok := extractInstallPaths(installedDoc.Plugins[key])
		if !ok || len(paths) == 0 || !marketplaceAllowsDisable(knownMap, marketplace, pluginName) {
			guarded = append(guarded, key)
			continue
		}
		keep := false
		for _, installPath := range paths {
			if installPath == "" || !filepath.IsAbs(installPath) {
				keep = true
				break
			}
			if !hookFileIsQuiet(filepath.Join(installPath, "hooks", "hooks.json"), true) ||
				!hookFileIsQuiet(filepath.Join(installPath, ".claude-plugin", "plugin.json"), false) {
				keep = true
				break
			}
		}
		if keep {
			guarded = append(guarded, key)
		} else {
			disabled = append(disabled, key)
		}
	}

	if len(guarded) > 0 {
		notes = append(notes, fmt.Sprintf("keeping %s on: a hook that can block, or an install that could not be checked", strings.Join(guarded, " ")))
	}
	return disabled, guarded, notes
}

type knownMarketplaceEntry struct {
	Source struct {
		Source string          `json:"source"`
		Path   json.RawMessage `json:"path"`
	} `json:"source"`
	InstallLocation string `json:"installLocation"`
}

func loadKnownMarketplaces(path string) map[string]knownMarketplaceEntry {
	raw, err := readRegularBounded(path, maxPluginMetaBytes)
	if err != nil {
		return nil
	}
	var m map[string]knownMarketplaceEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// marketplaceAllowsDisable checks that marketplace is a remote marketplace
// (github, git, url) with no source.path, an absolute installLocation, and a
// marketplace.json with exactly one entry for pluginName that neither declares
// a "hooks" key nor sets strict: false.
func marketplaceAllowsDisable(known map[string]knownMarketplaceEntry, marketplace, pluginName string) bool {
	if known == nil {
		return false
	}
	entry, ok := known[marketplace]
	if !ok {
		return false
	}
	if !remoteMarketplaceSources[entry.Source.Source] {
		return false
	}
	if len(entry.Source.Path) > 0 && !bytes.Equal(bytes.TrimSpace(entry.Source.Path), []byte("null")) {
		return false
	}
	if entry.InstallLocation == "" || !filepath.IsAbs(entry.InstallLocation) {
		return false
	}

	manifestPath := filepath.Join(entry.InstallLocation, ".claude-plugin", "marketplace.json")
	raw, err := readRegularBounded(manifestPath, maxPluginMetaBytes)
	if err != nil {
		return false
	}
	var manifest struct {
		Plugins []map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false
	}

	var matches []map[string]json.RawMessage
	for _, p := range manifest.Plugins {
		rawName, hasName := p["name"]
		if !hasName {
			continue
		}
		var name string
		if err := json.Unmarshal(rawName, &name); err != nil {
			continue
		}
		if name == pluginName {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return false
	}
	pluginEntry := matches[0]
	if _, hasHooks := pluginEntry["hooks"]; hasHooks {
		return false
	}
	if rawStrict, hasStrict := pluginEntry["strict"]; hasStrict {
		var strict bool
		if err := json.Unmarshal(rawStrict, &strict); err == nil && !strict {
			return false
		}
	}
	return true
}

// extractInstallPaths extracts all string installPath values from an
// installed_plugins.json entry, which may be a single object or an array of
// objects. Returns ok=false on malformed JSON or entries missing a string
// installPath.
func extractInstallPaths(raw json.RawMessage) ([]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	type installRec struct {
		InstallPath *string `json:"installPath"`
	}
	if trimmed[0] == '[' {
		var recs []installRec
		if err := json.Unmarshal(trimmed, &recs); err != nil {
			return nil, false
		}
		paths := make([]string, 0, len(recs))
		for _, r := range recs {
			if r.InstallPath == nil {
				return nil, false
			}
			paths = append(paths, *r.InstallPath)
		}
		return paths, len(paths) > 0
	}
	var rec installRec
	if err := json.Unmarshal(trimmed, &rec); err != nil || rec.InstallPath == nil {
		return nil, false
	}
	return []string{*rec.InstallPath}, true
}

// hookFileIsQuiet returns true when path does not exist, or when path is a
// regular JSON file whose declared hooks (if any) are exclusively on
// quietHookEvents. For hooks.json, any top-level key other than "description"
// or "hooks" fails closed. Any read/stat/parse error or non-regular file fails
// closed (returns false).
func hookFileIsQuiet(path string, isHooksJSON bool) bool {
	if _, err := os.Stat(path); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	raw, err := readRegularBounded(path, maxPluginMetaBytes)
	if err != nil {
		return false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return false
	}
	if isHooksJSON {
		for k := range doc {
			if k != "description" && k != "hooks" {
				return false
			}
		}
	}
	rawHooks, hasHooks := doc["hooks"]
	if !hasHooks || bytes.Equal(bytes.TrimSpace(rawHooks), []byte("null")) {
		return true
	}
	var hooksMap map[string]json.RawMessage
	if err := json.Unmarshal(rawHooks, &hooksMap); err != nil || hooksMap == nil {
		// Hooks declared as a path string, array, or other form not inspected
		// here: fail closed.
		return false
	}
	for event := range hooksMap {
		if !quietHookEvents[event] {
			return false
		}
	}
	return true
}

// readRegularBounded opens path, verifies on the open descriptor that it is a
// regular file (avoiding directories, FIFOs, and device nodes), and reads at
// most limit bytes.
func readRegularBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // G304: Claude plugin registry/manifest paths
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	return raw, nil
}

// roleHasSettings reports whether r produces any Claude --settings keys.
func roleHasSettings(r ResolvedRole) bool {
	return len(r.DisabledPlugins) > 0 || len(r.ClaudeMDExcludes) > 0
}

// roleSettingsJSON encodes r's Claude --settings JSON object, or "" when r
// sets no settings keys.
func roleSettingsJSON(r ResolvedRole) (string, error) {
	if !roleHasSettings(r) {
		return "", nil
	}
	doc := map[string]any{}
	if len(r.ClaudeMDExcludes) > 0 {
		doc["claudeMdExcludes"] = r.ClaudeMDExcludes
	}
	if len(r.DisabledPlugins) > 0 {
		ep := make(map[string]bool, len(r.DisabledPlugins))
		for _, k := range r.DisabledPlugins {
			ep[k] = false
		}
		doc["enabledPlugins"] = ep
	}
	// termsafe:allow-raw-json harness --settings argument, never printed raw
	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("launch: role settings do not encode: %w", err)
	}
	return string(out), nil
}

// mergeRoleSettingsInto merges r's enabledPlugins and claudeMdExcludes into
// an existing --settings JSON document (used for worker launches that also
// specify --role). Built-in worker keys are never overwritten; enabledPlugins
// and claudeMdExcludes are unioned.
func mergeRoleSettingsInto(base string, r ResolvedRole) (string, error) {
	if !roleHasSettings(r) {
		return base, nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(base), &doc); err != nil {
		return "", fmt.Errorf("launch: settings do not parse: %w", err)
	}
	if len(r.DisabledPlugins) > 0 {
		ep, _ := doc["enabledPlugins"].(map[string]any)
		if ep == nil {
			ep = map[string]any{}
		}
		for _, k := range r.DisabledPlugins {
			ep[k] = false
		}
		doc["enabledPlugins"] = ep
	}
	if len(r.ClaudeMDExcludes) > 0 {
		var excludes []string
		if existing, ok := doc["claudeMdExcludes"].([]any); ok {
			for _, item := range existing {
				if s, ok := item.(string); ok {
					excludes = append(excludes, s)
				}
			}
		}
		for _, pat := range r.ClaudeMDExcludes {
			if !slices.Contains(excludes, pat) {
				excludes = append(excludes, pat)
			}
		}
		doc["claudeMdExcludes"] = excludes
	}
	// termsafe:allow-raw-json harness --settings argument, never printed raw
	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("launch: merged role settings do not encode: %w", err)
	}
	return string(out), nil
}

// withRoleArgs injects a role's Claude CLI flags (--name,
// --append-system-prompt-file, and for non-workers --settings) right after the
// posture's --permission-mode pair so they always precede user args and `--`.
// Passthrough postures stay untouched.
func withRoleArgs(args []string, posture Posture, r ResolvedRole, isWorker bool) ([]string, error) {
	var anchor int
	switch posture {
	case PostureClaudeSession, PostureClaudeBuilder, PostureClaudePrint:
		if len(args) < 2 || args[0] != "--permission-mode" {
			return nil, fmt.Errorf("%w: argv %q does not start with --permission-mode", ErrRole, args)
		}
		anchor = 2
	case PostureClaudeAgents:
		if len(args) < 3 || args[0] != "agents" || args[1] != "--permission-mode" {
			return nil, fmt.Errorf("%w: agents argv %q does not start with agents --permission-mode", ErrRole, args)
		}
		anchor = 3
	default:
		return args, nil
	}

	var roleFlags []string
	if !isWorker && r.SessionName != "" {
		roleFlags = append(roleFlags, "--name", r.SessionName)
	}
	if r.CharterFile != "" {
		roleFlags = append(roleFlags, "--append-system-prompt-file", r.CharterFile)
	}
	if !isWorker && roleHasSettings(r) {
		settings, err := roleSettingsJSON(r)
		if err != nil {
			return nil, err
		}
		roleFlags = append(roleFlags, "--settings", settings)
	}
	if len(roleFlags) == 0 {
		return args, nil
	}
	out := make([]string, 0, len(args)+len(roleFlags))
	out = append(out, args[:anchor]...)
	out = append(out, roleFlags...)
	out = append(out, args[anchor:]...)
	return out, nil
}

// rolesUseHome reports whether any [launch.roles.*] entry uses a home-relative
// path ("~" or "~/...").
func rolesUseHome(roles map[string]config.LaunchRole) bool {
	for _, r := range roles {
		if usesHome([]string{r.CharterFile}) || usesHome(r.ClaudeMDExcludes) {
			return true
		}
	}
	return false
}
