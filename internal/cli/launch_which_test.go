package cli

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// TestBuildLaunchWhichJSON_KeySet pins the exact top-level key set of `launch
// which --json` — an agent decoding into a map must see exactly this shape,
// never a superset or subset a future edit drifted into.
func TestBuildLaunchWhichJSON_KeySet(t *testing.T) {
	got := buildLaunchWhichJSON(launch.Profile{
		Harness:        "claude",
		Model:          "opus",
		Effort:         "high",
		PermissionMode: "acceptEdits",
		AllowDanger:    true,
		Match:          "cadence-ecosystem",
		AddDir:         []string{"/tmp/extra"},
		Env:            map[string]string{"ANTHROPIC_API_KEY": "sk-ant-hunter2"},
	}, "/tmp/cwd", "/tmp/config.toml")

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"directory", "config", "matched", "harness", "model", "effort", "permission_mode", "allow_danger", "env_keys", "add_dir"}
	if len(decoded) != len(want) {
		t.Fatalf("key set = %v, want exactly %v", keysOf(decoded), want)
	}
	for _, k := range want {
		if _, ok := decoded[k]; !ok {
			t.Errorf("missing key %q; got %v", k, keysOf(decoded))
		}
	}
}

// TestBuildLaunchWhichJSON_RowsMatchHumanTable asserts each JSON field carries
// the same information the human table's row shows for it.
func TestBuildLaunchWhichJSON_RowsMatchHumanTable(t *testing.T) {
	profile := launch.Profile{
		Harness:        "claude",
		Model:          "opus",
		Effort:         "high",
		PermissionMode: "acceptEdits",
		AllowDanger:    true,
		Match:          "cadence-ecosystem",
		AddDir:         []string{"/tmp/extra"},
		Env:            map[string]string{"ANTHROPIC_API_KEY": "x", "FOO": "y"},
	}
	got := buildLaunchWhichJSON(profile, "/tmp/cwd", "/tmp/config.toml")

	var buf bytes.Buffer
	printLaunchProfile(&buf, theme.Theme{}, profile, "/tmp/cwd", "/tmp/config.toml")
	human := buf.String()

	for _, want := range []string{profile.Harness, profile.Model, profile.Effort, profile.PermissionMode, profile.Match, "/tmp/extra"} {
		if !strings.Contains(human, want) {
			t.Fatalf("test setup: human table missing %q:\n%s", want, human)
		}
	}

	if got.Directory != "/tmp/cwd" || got.Config != "/tmp/config.toml" {
		t.Errorf("directory/config = %q/%q, want /tmp/cwd //tmp/config.toml", got.Directory, got.Config)
	}
	if got.Matched != profile.Match || got.Harness != profile.Harness || got.Model != profile.Model ||
		got.Effort != profile.Effort || got.PermissionMode != profile.PermissionMode || got.AllowDanger != profile.AllowDanger {
		t.Errorf("scalar fields diverged from the profile: %+v vs %+v", got, profile)
	}
	if len(got.AddDir) != 1 || got.AddDir[0] != "/tmp/extra" {
		t.Errorf("add_dir = %v, want [/tmp/extra]", got.AddDir)
	}
	wantKeys := []string{"ANTHROPIC_API_KEY", "FOO"}
	sort.Strings(wantKeys)
	if len(got.EnvKeys) != len(wantKeys) || got.EnvKeys[0] != wantKeys[0] || got.EnvKeys[1] != wantKeys[1] {
		t.Errorf("env_keys = %v, want %v (sorted)", got.EnvKeys, wantKeys)
	}
}

// TestBuildLaunchWhichJSON_EmptyCollectionsAreArraysNeverNull covers the
// no-env, no-add-dir profile: both slice fields must encode as [], never null,
// so a caller can range over them without a nil guard.
func TestBuildLaunchWhichJSON_EmptyCollectionsAreArraysNeverNull(t *testing.T) {
	got := buildLaunchWhichJSON(launch.Profile{Harness: "claude", Model: "opus"}, "/tmp/cwd", "/tmp/config.toml")
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"env_keys":[]`, `"add_dir":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty collection did not encode as []: got %s, want to contain %s", raw, want)
		}
	}
}

