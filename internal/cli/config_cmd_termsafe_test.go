package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// termControls are the runes an untrusted fragment must never contribute to a
// human rendering: they erase or move terminal rows (ESC, CR, C1 CSI), drive
// OSC title/clipboard payloads (BEL), or reorder the visible line (RLO).
// Newline is absent deliberately — the renderer authors its own, so an injected
// one is caught by assertUnsplitRow rather than by a global ban.
//
// Spelled as numeric escapes, never literal bytes: a source file carrying a raw
// ESC or RLO is itself the hazard this package exists to prevent, and editors,
// diff viewers, and code review surfaces all mangle it.
var termControls = []rune{0x00, 0x07, 0x1b, 0x7f, '\r', '\u009b', '\u202e'}

// hostilePayload builds one untrusted fragment carrying every control class
// #250 must neutralize, bracketed by a marker-derived head and tail so a test
// can prove the value was REPLACED rather than truncated or row-split.
//
// The C1 CSI form (\u009b) is included because it drives the same cursor
// operations as the two-byte ESC-[ form while surviving naive ESC-only
// filtering — a sanitizer that strips 0x1b alone still passes 0x9b through.
func hostilePayload(marker string) string {
	return hostileHead(marker) + "\x1b[2K\x1b]0;forged\x07\r\n\x00\x7f\u009b6n\u202e" + hostileTail(marker)
}

func hostileHead(marker string) string { return "h" + marker }
func hostileTail(marker string) string { return "t" + marker }

func assertNoTerminalControls(t *testing.T, out string) {
	t.Helper()
	for _, r := range termControls {
		if strings.ContainsRune(out, r) {
			t.Errorf("rendering carries terminal control U+%04X from an untrusted fragment:\n%q", r, out)
		}
	}
}

// assertUnsplitRow proves an embedded newline did not splice the fragment's
// row: the head and tail of one payload must still share a physical line.
func assertUnsplitRow(t *testing.T, out, marker string) {
	t.Helper()
	head, tail := hostileHead(marker), hostileTail(marker)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, head) && strings.Contains(line, tail) {
			return
		}
	}
	t.Errorf("no single line carries both %q and %q — the %s fragment split its row:\n%s", head, tail, marker, out)
}

// hostileTextFixture is the shared entry/host/launch material for the direct
// renderConfigText cases: every untrusted string leaf gets a distinct payload,
// so a partial fix fails on the marker it missed rather than riding a
// neighbour's coverage.
func hostileTextFixture() ([]configEntry, hostResolvedView, launchResolvedView) {
	entries := []configEntry{
		{Key: "log_level", Group: "", display: hostilePayload("root"), Set: true},
		{Key: "docs.roots", Group: "docs", display: hostilePayload("leaf"), Set: true},
	}
	hosts := hostResolvedView{
		LogLevel: hostilePayload("level"),
		LogFile:  hostilePayload("logfile"),
	}
	resolved := launchResolvedView{
		Harness:        hostilePayload("harness"),
		Model:          hostilePayload("model"),
		Effort:         hostilePayload("effort"),
		PermissionMode: hostilePayload("perm"),
		BinaryLabel:    "launch.claude_bin",
		BinaryPath:     hostilePayload("binpath"),
	}
	return entries, hosts, resolved
}

// TestRenderConfigText_SanitizesEveryUntrustedFragment is the direct oracle for
// the human config sink. It covers all three config-file status arms plus both
// harness arms of the resolved block, because each renders a different set of
// external fragments — and a per-arm fix is exactly how a sink stays half-open.
//
// The first case sets DecodeErr and Unrecognized together. config.Describe
// never produces that pair (a decode aborts before key binding), but
// renderConfigText is a pure function and the point here is to exercise both
// writers in one pass, not to model a reachable Report.
func TestRenderConfigText_SanitizesEveryUntrustedFragment(t *testing.T) {
	entries, hosts, claudeResolved := hostileTextFixture()

	codexResolved := claudeResolved
	codexResolved.Harness = "codex"
	codexResolved.ApprovalPolicy = hostilePayload("approval")
	codexResolved.Sandbox = hostilePayload("sandbox")

	tests := []struct {
		name     string
		rep      config.Report
		resolved launchResolvedView
		markers  []string
	}{
		{
			name: "path found with decode error",
			rep: config.Report{
				Path:         hostilePayload("path"),
				Found:        true,
				DecodeErr:    errors.New(hostilePayload("decode")),
				Unrecognized: []string{hostilePayload("unrec")},
			},
			resolved: claudeResolved,
			markers:  []string{"path", "decode", "unrec", "root", "leaf", "level", "logfile", "harness", "model", "effort", "perm", "binpath"},
		},
		{
			name:     "path unresolvable",
			rep:      config.Report{PathErr: errors.New(hostilePayload("patherr"))},
			resolved: claudeResolved,
			markers:  []string{"patherr", "root", "leaf", "level", "logfile", "harness", "model", "binpath"},
		},
		{
			name:     "path not found",
			rep:      config.Report{Path: hostilePayload("path"), Found: false},
			resolved: claudeResolved,
			markers:  []string{"path", "root", "leaf", "binpath"},
		},
		{
			name:     "codex arm",
			rep:      config.Report{Path: hostilePayload("path"), Found: true},
			resolved: codexResolved,
			markers:  []string{"path", "approval", "sandbox", "model", "binpath"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderConfigText(&buf, entries, tc.rep, hosts, tc.resolved)
			out := buf.String()

			assertNoTerminalControls(t, out)
			for _, m := range tc.markers {
				if !strings.Contains(out, hostileTail(m)) {
					t.Errorf("the %s fragment lost its visible suffix %q — sanitization must replace, not drop evidence:\n%s", m, hostileTail(m), out)
				}
				assertUnsplitRow(t, out, m)
			}

			// Trusted layout is not collateral: code-authored labels, section
			// headers, and structural newlines must survive untouched.
			for _, want := range []string{"config file:", "\n[docs]\n", "  launch.claude_bin"} {
				if !strings.Contains(out, want) {
					t.Errorf("trusted layout fragment %q missing — sanitization reached renderer-authored text:\n%s", want, out)
				}
			}
		})
	}
}

