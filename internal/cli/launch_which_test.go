package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/launch"
)

// TestPrintLaunchProfile_EnvValuesAreWithheld: `launch which`'s env row prints
// key names only. The row is the one place in this renderer that echoes
// arbitrary config-supplied strings, and `[launch.defaults.env]` is where an
// ANTHROPIC_API_KEY lives — `which` output gets pasted into issues and shared
// terminals. Mirrors config's TestConfig_EnvMapValuesAreWithheld; the two
// surfaces render the same map under the same policy.
func TestPrintLaunchProfile_EnvValuesAreWithheld(t *testing.T) {
	var buf bytes.Buffer
	printLaunchProfile(&buf, launch.Profile{
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
	printLaunchProfile(&buf, launch.Profile{
		Harness: "claude",
		Model:   "opus",
	}, "/tmp/cwd", "/tmp/config.toml")

	// Anchored on the rendered LABEL, not a bare "env" substring: three
	// letters matched against the whole render would also trip on a config
	// path like ~/.envs, or any future row label containing "env".
	if strings.Contains(buf.String(), launchLabelStyle.Render("env")) {
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
	printLaunchProfile(&buf, launch.Profile{
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
	printLaunchProfile(&buf, launch.Profile{
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
