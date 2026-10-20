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
	"strings"
	"unicode"
)

// ErrWorkerSettings reports a [launch.worker] settings file forgectl will not
// pass to a worker: unreadable, not JSON, or carrying a key or value that
// could widen the worker (#1323).
var ErrWorkerSettings = errors.New("launch: [launch.worker] settings")

// maxWorkerSettingsBytes caps the settings file. The merged document rides
// argv, and a trimming file is a few kilobytes even with every agent denied.
const maxWorkerSettingsBytes = 64 << 10

// workerSettingsFile is the shape a [launch.worker] settings file may take.
// Every key only takes something away from the worker; anything else is
// refused by the strict decoder, so the file can never widen a worker past
// the floor (no permissions.allow, defaultMode, hooks, env, or a plugin
// turned on).
//
//   - enabledPlugins: plugin@marketplace -> false turns a plugin off for the
//     worker, with its skills, agents, hooks, output style and MCP servers.
//   - skillOverrides: skill -> name-only | user-invocable-only | off. Claude
//     Code 2.1.305 applies it to user and synced skills, not plugin skills.
//   - outputStyle: the output style the worker uses.
//   - permissions.deny: deny rules; Agent(plugin:agent) also drops the agent
//     from the worker's agent listing.
type workerSettingsFile struct {
	EnabledPlugins map[string]*bool   `json:"enabledPlugins,omitempty"`
	SkillOverrides map[string]*string `json:"skillOverrides,omitempty"`
	OutputStyle    *string            `json:"outputStyle,omitempty"`
	Permissions    *struct {
		Deny []string `json:"deny"`
	} `json:"permissions,omitempty"`
}

// narrowingSkillOverrides are the skillOverrides values a worker file may
// set. "on" is left out: it would re-list a skill the operator turned off.
var narrowingSkillOverrides = []string{"name-only", "user-invocable-only", "off"}

// workerDenyRule is the shape a deny rule in the file must take: a tool name
// with an optional (specifier), or an mcp__ name. Claude Code checks the
// whole --settings document, and one rule it rejects voids every key in it,
// forgectl's own included (forgectl#694), so the file is held to the tool
// names the vendored settings schema accepts
// (internal/pr/testdata/claude-code-settings.schema.json, permissionRule).
// TestWorkerDenyToolsMatchTheSettingsSchema keeps the two lists together.
var workerDenyTools = []string{
	"Agent", "Artifact", "Bash", "Cd", "Edit", "EnterWorktree", "ExitPlanMode", "Glob", "Grep",
	"KillShell", "LSP", "Monitor", "MultiEdit", "NotebookEdit", "PowerShell", "Read",
	"ShareOnboardingGuide", "Skill", "TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop",
	"TaskUpdate", "TodoWrite", "ToolSearch", "WebFetch", "WebSearch", "Workflow", "Write",
}

var workerDenyRule = regexp.MustCompile(`^((` + strings.Join(workerDenyTools, "|") + `)(\([^)]+\))?|mcp__.*)$`)

// readWorkerSettingsFor is readWorkerSettings with the home directory looked
// up only when path needs it, so a launch with no file, or an absolute one,
// never fails on an unresolvable home.
func readWorkerSettingsFor(path string, home func() (string, error)) (*workerSettingsFile, error) {
	if path == "" {
		return nil, nil
	}
	var h string
	if path == "~" || strings.HasPrefix(path, "~/") {
		var err error
		if h, err = home(); err != nil {
			return nil, fmt.Errorf("%w: expand %q: %w", ErrWorkerSettings, path, err)
		}
	}
	return readWorkerSettings(path, h)
}

