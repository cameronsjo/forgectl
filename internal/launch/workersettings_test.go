package launch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/cameronsjo/forgectl/internal/config"
)

// writeSettings writes body to a settings file in a temp dir and returns its path.
func writeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker-settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

// TestParseWorkerSettingsAcceptsOnlyNarrowingKeys: the file takes plugins
// off, skills down, an output style and deny rules, and refuses every key or
// value that could widen a worker.
func TestParseWorkerSettingsAcceptsOnlyNarrowingKeys(t *testing.T) {
	ok := `{
	  "enabledPlugins": {"artificer-voice@workbench": false, "cadence-palette@workbench": false},
	  "skillOverrides": {"deep-research": "off", "dataviz": "name-only", "loop": "user-invocable-only"},
	  "outputStyle": "default",
	  "permissions": {"deny": ["Agent(cadence:doc-writer)"]}
	}`
	if _, err := parseWorkerSettings([]byte(ok)); err != nil {
		t.Fatalf("narrowing file refused: %v", err)
	}
	for name, body := range map[string]string{
		"allow rule":         `{"permissions": {"allow": ["Bash(*)"]}}`,
		"default mode":       `{"permissions": {"defaultMode": "bypassPermissions"}}`,
		"top-level mode":     `{"defaultMode": "bypassPermissions"}`,
		"hooks":              `{"hooks": {}}`,
		"env":                `{"env": {"GH_TOKEN": "x"}}`,
		"plugin on":          `{"enabledPlugins": {"x@m": true}}`,
		"plugin null":        `{"enabledPlugins": {"x@m": null}}`,
		"duplicate turns on": `{"enabledPlugins": {"x@m": false, "x@m": true}}`,
		"skill on":           `{"skillOverrides": {"x": "on"}}`,
		"skill unknown":      `{"skillOverrides": {"x": "hidden"}}`,
		"empty plugin":       `{"enabledPlugins": {"": false}}`,
		"empty deny":         `{"permissions": {"deny": [" "]}}`,
		"control char":       `{"permissions": {"deny": ["Agent(x)\u001b[2J"]}}`,
		"format char":        "{\"outputStyle\": \"de\\u200bfault\"}",
		"trailing data":      `{} {}`,
		"not an object":      `["enabledPlugins"]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWorkerSettings([]byte(body)); err == nil {
				t.Errorf("parseWorkerSettings(%s) accepted it", body)
			}
		})
	}
}

// TestMergeWorkerSettingsNeverChangesForgectlKeys: the file's keys land, a
// key forgectl sets keeps forgectl's value, and deny rules are a union.
func TestMergeWorkerSettingsNeverChangesForgectlKeys(t *testing.T) {
	if got, err := mergeWorkerSettings(workerClaudeEditSettings, nil); err != nil || got != workerClaudeEditSettings {
		t.Fatalf("no file changed the settings: %q, %v", got, err)
	}
	file, err := parseWorkerSettings([]byte(`{
	  "enabledPlugins": {"artificer-voice@workbench": false},
	  "outputStyle": "default",
	  "permissions": {"deny": ["Agent(a:b)", "Agent(a:b)", "Bash(rm *)"]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	base := `{"useAutoModeDuringPlan":false,"outputStyle":"forgectl","permissions":{"allow":["Bash(go test *)"],"deny":["Bash(rm *)"]}}`
	got, err := mergeWorkerSettings(base, file)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		UseAutoModeDuringPlan *bool           `json:"useAutoModeDuringPlan"`
		EnabledPlugins        map[string]bool `json:"enabledPlugins"`
		OutputStyle           string          `json:"outputStyle"`
		Permissions           struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("merged settings do not parse: %v\n%s", err, got)
	}
	if doc.UseAutoModeDuringPlan == nil || *doc.UseAutoModeDuringPlan {
		t.Error("useAutoModeDuringPlan lost its false")
	}
	if on, set := doc.EnabledPlugins["artificer-voice@workbench"]; !set || on {
		t.Errorf("enabledPlugins = %v, want artificer-voice off", doc.EnabledPlugins)
	}
	if doc.OutputStyle != "forgectl" {
		t.Errorf("outputStyle = %q; a key forgectl sets must keep forgectl's value", doc.OutputStyle)
	}
	if !slices.Equal(doc.Permissions.Allow, []string{"Bash(go test *)"}) {
		t.Errorf("allow = %q, want forgectl's list untouched", doc.Permissions.Allow)
	}
	if want := []string{"Bash(rm *)", "Agent(a:b)"}; !slices.Equal(doc.Permissions.Deny, want) {
		t.Errorf("deny = %q, want the union %q", doc.Permissions.Deny, want)
	}
	again, _ := mergeWorkerSettings(base, file)
	if again != got {
		t.Error("merge is not deterministic")
	}
}

// TestReadWorkerSettingsRefusals: a relative path, a missing file and an
// oversized file are refused; a ~/ path expands against home.
func TestReadWorkerSettingsRefusals(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "w.json"), []byte(`{"outputStyle":"default"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	homeFn := func() (string, error) { return home, nil }
	if s, err := readWorkerSettingsFor("~/w.json", homeFn); err != nil || s == nil || *s.OutputStyle != "default" {
		t.Fatalf("~/w.json: %v, %v", s, err)
	}
	if s, err := readWorkerSettingsFor("", func() (string, error) { return "", errors.New("no home") }); s != nil || err != nil {
		t.Fatalf("an unset path must be no file and never look up home: %v, %v", s, err)
	}
	big := writeSettings(t, `{"outputStyle":"`+strings.Repeat("x", maxWorkerSettingsBytes)+`"}`)
	for name, path := range map[string]string{
		"relative": "w.json",
		"missing":  filepath.Join(home, "absent.json"),
		"oversize": big,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readWorkerSettingsFor(path, homeFn); !errors.Is(err, ErrWorkerSettings) {
				t.Errorf("readWorkerSettingsFor(%s) = %v, want ErrWorkerSettings", name, err)
			}
		})
	}
}

// buildWorker builds a worker invocation for harness with lw as [launch.worker].
func buildWorker(t *testing.T, harness string, lw config.LaunchWorker) (BuiltInvocation, error) {
	t.Helper()
	return BuildInvocation(InvocationRequest{
		StdoutTerminal: true,
		Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: harness, PermissionMode: "plan"}, Worker: lw},
		CWD:            projectDir(t), Worker: true,
		Resolve: fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH}),
	})
}

// settingsArg returns the value after --settings in args, or "".
func settingsArg(args []string) string {
	if i := slices.Index(args, "--settings"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// TestWorkerLaunchMergesTheSettingsFile: a claude worker's --settings carries
// the file's keys beside forgectl's own; a bad file refuses the launch; a
// codex worker never reads the file.
func TestWorkerLaunchMergesTheSettingsFile(t *testing.T) {
	path := writeSettings(t, `{"enabledPlugins":{"artificer-voice@workbench":false}}`)
	built, err := buildWorker(t, "claude", config.LaunchWorker{Settings: path})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	got := settingsArg(built.Invocation.Args)
	if !strings.Contains(got, `"enabledPlugins":{"artificer-voice@workbench":false}`) || !strings.Contains(got, `"useAutoModeDuringPlan":false`) {
		t.Errorf("--settings = %s, want the file's plugin off beside forgectl's keys", got)
	}

	bad := writeSettings(t, `{"permissions":{"allow":["Bash(*)"]}}`)
	if _, err := buildWorker(t, "claude", config.LaunchWorker{Settings: bad}); !errors.Is(err, ErrWorkerSettings) {
		t.Errorf("a widening file launched: %v", err)
	}
	if _, err := buildWorker(t, "codex", config.LaunchWorker{Settings: bad}); err != nil {
		t.Errorf("a codex worker read the claude settings file: %v", err)
	}
}

// TestWorkerPostureReportsTheSettingsFile: launch which names the file and
// says when the launch would refuse it.
func TestWorkerPostureReportsTheSettingsFile(t *testing.T) {
	good := writeSettings(t, `{"outputStyle":"default"}`)
	w := DefaultWorkerPosture(config.LaunchConfig{Worker: config.LaunchWorker{Settings: good}})
	if w.Settings != good || w.Refused != "" {
		t.Errorf("good file: settings %q refused %q", w.Settings, w.Refused)
	}
	bad := writeSettings(t, `{"hooks":{}}`)
	w = DefaultWorkerPosture(config.LaunchConfig{Worker: config.LaunchWorker{Settings: bad}})
	if !strings.Contains(w.Refused, "[launch.worker] settings") {
		t.Errorf("bad file not reported as refused: %q", w.Refused)
	}
}

// workerSettingsSchema loads the Claude Code settings schema vendored for the
// pr package (see AGENTS.md, Vendored Claude Code settings schema).
func workerSettingsSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "pr", "testdata", "claude-code-settings.schema.json"))
	if err != nil {
		t.Fatalf("read vendored schema: %v", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse vendored schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve vendored schema: %v", err)
	}
	return resolved
}

// TestMergedWorkerSettingsValidateAgainstTheSettingsSchema: one key Claude
// Code rejects voids the whole --settings document, forgectl's own keys
// included (forgectl#694), so every merged document must validate.
func TestMergedWorkerSettingsValidateAgainstTheSettingsSchema(t *testing.T) {
	schema := workerSettingsSchema(t)
	file, err := parseWorkerSettings([]byte(`{
	  "enabledPlugins": {"artificer-voice@workbench": false},
	  "skillOverrides": {"deep-research": "off", "dataviz": "name-only"},
	  "outputStyle": "default",
	  "permissions": {"deny": ["Agent(cadence:doc-writer)", "Skill(cadence:dream)", "WebFetch", "mcp__board"]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{workerClaudeSettings, workerClaudeEditSettings} {
		doc, err := mergeWorkerSettings(base, file)
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal([]byte(doc), &instance); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Errorf("merged worker settings fail the settings schema, so Claude Code would drop all of it: %v\n%s", err, doc)
		}
	}
}

// TestWorkerDenyToolsMatchTheSettingsSchema: every tool name the file may
// deny is one the schema's permissionRule accepts, and the control (a name
// the schema rejects) is refused by the file check too.
func TestWorkerDenyToolsMatchTheSettingsSchema(t *testing.T) {
	schema := workerSettingsSchema(t)
	for _, tool := range workerDenyTools {
		instance := map[string]any{"permissions": map[string]any{"deny": []any{tool + "(x)"}}}
		if err := schema.Validate(instance); err != nil {
			t.Errorf("workerDenyTools has %q, which the settings schema rejects: %v", tool, err)
		}
	}
	control := map[string]any{"permissions": map[string]any{"deny": []any{"Foo(x)"}}}
	if schema.Validate(control) == nil {
		t.Fatal("control: the schema accepted Foo(x), so this test cannot go red")
	}
	if _, err := parseWorkerSettings([]byte(`{"permissions":{"deny":["Foo(x)"]}}`)); err == nil {
		t.Error("the file accepted a deny rule the schema rejects")
	}
}
