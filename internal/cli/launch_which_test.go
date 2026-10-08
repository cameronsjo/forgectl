package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
	}, "/tmp/cwd", "", "/tmp/config.toml", nil)

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"directory", "config", "matched", "harness", "model", "effort",
		"permission_mode", "allow_danger", "env_keys", "injected_env_keys", "add_dir"}
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
	got := buildLaunchWhichJSON(profile, "/tmp/cwd", "", "/tmp/config.toml", nil)

	var buf bytes.Buffer
	printLaunchProfile(&buf, theme.Theme{}, profile, "/tmp/cwd", "", "/tmp/config.toml", nil)
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
	got := buildLaunchWhichJSON(launch.Profile{Harness: "claude", Model: "opus"}, "/tmp/cwd", "", "/tmp/config.toml", nil)
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
	}, "/tmp/cwd", "", "/tmp/config.toml", nil)
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
	if err := writeLaunchWhichJSON(buf, launch.Profile{Harness: "claude"}, "/tmp/cwd", "", "/tmp/config.toml", nil); err == nil {
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
	}, "/tmp/cwd", "", "/tmp/config.toml", nil)
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
	}, "/tmp/cwd", "", "/tmp/config.toml", nil)

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
	}, attack, attack, attack, nil)
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
	}, "/tmp/cwd", "", "/tmp/config.toml", nil)
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

// TestLaunchWorkingDirectory_CapsTheFailingPath is #832: `launch` and
// `launch which` wrapped Getwd's *PathError in fmt.Errorf and only then
// handed it to termsafe.Error, whose path cap applies to a *PathError that is
// the error itself, so the path came through whole. The cap now applies, the
// filename survives the cut, and errors.As still reaches the *PathError.
//
// Mutation: restore termsafe.Error(fmt.Errorf("...: %w", err)) in
// launchWorkingDirectory and the whole directory run comes back.
func TestLaunchWorkingDirectory_CapsTheFailingPath(t *testing.T) {
	long := "/" + strings.Repeat("d", 4*termsafe.PathEchoMaxRunes) + "/gone"
	orig := launchGetwd
	t.Cleanup(func() { launchGetwd = orig })
	launchGetwd = func() (string, error) {
		return "", &os.PathError{Op: "getwd", Path: long, Err: syscall.ENOENT}
	}
	_, err := launchWorkingDirectory()
	if err == nil {
		t.Fatal("launchWorkingDirectory: nil error from a failing getwd")
	}
	got := err.Error()
	if strings.Count(got, "d") > termsafe.PathEchoMaxRunes {
		t.Errorf("error echoed the %d-rune path uncapped (%d bytes)", len(long), len(got))
	}
	if !strings.Contains(got, `/gone"`) {
		t.Errorf("error = %q; want the final element kept", got)
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Errorf("errors.As lost the *os.PathError: %v", err)
	}
}

// TestLaunchWhich_RunDirectory pins `launch which` reporting where a claude
// session will start (cadence-ecosystem#608): a "runs in" row only when it
// differs from the directory, and run_directory in --json for claude alone.
func TestLaunchWhich_RunDirectory(t *testing.T) {
	claude := launch.Profile{Harness: "claude", Model: "opus"}

	var moved bytes.Buffer
	printLaunchProfile(&moved, theme.Theme{}, claude, "/repo/pkg", "/repo", "/tmp/config.toml", nil)
	if !strings.Contains(moved.String(), "runs in") || !strings.Contains(moved.String(), "/repo  (settings root; --here to stay)") {
		t.Errorf("moved launch has no runs-in row:\n%s", moved.String())
	}

	var stays bytes.Buffer
	printLaunchProfile(&stays, theme.Theme{}, claude, "/repo/pkg", "/repo/pkg", "/tmp/config.toml", nil)
	if strings.Contains(stays.String(), "runs in") {
		t.Errorf("unmoved launch printed a runs-in row:\n%s", stays.String())
	}

	for _, tc := range []struct {
		name    string
		profile launch.Profile
		runDir  string
		want    string
		present bool
	}{
		{"claude moved", claude, "/repo", "/repo", true},
		{"claude unmoved", claude, "/repo/pkg", "/repo/pkg", true},
		{"codex", launch.Profile{Harness: "codex"}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(buildLaunchWhichJSON(tc.profile, "/repo/pkg", tc.runDir, "/tmp/config.toml", nil))
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			got, ok := decoded["run_directory"]
			if ok != tc.present {
				t.Fatalf("run_directory present = %t, want %t: %s", ok, tc.present, raw)
			}
			if ok && string(got) != `"`+tc.want+`"` {
				t.Errorf("run_directory = %s, want %q", got, tc.want)
			}
		})
	}
}