// readWorkerSettings reads and checks the file at path (tilde-expanded
// against home). An empty path is no file and returns nil.
func readWorkerSettings(path, home string) (*workerSettingsFile, error) {
	if path == "" {
		return nil, nil
	}
	path = expandTilde(path, home)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %q is not an absolute path (a leading ~/ is expanded)", ErrWorkerSettings, path)
	}
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // G304: the operator's own config.toml names this file
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWorkerSettings, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxWorkerSettingsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrWorkerSettings, path, err)
	}
	if len(raw) > maxWorkerSettingsBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrWorkerSettings, path, maxWorkerSettingsBytes)
	}
	s, err := parseWorkerSettings(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrWorkerSettings, path, err)
	}
	return s, nil
}

// parseWorkerSettings decodes raw strictly and checks every value narrows.
func parseWorkerSettings(raw []byte) (*workerSettingsFile, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s workerSettingsFile
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("only enabledPlugins, skillOverrides, outputStyle and permissions.deny are allowed: %w", err)
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON object")
	}
	for name, on := range s.EnabledPlugins {
		if err := checkSettingName("enabledPlugins", name); err != nil {
			return nil, err
		}
		if on == nil || *on {
			return nil, fmt.Errorf("enabledPlugins %q must be false: a worker settings file can only turn plugins off", name)
		}
	}
	for name, v := range s.SkillOverrides {
		if err := checkSettingName("skillOverrides", name); err != nil {
			return nil, err
		}
		if v == nil || !slices.Contains(narrowingSkillOverrides, *v) {
			return nil, fmt.Errorf("skillOverrides %q must be one of %s", name, strings.Join(narrowingSkillOverrides, ", "))
		}
	}
	if s.OutputStyle != nil {
		if err := checkSettingName("outputStyle", *s.OutputStyle); err != nil {
			return nil, err
		}
	}
	if s.Permissions != nil {
		for _, rule := range s.Permissions.Deny {
			if err := checkSettingName("permissions.deny", rule); err != nil {
				return nil, err
			}
			if !workerDenyRule.MatchString(rule) {
				return nil, fmt.Errorf("permissions.deny %q is not a rule Claude Code's settings schema accepts (Tool or Tool(specifier), or mcp__…), and one bad rule voids the whole worker --settings", rule)
			}
		}
	}
	return &s, nil
}

// checkSettingName refuses an empty string or one carrying a control or
// format character, which no plugin, skill, style or rule name needs and
// which `launch which` would otherwise echo to a terminal.
func checkSettingName(key, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s has an empty entry", key)
	}
	if strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return fmt.Errorf("%s entry %q carries a control character", key, v)
	}
	return nil
}

// mergeWorkerSettings lays the file's keys over base, forgectl's own worker
// settings JSON. A key base already sets keeps base's value, and deny rules
// are the union of both lists in order, so the file can add to forgectl's
// settings but never change one.
func mergeWorkerSettings(base string, s *workerSettingsFile) (string, error) {
	if s == nil {
		return base, nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(base), &doc); err != nil {
		return "", fmt.Errorf("launch: built-in worker settings do not parse: %w", err)
	}
	setIfAbsent := func(key string, v any) {
		if _, ok := doc[key]; !ok {
			doc[key] = v
		}
	}
	if len(s.EnabledPlugins) > 0 {
		setIfAbsent("enabledPlugins", s.EnabledPlugins)
	}
	if len(s.SkillOverrides) > 0 {
		setIfAbsent("skillOverrides", s.SkillOverrides)
	}
	if s.OutputStyle != nil {
		setIfAbsent("outputStyle", *s.OutputStyle)
	}
	if s.Permissions != nil && len(s.Permissions.Deny) > 0 {
		perms, _ := doc["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		var deny []string
		if existing, ok := perms["deny"].([]any); ok {
			for _, r := range existing {
				if rule, ok := r.(string); ok {
					deny = append(deny, rule)
				}
			}
		}
		for _, rule := range s.Permissions.Deny {
			if !slices.Contains(deny, rule) {
				deny = append(deny, rule)
			}
		}
		perms["deny"] = deny
		doc["permissions"] = perms
	}
	// termsafe:allow-raw-json harness --settings argument, never printed
	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("launch: worker settings do not encode: %w", err)
	}
	return string(out), nil
}