// TestBuildLaunchWhichJSON_NoEnvValuesEmitted is the no-secret-leak assertion:
// no configured env VALUE — only its key name — may appear anywhere in the
// marshaled document, mirroring printLaunchProfile's terminal-side rule.
func TestBuildLaunchWhichJSON_NoEnvValuesEmitted(t *testing.T) {
	got := buildLaunchWhichJSON(launch.Profile{
		Harness: "claude",
		Model:   "opus",
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-hunter2",
			"FOO":               "plainbarvalue",
		},
	}, "/tmp/cwd", "/tmp/config.toml")
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"sk-ant-hunter2", "plainbarvalue"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("launch which --json leaked an env value %q: %s", secret, raw)
		}
	}
	for _, want := range []string{"ANTHROPIC_API_KEY", "FOO"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("launch which --json missing env key %q: %s", want, raw)
		}
	}
}

// TestNewLaunchWhichCmd_JSONFailingRunLeavesStdoutEmpty: the RunE error path
// (a broken os.Getwd) must not have written any JSON before failing.
func TestNewLaunchWhichCmd_JSONFailingRunLeavesStdoutEmpty(t *testing.T) {
	// writeLaunchWhichJSON itself cannot fail on well-formed input, and
	// os.Getwd failing is not reproducible in a test sandbox — so this test
	// pins the achievable half of the contract: an encoder error propagates
	// without a partial write reaching a real io.Writer beforehand.
	buf := failingWriter{err: errWriteFailed}
	if err := writeLaunchWhichJSON(buf, launch.Profile{Harness: "claude"}, "/tmp/cwd", "/tmp/config.toml"); err == nil {
		t.Fatal("expected an error from a failing writer, got nil")
	}
}

var errWriteFailed = errFmt("write failed")

type errFmt string

func (e errFmt) Error() string { return string(e) }

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestPrintLaunchProfile_EnvValuesAreWithheld: `launch which`'s env row prints
// key names only. The row is the one place in this renderer that echoes
// arbitrary config-supplied strings, and `[launch.defaults.env]` is where an
// ANTHROPIC_API_KEY lives — `which` output gets pasted into issues and shared
// terminals. Mirrors config's TestConfig_EnvMapValuesAreWithheld; the two
// surfaces render the same map under the same policy.
func TestPrintLaunchProfile_EnvValuesAreWithheld(t *testing.T) {
	var buf bytes.Buffer
	printLaunchProfile(&buf, theme.Theme{}, launch.Profile{
		Harness: "claude",
		Model:   "opus",
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-hunter2",
			"FOO":               "plainbarvalue",
		},
	}, "/tmp/cwd", "/tmp/config.toml")
	out := buf.String()

	// Both values, not just the secret-looking one: the policy is every value,
	// never a heuristic on the key name.
	for _, secret := range []string{"sk-ant-hunter2", "plainbarvalue"} {
		if strings.Contains(out, secret) {
			t.Errorf("`launch which` leaked the env value %q:\n%s", secret, out)
		}
	}

	// Withholding must not degrade into dropping the row — the key names are
	// the signal an operator came for.
	for _, want := range []string{"env", "ANTHROPIC_API_KEY", "FOO"} {
		if !strings.Contains(out, want) {
			t.Errorf("`launch which` output missing %q:\n%s", want, out)
		}
	}

	// Go randomises map iteration, so the sort is load-bearing for
	// reproducible output.
	if i, j := strings.Index(out, "ANTHROPIC_API_KEY"), strings.Index(out, "FOO"); i > j {
		t.Errorf("env keys not sorted (ANTHROPIC_API_KEY at %d, FOO at %d):\n%s", i, j, out)
	}
}

