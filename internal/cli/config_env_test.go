package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// configEnvDoc mirrors the env_resolved half of the --json contract.
type configEnvDoc struct {
	EnvResolved []struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		Source string `json:"source"`
	} `json:"env_resolved"`
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"FORGECTL_SESSIONS_DSN", "HEARTH_DIR", "CHRONICLE_DIR", "DESK_DIR", "CLAUDE_DESK_DIR", "XDG_STATE_HOME"} {
		t.Setenv(k, "")
	}
}

// TestConfig_EnvOverridesAreShown pins ADR-0008 rule 4 for forgectl#1150:
// with FORGECTL_SESSIONS_DSN, HEARTH_DIR and DESK_DIR set, config shows the
// value each verb uses and names the variable that supplied it, in both
// output modes, and the DSN itself is still withheld.
func TestConfig_EnvOverridesAreShown(t *testing.T) {
	clearConfigEnv(t)
	desk := filepath.Join(t.TempDir(), "desk")
	t.Setenv("FORGECTL_SESSIONS_DSN", "postgres://u:hunter2@h/db")
	t.Setenv("HEARTH_DIR", "/srv/hearth")
	t.Setenv("DESK_DIR", desk)

	raw := runConfig(t, "", "--json")
	if strings.Contains(raw, "hunter2") {
		t.Fatalf("--json leaks the DSN from the environment:\n%s", raw)
	}
	var doc configEnvDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string][2]string{}
	for _, e := range doc.EnvResolved {
		got[e.Key] = [2]string{e.Value, e.Source}
	}
	want := map[string][2]string{
		"sessions.dsn":        {"(redacted)", "FORGECTL_SESSIONS_DSN"},
		"bench.hearth_dir":    {"/srv/hearth", "HEARTH_DIR"},
		"bench.chronicle_dir": {"", "unset"},
		"desk.dir":            {desk, "DESK_DIR"},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("env_resolved %s = %q; want %q", k, got[k], w)
		}
	}

	text := runConfig(t, "")
	if strings.Contains(text, "hunter2") {
		t.Fatalf("human output leaks the DSN from the environment:\n%s", text)
	}
	for _, s := range []string{"(FORGECTL_SESSIONS_DSN)", "/srv/hearth", "(HEARTH_DIR)", desk, "(DESK_DIR)"} {
		if !strings.Contains(text, s) {
			t.Errorf("human output lacks %q:\n%s", s, text)
		}
	}
}

// TestConfig_EnvResolvedFileAndDefaults: the file's value wins over the
// environment for the [bench] dirs (config.resolveDir's order), the desk dir
// falls back to its default, and a relative DESK_DIR is reported as the error
// the desk verbs would refuse with.
func TestConfig_EnvResolvedFileAndDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HEARTH_DIR", "/from/env")
	var doc configEnvDoc
	raw := runConfig(t, "[bench]\nhearth_dir = \"/from/file\"\n", "--json")
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range doc.EnvResolved {
		switch e.Key {
		case "bench.hearth_dir":
			if e.Value != "/from/file" || e.Source != envSourceConfig {
				t.Errorf("hearth_dir = %q (%s); want the file's value", e.Value, e.Source)
			}
		case "desk.dir":
			if !strings.HasSuffix(e.Value, filepath.Join("forgectl", "desk")) || e.Source != envSourceDefault {
				t.Errorf("desk.dir = %q (%s); want the default", e.Value, e.Source)
			}
		case "sessions.dsn":
			if e.Value != "" || e.Source != envSourceUnset {
				t.Errorf("sessions.dsn = %q (%s); want unset", e.Value, e.Source)
			}
		}
	}

	t.Setenv("DESK_DIR", "relative/desk")
	doc = configEnvDoc{}
	if err := json.Unmarshal([]byte(runConfig(t, "", "--json")), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range doc.EnvResolved {
		if e.Key == "desk.dir" && (e.Source != envSourceError || !strings.Contains(e.Value, "DESK_DIR must be an absolute path")) {
			t.Errorf("desk.dir = %q (%s); want the absolute-path error", e.Value, e.Source)
		}
	}
}
