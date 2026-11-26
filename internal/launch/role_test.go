package launch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

type pluginFixture struct {
	name            string
	marketplace     string
	sourceType      string // github | git | url | directory
	sourcePath      string // optional source.path (causes fail-closed)
	manifestHooks   bool
	manifestStrictF bool
	duplicateEntry  bool
	hooksJSON       string // raw content for hooks/hooks.json; "" = absent
	hooksJSONIsDir  bool
	pluginJSON      string // raw content for .claude-plugin/plugin.json; "" = absent
	relativeInstall bool
	missingInstall  bool
}

func setupClaudePluginEnv(t *testing.T, fixtures []pluginFixture) string {
	t.Helper()
	cfgDir := filepath.Join(t.TempDir(), "claude-cfg")
	pluginsDir := filepath.Join(cfgDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		t.Fatalf("mkdir pluginsDir: %v", err)
	}

	installed := map[string]any{}
	known := map[string]any{}
	mpPlugins := map[string][]map[string]any{}
	mpLocs := map[string]string{}

	for _, fx := range fixtures {
		key := fx.name + "@" + fx.marketplace
		if _, ok := mpLocs[fx.marketplace]; !ok {
			mpLoc := filepath.Join(cfgDir, "marketplaces", fx.marketplace)
			mpLocs[fx.marketplace] = mpLoc
			if err := os.MkdirAll(filepath.Join(mpLoc, ".claude-plugin"), 0o700); err != nil {
				t.Fatalf("mkdir mp: %v", err)
			}
			src := map[string]any{"source": fx.sourceType}
			if fx.sourcePath != "" {
				src["path"] = fx.sourcePath
			}
			known[fx.marketplace] = map[string]any{
				"source":          src,
				"installLocation": mpLoc,
			}
		}

		entry := map[string]any{"name": fx.name}
		if fx.manifestHooks {
			entry["hooks"] = map[string]any{"PreToolUse": []any{}}
		}
		if fx.manifestStrictF {
			entry["strict"] = false
		}
		mpPlugins[fx.marketplace] = append(mpPlugins[fx.marketplace], entry)
		if fx.duplicateEntry {
			mpPlugins[fx.marketplace] = append(mpPlugins[fx.marketplace], map[string]any{"name": fx.name})
		}

		installPath := filepath.Join(cfgDir, "cache", fx.marketplace, fx.name)
		if err := os.MkdirAll(installPath, 0o700); err != nil {
			t.Fatalf("mkdir installPath: %v", err)
		}
		if fx.hooksJSONIsDir {
			if err := os.MkdirAll(filepath.Join(installPath, "hooks", "hooks.json"), 0o700); err != nil {
				t.Fatalf("mkdir hooks.json dir: %v", err)
			}
		} else if fx.hooksJSON != "" {
			if err := os.MkdirAll(filepath.Join(installPath, "hooks"), 0o700); err != nil {
				t.Fatalf("mkdir hooks: %v", err)
			}
			if err := os.WriteFile(filepath.Join(installPath, "hooks", "hooks.json"), []byte(fx.hooksJSON), 0o600); err != nil {
				t.Fatalf("write hooks.json: %v", err)
			}
		}
		if fx.pluginJSON != "" {
			if err := os.MkdirAll(filepath.Join(installPath, ".claude-plugin"), 0o700); err != nil {
				t.Fatalf("mkdir .claude-plugin: %v", err)
			}
			if err := os.WriteFile(filepath.Join(installPath, ".claude-plugin", "plugin.json"), []byte(fx.pluginJSON), 0o600); err != nil {
				t.Fatalf("write plugin.json: %v", err)
			}
		}

		switch {
		case fx.missingInstall:
			installed[key] = []map[string]any{{}}
		case fx.relativeInstall:
			installed[key] = []map[string]any{{"installPath": "relative/path"}}
		default:
			installed[key] = []map[string]any{{"installPath": installPath}}
		}
	}

	for mp, entries := range mpPlugins {
		raw, err := json.Marshal(map[string]any{"plugins": entries})
		if err != nil {
			t.Fatalf("marshal marketplace.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(mpLocs[mp], ".claude-plugin", "marketplace.json"), raw, 0o600); err != nil {
			t.Fatalf("write marketplace.json: %v", err)
		}
	}

	rawInstalled, err := json.Marshal(map[string]any{"plugins": installed})
	if err != nil {
		t.Fatalf("marshal installed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), rawInstalled, 0o600); err != nil {
		t.Fatalf("write installed_plugins.json: %v", err)
	}

	rawKnown, err := json.Marshal(known)
	if err != nil {
		t.Fatalf("marshal known: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "known_marketplaces.json"), rawKnown, 0o600); err != nil {
		t.Fatalf("write known_marketplaces.json: %v", err)
	}

	return cfgDir
}

func TestResolveDisabledPlugins_GuardHookSafetyCheck(t *testing.T) {
	cfgDir := setupClaudePluginEnv(t, []pluginFixture{
		{
			name:        "safe-no-hooks",
			marketplace: "workbench",
			sourceType:  "github",
			pluginJSON:  `{"name":"safe-no-hooks"}`,
		},
		{
			name:        "safe-quiet-hooks",
			marketplace: "workbench",
			sourceType:  "github",
			hooksJSON:   `{"description":"quiet","hooks":{"SessionStart":[],"Notification":[],"PostModelSwitch":[]}}`,
			pluginJSON:  `{"name":"safe-quiet-hooks","hooks":{"SessionEnd":[]}}`,
		},
		{
			name:        "blocking-pretool",
			marketplace: "workbench",
			sourceType:  "github",
			hooksJSON:   `{"hooks":{"PreToolUse":[{"matcher":"Bash"}]}}`,
		},
		{
			name:        "blocking-plugin-json",
			marketplace: "workbench",
			sourceType:  "github",
			pluginJSON:  `{"name":"blocking-plugin-json","hooks":{"Stop":[]}}`,
		},
		{
			name:        "unknown-hooks-key",
			marketplace: "workbench",
			sourceType:  "github",
			hooksJSON:   `{"hooks":{"SessionStart":[]},"extra":true}`,
		},
		{
			name:        "hooks-as-string",
			marketplace: "workbench",
			sourceType:  "github",
			pluginJSON:  `{"name":"hooks-as-string","hooks":"./hooks/other.json"}`,
		},
		{
			name:        "malformed-hooks-json",
			marketplace: "workbench",
			sourceType:  "github",
			hooksJSON:   `{not json`,
		},
		{
			name:           "dir-hooks-json",
			marketplace:    "workbench",
			sourceType:     "github",
			hooksJSONIsDir: true,
		},
		{
			name:        "local-marketplace",
			marketplace: "localmp",
			sourceType:  "directory",
			pluginJSON:  `{"name":"local-marketplace"}`,
		},
		{
			name:        "remote-with-source-path",
			marketplace: "pathmp",
			sourceType:  "git",
			sourcePath:  "./sub",
			pluginJSON:  `{"name":"remote-with-source-path"}`,
		},
		{
			name:          "manifest-has-hooks",
			marketplace:   "workbench",
			sourceType:    "github",
			manifestHooks: true,
		},
		{
			name:            "manifest-strict-false",
			marketplace:     "workbench",
			sourceType:      "github",
			manifestStrictF: true,
		},
		{
			name:           "manifest-duplicate",
			marketplace:    "workbench",
			sourceType:     "github",
			duplicateEntry: true,
		},
		{
			name:            "relative-install",
			marketplace:     "workbench",
			sourceType:      "github",
			relativeInstall: true,
		},
		{
			name:           "missing-install",
			marketplace:    "workbench",
			sourceType:     "github",
			missingInstall: true,
		},
	})

	requested := []string{
		"safe-no-hooks",
		"safe-quiet-hooks",
		"blocking-pretool",
		"blocking-plugin-json",
		"unknown-hooks-key",
		"hooks-as-string",
		"malformed-hooks-json",
		"dir-hooks-json",
		"local-marketplace",
		"remote-with-source-path",
		"manifest-has-hooks",
		"manifest-strict-false",
		"manifest-duplicate",
		"relative-install",
		"missing-install",
		"not-installed",
	}

	disabled, guarded, notes := resolveDisabledPlugins(cfgDir, requested, false)
	wantDisabled := []string{"safe-no-hooks@workbench", "safe-quiet-hooks@workbench"}
	if !slices.Equal(disabled, wantDisabled) {
		t.Errorf("disabled = %v, want %v", disabled, wantDisabled)
	}
	wantGuarded := []string{
		"blocking-plugin-json@workbench",
		"blocking-pretool@workbench",
		"dir-hooks-json@workbench",
		"hooks-as-string@workbench",
		"local-marketplace@localmp",
		"malformed-hooks-json@workbench",
		"manifest-duplicate@workbench",
		"manifest-has-hooks@workbench",
		"manifest-strict-false@workbench",
		"missing-install@workbench",
		"relative-install@workbench",
		"remote-with-source-path@pathmp",
		"unknown-hooks-key@workbench",
	}
	if !slices.Equal(guarded, wantGuarded) {
		t.Errorf("guarded = %v, want %v", guarded, wantGuarded)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "keeping ") {
		t.Errorf("notes = %v, want keeping-on warning", notes)
	}

	// Explicit override disables all matched installed plugins.
	disabledAll, guardedAll, notesAll := resolveDisabledPlugins(cfgDir, requested, true)
	if len(guardedAll) != 0 {
		t.Errorf("allowUnsafe guarded = %v, want none", guardedAll)
	}
	if len(disabledAll) != len(wantDisabled)+len(wantGuarded) {
		t.Errorf("allowUnsafe disabled count = %d, want %d", len(disabledAll), len(wantDisabled)+len(wantGuarded))
	}
	if len(notesAll) != 0 {
		t.Errorf("allowUnsafe notes = %v, want none", notesAll)
	}
}

func TestResolveDisabledPlugins_MissingOrMalformedInstalledFailsClosed(t *testing.T) {
	emptyDir := t.TempDir()
	disabled, guarded, notes := resolveDisabledPlugins(emptyDir, []string{"cadence-voice"}, false)
	if len(disabled) != 0 || len(guarded) != 0 {
		t.Errorf("missing installed_plugins.json: disabled=%v guarded=%v, want empty", disabled, guarded)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no ") {
		t.Errorf("notes = %v, want missing file notice", notes)
	}

	// Malformed JSON
	pluginsDir := filepath.Join(emptyDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	disabled, guarded, notes = resolveDisabledPlugins(emptyDir, []string{"cadence-voice"}, false)
	if len(disabled) != 0 || len(guarded) != 0 {
		t.Errorf("malformed installed_plugins.json: disabled=%v guarded=%v, want empty", disabled, guarded)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "could not read") {
		t.Errorf("notes = %v, want unreadable notice", notes)
	}
}

func TestBuildInvocation_RoleInteractiveResumeAndWorker(t *testing.T) {
	cfgDir := setupClaudePluginEnv(t, []pluginFixture{
		{name: "cadence-voice", marketplace: "workbench", sourceType: "github", pluginJSON: `{"name":"cadence-voice"}`},
		{name: "cadence-guardrails", marketplace: "workbench", sourceType: "github", hooksJSON: `{"hooks":{"PreToolUse":[]}}`},
	})
	cwd := t.TempDir()
	charter := filepath.Join(t.TempDir(), "charter.md")
	if err := os.WriteFile(charter, []byte("# Chief of Staff\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lc := config.LaunchConfig{
		Roles: map[string]config.LaunchRole{
			"chief-of-staff": {
				CharterFile:     charter,
				DisablePlugins:  []string{"cadence-voice", "cadence-guardrails"},
				Posture:         RolePostureAutonomous,
				ExcludeClaudeMD: true,
				SessionName:     "chief-of-staff",
			},
			"posture-only": {
				Posture: RolePostureInteractive,
			},
		},
	}

	stubResolve := func(harness string, _ config.LaunchDefaults) (ResolvedBinary, error) {
		return ResolvedBinary{Path: "/usr/bin/" + harness, Source: BinaryPATH}, nil
	}

	// 1. Interactive launch with --role
	built, err := BuildInvocation(InvocationRequest{
		Config:         lc,
		CWD:            cwd,
		BaseEnv:        []string{"CLAUDE_CONFIG_DIR=" + cfgDir, "HOME=" + t.TempDir()},
		Resolve:        stubResolve,
		StdoutTerminal: true,
		StayInCWD:      true,
		Role:           "chief-of-staff",
	})
	if err != nil {
		t.Fatalf("interactive BuildInvocation: %v", err)
	}
	if envLookup(built.Invocation.Env, RoleEnv) != "chief-of-staff" {
		t.Errorf("FORGECTL_ROLE = %q, want chief-of-staff", envLookup(built.Invocation.Env, RoleEnv))
	}
	if envLookup(built.Invocation.Env, PostureEnv) != "autonomous" {
		t.Errorf("FORGECTL_POSTURE = %q, want autonomous", envLookup(built.Invocation.Env, PostureEnv))
	}
	if !slices.Contains(built.Invocation.Args, "--append-system-prompt-file") || !slices.Contains(built.Invocation.Args, charter) {
		t.Errorf("args missing charter: %v", built.Invocation.Args)
	}
	if !slices.Contains(built.Invocation.Args, "--name") || !slices.Contains(built.Invocation.Args, "chief-of-staff") {
		t.Errorf("args missing --name: %v", built.Invocation.Args)
	}
	settingsStr := flagValue(built.Invocation.Args, "--settings")
	if settingsStr == "" {
		t.Fatalf("args missing --settings: %v", built.Invocation.Args)
	}
	var settingsDoc struct {
		EnabledPlugins   map[string]bool `json:"enabledPlugins"`
		ClaudeMDExcludes []string        `json:"claudeMdExcludes"`
	}
	if err := json.Unmarshal([]byte(settingsStr), &settingsDoc); err != nil {
		t.Fatalf("unmarshal --settings: %v", err)
	}
	if v, ok := settingsDoc.EnabledPlugins["cadence-voice@workbench"]; !ok || v {
		t.Errorf("expected cadence-voice@workbench disabled, got %v", settingsDoc.EnabledPlugins)
	}
	if _, ok := settingsDoc.EnabledPlugins["cadence-guardrails@workbench"]; ok {
		t.Errorf("cadence-guardrails@workbench must stay ON (guarded), got %v", settingsDoc.EnabledPlugins)
	}
	wantExcludes := []string{
		filepath.Join(cfgDir, "CLAUDE.md"),
		filepath.Join(cfgDir, "rules", "**"),
		filepath.Join(cwd, "CLAUDE.md"),
		filepath.Join(cwd, "AGENTS.md"),
	}
	if !slices.Equal(settingsDoc.ClaudeMDExcludes, wantExcludes) {
		t.Errorf("claudeMdExcludes = %v, want %v", settingsDoc.ClaudeMDExcludes, wantExcludes)
	}
	if len(built.Notes) == 0 || !strings.Contains(built.Notes[0], "cadence-guardrails@workbench") {
		t.Errorf("expected guard note, got %v", built.Notes)
	}

	// 2. Resume launch (`-- --resume <id>`) preserves role charter, settings, and env
	resumed, err := BuildInvocation(InvocationRequest{
		Config:         lc,
		CWD:            cwd,
		Args:           []string{"--resume", "abc-123"},
		BaseEnv:        []string{"CLAUDE_CONFIG_DIR=" + cfgDir},
		Resolve:        stubResolve,
		StdoutTerminal: true,
		Role:           "chief-of-staff",
	})
	if err != nil {
		t.Fatalf("resume BuildInvocation: %v", err)
	}
	if !slices.Contains(resumed.Invocation.Args, "--append-system-prompt-file") {
		t.Errorf("resume args missing charter: %v", resumed.Invocation.Args)
	}
	if flagValue(resumed.Invocation.Args, "--settings") == "" {
		t.Errorf("resume args missing --settings: %v", resumed.Invocation.Args)
	}
	if flagValue(resumed.Invocation.Args, "--resume") != "abc-123" {
		t.Errorf("resume args missing --resume abc-123: %v", resumed.Invocation.Args)
	}
	if envLookup(resumed.Invocation.Env, RoleEnv) != "chief-of-staff" || envLookup(resumed.Invocation.Env, PostureEnv) != "autonomous" {
		t.Errorf("resume env missing role/posture")
	}

	// 3. Worker launch with --role merges role settings into worker --settings
	workerBuilt, err := BuildInvocation(InvocationRequest{
		Config:         lc,
		CWD:            cwd,
		BaseEnv:        []string{"CLAUDE_CONFIG_DIR=" + cfgDir, "HOME=" + t.TempDir()},
		Resolve:        stubResolve,
		StdoutTerminal: true,
		Worker:         true,
		SessionName:    "my-worker",
		Role:           "chief-of-staff",
	})
	if err != nil {
		t.Fatalf("worker BuildInvocation: %v", err)
	}
	if envLookup(workerBuilt.Invocation.Env, DrainWorkerEnv) != "1" {
		t.Errorf("worker missing FORGECTL_DRAIN_WORKER=1")
	}
	if envLookup(workerBuilt.Invocation.Env, RoleEnv) != "chief-of-staff" || envLookup(workerBuilt.Invocation.Env, PostureEnv) != "autonomous" {
		t.Errorf("worker missing FORGECTL_ROLE/FORGECTL_POSTURE")
	}
	if flagValue(workerBuilt.Invocation.Args, "--name") != "my-worker" {
		t.Errorf("worker --name = %q, want my-worker (not role session_name)", flagValue(workerBuilt.Invocation.Args, "--name"))
	}
	if !slices.Contains(workerBuilt.Invocation.Args, "--append-system-prompt-file") {
		t.Errorf("worker args missing charter: %v", workerBuilt.Invocation.Args)
	}
	workerSettings := flagValue(workerBuilt.Invocation.Args, "--settings")
	var wsDoc map[string]any
	if err := json.Unmarshal([]byte(workerSettings), &wsDoc); err != nil {
		t.Fatalf("unmarshal worker --settings: %v", err)
	}
	if wsDoc["useAutoModeDuringPlan"] != false || wsDoc["editorMode"] != "normal" {
		t.Errorf("worker built-in settings lost: %v", wsDoc)
	}
	ep, _ := wsDoc["enabledPlugins"].(map[string]any)
	if ep["cadence-voice@workbench"] != false {
		t.Errorf("worker merged settings missing cadence-voice@workbench: false, got %v", wsDoc)
	}

	// 4. Non-claude harness refuses Claude-only role, accepts posture-only role
	if _, err := BuildInvocation(InvocationRequest{
		Config:  lc,
		CWD:     cwd,
		Harness: "codex",
		Resolve: stubResolve,
		Role:    "chief-of-staff",
	}); !errors.Is(err, ErrRoleUnsupportedHarness) {
		t.Errorf("codex with claude-only role: err = %v, want ErrRoleUnsupportedHarness", err)
	}
	codexBuilt, err := BuildInvocation(InvocationRequest{
		Config:  lc,
		CWD:     cwd,
		Harness: "codex",
		Resolve: stubResolve,
		Role:    "posture-only",
	})
	if err != nil {
		t.Fatalf("codex with posture-only role: %v", err)
	}
	if envLookup(codexBuilt.Invocation.Env, RoleEnv) != "posture-only" || envLookup(codexBuilt.Invocation.Env, PostureEnv) != "interactive" {
		t.Errorf("codex posture-only env missing FORGECTL_ROLE/FORGECTL_POSTURE")
	}

	// 5. Unknown role fails with ErrRoleNotFound
	if _, err := BuildInvocation(InvocationRequest{
		Config:  lc,
		CWD:     cwd,
		Resolve: stubResolve,
		Role:    "does-not-exist",
	}); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("unknown role: err = %v, want ErrRoleNotFound", err)
	}
}

func flagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