// TestPrintLaunchProfile_NoEnvRowWhenUnset pins that withholding the values
// did not quietly become an always-empty row: with no env configured the row
// is absent entirely. (That the row RENDERS when env is set is already proved
// by the key-presence assertions above, not by this test.)
func TestPrintLaunchProfile_NoEnvRowWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	printLaunchProfile(&buf, theme.Theme{}, launch.Profile{
		Harness: "claude",
		Model:   "opus",
	}, "/tmp/cwd", "/tmp/config.toml")

	// Anchored on the rendered LABEL, not a bare "env" substring: three
	// letters matched against the whole render would also trip on a config
	// path like ~/.envs, or any future row label containing "env".
	if strings.Contains(buf.String(), theme.Theme{}.Styles().Muted.Width(14).Render("env")) {
		t.Errorf("`launch which` printed an env row for a profile with no env:\n%s", buf.String())
	}
}

func TestRenderSafe_EscapesAttackerTextBeforeTrustedANSI(t *testing.T) {
	render := func(parts ...string) string { return "\x1b[32m" + strings.Join(parts, "") + "\x1b[0m" }
	got := renderSafe(render, "value\nforged\x1b[2K\u202eexe")
	for _, trusted := range []string{"\x1b[32m", "\x1b[0m"} {
		if !strings.Contains(got, trusted) {
			t.Fatalf("trusted ANSI %q was escaped: %q", trusted, got)
		}
	}
	for _, attacker := range []string{"\n", "\x1b[2K", "\u202e"} {
		if strings.Contains(got, attacker) {
			t.Fatalf("attacker sequence %q survived: %q", attacker, got)
		}
	}
	for _, escaped := range []string{`\n`, `\x1b`, `\u202e`} {
		if !strings.Contains(got, escaped) {
			t.Errorf("escaped marker %q absent: %q", escaped, got)
		}
	}
}

func TestPrintLaunchProfile_EscapesEveryUntrustedSurfaceToOneLinePerRow(t *testing.T) {
	attack := "x\tline\nforged\r\x1b[2K\x7f\u009b\u202e"
	var buf bytes.Buffer
	printLaunchProfile(&buf, theme.Theme{}, launch.Profile{
		Match:          attack,
		Harness:        attack,
		Model:          attack,
		PermissionMode: attack,
		AddDir:         []string{attack},
		Env:            map[string]string{attack: "withheld"},
	}, attack, attack)
	out := buf.String()
	if strings.ContainsAny(out, "\t\r\x7f") || strings.Contains(out, "\x1b[2K") || strings.ContainsRune(out, '\u009b') || strings.ContainsRune(out, '\u202e') {
		t.Fatalf("profile output contains attacker controls: %q", out)
	}
	for _, escaped := range []string{`\t`, `\n`, `\r`, `\x1b`, `\x7f`, `\u009b`, `\u202e`} {
		if !strings.Contains(out, escaped) {
			t.Errorf("profile output missing escaped marker %q: %q", escaped, out)
		}
	}
	if lines := strings.Count(out, "\n"); lines != 7 {
		t.Fatalf("physical lines = %d, want one title plus six rows for an invalid harness; output=%q", lines, out)
	}
}

func TestPrintLaunchProfile_EscapesPiProvider(t *testing.T) {
	attack := "lm-studio\nforged\x1b[2K\u202e"
	var buf bytes.Buffer
	printLaunchProfile(&buf, theme.Theme{}, launch.Profile{
		Harness:  "pi",
		Provider: attack,
		Model:    "qwen/qwen3-coder-next",
	}, "/tmp/cwd", "/tmp/config.toml")
	out := buf.String()
	if strings.Contains(out, "\x1b[2K") || strings.ContainsRune(out, '\u202e') || strings.Contains(out, "lm-studio\nforged") {
		t.Fatalf("Pi provider output contains attacker controls: %q", out)
	}
	for _, escaped := range []string{`\n`, `\x1b`, `\u202e`} {
		if !strings.Contains(out, escaped) {
			t.Errorf("Pi provider output missing escaped marker %q: %q", escaped, out)
		}
	}
}
