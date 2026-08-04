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

// TestPrintLaunchProfile_NoEnvRowWhenUnset is the negative control for the
// assertions above: with no env configured the row is absent entirely, so a
// passing leak assertion means the row was rendered and withheld, not skipped.
func TestPrintLaunchProfile_NoEnvRowWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	printLaunchProfile(&buf, launch.Profile{
		Harness: "claude",
		Model:   "opus",
	}, "/tmp/cwd", "/tmp/config.toml")

	if strings.Contains(buf.String(), "env") {
		t.Errorf("`launch which` printed an env row for a profile with no env:\n%s", buf.String())
	}
}