// hostileConfigBody carries the same control classes through a real TOML
// decode, so the test pins the whole walkConfig -> leafValue -> resolve ->
// write chain rather than the renderer in isolation. The controls are written
// as TOML \u escapes for the same reason the Go ones are: no raw bytes in
// source. NUL is omitted — TOML parsers legitimately reject it, and the direct
// renderer case already covers it.
const hostileConfigBody = `log_level = "debug"

[docs]
roots = ["hleaf\u001b[2K\r\u202etleaf"]

[launch.defaults]
model = "hmodel\u001b[2K\r\u202etmodel"
binary_path = "/tmp/forgectl-nonexistent/hbinpath\u001b[2K\u202etbinpath"
`

// TestConfig_HostileValuesAreInertInText is the end-to-end control: hostile
// values reach real command output through the config decoder, not through a
// hand-built fixture. It also exercises the binary-resolution error branch,
// because binary_path names a path that cannot resolve.
func TestConfig_HostileValuesAreInertInText(t *testing.T) {
	out := runConfig(t, hostileConfigBody)

	assertNoTerminalControls(t, out)
	for _, m := range []string{"leaf", "model", "binpath"} {
		if !strings.Contains(out, hostileTail(m)) {
			t.Errorf("the %s value lost its visible suffix %q in the config rendering:\n%s", m, hostileTail(m), out)
		}
		assertUnsplitRow(t, out, m)
	}
	if !strings.Contains(out, "unresolved") {
		t.Errorf("the unresolvable binary_path did not surface its diagnostic:\n%s", out)
	}
}

// TestConfig_JSONPreservesRawControlValues is the negative control that keeps
// the fix at the terminal boundary. If an implementation sanitizes inside
// walkConfig, leafValue, or resolveLaunchView instead, the decoded JSON stops
// reconstructing the operator's actual value and this fails.
//
// Cc controls are byte-faithful AND terminal-inert in JSON: the encoder emits
// them as \uXXXX escapes. Bidi Cf was faithful but NOT inert until #279 gave it
// the same treatment via termsafe.JSONEncoder — value-preserving encoding, so
// this control's round-trip assertion still holds unchanged. That is the point:
// closing the display gap must not cost the machine contract.
func TestConfig_JSONPreservesRawControlValues(t *testing.T) {
	const rawModel = "opus\u001b[2K\u202e-tail"
	const body = `[launch.defaults]
model = "opus\u001b[2K\u202e-tail"
`

	out := runConfig(t, body, "--json")

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("--json emitted a literal ESC byte; the encoder must escape Cc:\n%q", out)
	}
	if !strings.Contains(out, "\\u001b") {
		t.Errorf("--json lost the ESC escape — the stored value was mutated for presentation:\n%s", out)
	}

	var doc struct {
		Entries []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"entries"`
		LaunchResolved struct {
			Model string `json:"model"`
		} `json:"launch_resolved"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, out)
	}
	if doc.LaunchResolved.Model != rawModel {
		t.Errorf("launch_resolved.model = %q, want the exact stored value %q", doc.LaunchResolved.Model, rawModel)
	}
	var seen bool
	for _, e := range doc.Entries {
		if e.Key != "launch.defaults.model" {
			continue
		}
		seen = true
		if e.Value != rawModel {
			t.Errorf("entries[launch.defaults.model].value = %v, want the exact stored value %q", e.Value, rawModel)
		}
	}
	if !seen {
		t.Error("no launch.defaults.model entry in the --json document")
	}
}

// TestConfig_JSONEscapesBidiFormatCharacters is the #279 counterpart to the
// negative control above. Cc controls were already byte-faithful AND
// terminal-inert in JSON because encoding/json escapes them; Bidi_Control
// formatting characters were faithful but NOT inert, so a stored
// RIGHT-TO-LEFT OVERRIDE reordered the visible text of a document piped
// straight to a terminal.
//
// The fix is value-preserving encoding, not sanitization: the bidi character
// is emitted as its \uXXXX escape, which a terminal renders as six inert ASCII
// characters and a decoder turns back into the exact stored rune. Both halves
// are asserted here, so an implementation that neutralizes by deleting or
// replacing the rune fails the round trip and the negative control both.
func TestConfig_JSONEscapesBidiFormatCharacters(t *testing.T) {
	const rawModel = "opus\u202e-tail\u2066\u2069\u200f\u061c"
	const body = `[launch.defaults]
model = "opus\u202E-tail\u2066\u2069\u200F\u061C"
`

	out := runConfig(t, body, "--json")

	for _, r := range []rune{'\u202e', '\u2066', '\u2069', '\u200f', '\u061c'} {
		if strings.ContainsRune(out, r) {
			t.Errorf("--json emitted literal bidi U+%04X; it must be \\u-escaped:\n%q", r, out)
		}
	}
	if !strings.Contains(out, "\\u202e") {
		t.Errorf("--json lost the RLO escape entirely — the stored value was mutated:\n%s", out)
	}

	var doc struct {
		LaunchResolved struct {
			Model string `json:"model"`
		} `json:"launch_resolved"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, out)
	}
	if doc.LaunchResolved.Model != rawModel {
		t.Errorf("launch_resolved.model = %q, want the exact stored value %q", doc.LaunchResolved.Model, rawModel)
	}
}
