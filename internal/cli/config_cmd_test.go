package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
)

// runConfig executes `forgectl config <args…>` in-process against an isolated
// HOME/XDG_CONFIG_HOME holding body as config.toml. An empty body writes no
// file at all — the defaults-only posture. Returns combined output.
func runConfig(t *testing.T, body string, args ...string) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", base)
	if body != "" {
		path := childConfigPath(base)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir config dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write config.toml: %v", err)
		}
	}

	cmd := newConfigCmd(module.Deps{})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forgectl config %v: %v\noutput:\n%s", args, err, buf.String())
	}
	return buf.String()
}

// configJSONDoc and configJSONEntry mirror the --json contract. They are
// declared here rather than reusing configReport/configEntry so a silent
// rename of a JSON tag fails the test instead of travelling through it.
type configJSONDoc struct {
	Path         string            `json:"path"`
	Found        bool              `json:"found"`
	DecodeError  string            `json:"decode_error"`
	Unrecognized []string          `json:"unrecognized"`
	Entries      []configJSONEntry `json:"entries"`
	HostResolved struct {
		LogLevel string `json:"log_level"`
		LogFile  string `json:"log_file"`
	} `json:"host_resolved"`
	LaunchResolved struct {
		Harness  string `json:"harness"`
		Projects int    `json:"projects"`
	} `json:"launch_resolved"`
}

type configJSONEntry struct {
	Key      string `json:"key"`
	Group    string `json:"group"`
	Value    any    `json:"value"`
	Set      bool   `json:"set"`
	Redacted bool   `json:"redacted"`
}

// runConfigJSONRaw is runConfig --json, returning the decoded document and the
// raw payload — the raw form so a test can scan the WHOLE output for a leaked
// secret rather than one field at a time.
func runConfigJSONRaw(t *testing.T, body string) (configJSONDoc, string) {
	t.Helper()
	out := runConfig(t, body, "--json")
	var doc configJSONDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode --json output: %v\noutput:\n%s", err, out)
	}
	return doc, out
}

// runConfigJSON is runConfigJSONRaw without the raw payload.
func runConfigJSON(t *testing.T, body string) configJSONDoc {
	t.Helper()
	doc, _ := runConfigJSONRaw(t, body)
	return doc
}

// findEntry returns the entry for a dotted key, failing when absent.
func findEntry(t *testing.T, doc configJSONDoc, key string) configJSONEntry {
	t.Helper()
	for _, e := range doc.Entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry for key %q; got %d entries", key, len(doc.Entries))
	panic("unreachable")
}

// topLevelHeaderRe matches a top-level section header at column 0. Nested
// headers ([review.gitea], [launch.defaults]) are indented and carry a dot, so
// they are deliberately excluded — this counts sections, not subsections.
var topLevelHeaderRe = regexp.MustCompile(`(?m)^\[[a-z_]+\]$`)

// TestConfig_PrintsEverySection is the anti-staleness pin: the expected set is
// DERIVED from config.Config by reflection (configStructSections, shared with
// modules_test.go), never hand-written, so a 12th section fails this test on
// the commit that adds it. The original defect was exactly a hand-maintained
// render list going stale — a hand-written list here would relocate that defect
// into the test file rather than remove it.
func TestConfig_PrintsEverySection(t *testing.T) {
	sections := configStructSections(t)
	// Self-check: if the derivation ever returns empty (a refactor to
	// non-struct sections), every assertion below passes vacuously.
	if len(sections) == 0 {
		t.Fatal("configStructSections derived no sections — the pin would pass vacuously")
	}

	out := runConfig(t, "")

	for name := range sections {
		if !strings.Contains(out, "\n["+name+"]\n") {
			t.Errorf("output has no [%s] section header; got:\n%s", name, out)
		}
	}

	// Substring control: "the name appears somewhere" passes spuriously — net
	// is a substring of internal, docs of docs_dir, clean of cleanup. A count
	// equality over anchored headers cannot be satisfied by loose matching.
	if got := len(topLevelHeaderRe.FindAllString(out, -1)); got != len(sections) {
		t.Errorf("rendered %d top-level section headers, want %d (one per config.Config struct section); got:\n%s",
			got, len(sections), out)
	}
}

// TestConfig_TypoIsFlagged covers the case that motivated the issue: a
// misspelled key is silently ignored by the decoder, and value-only output
// renders it identically to "never set".
func TestConfig_TypoIsFlagged(t *testing.T) {
	doc := runConfigJSON(t, "[net]\nprobe_hostt = \"10.0.0.1\"\n")

	if len(doc.Unrecognized) != 1 || doc.Unrecognized[0] != "net.probe_hostt" {
		t.Errorf("unrecognized = %v, want exactly [net.probe_hostt]", doc.Unrecognized)
	}

	out := runConfig(t, "[net]\nprobe_hostt = \"10.0.0.1\"\n")
	if !strings.Contains(out, "unrecognized keys") || !strings.Contains(out, "net.probe_hostt") {
		t.Errorf("human output does not flag the typo; got:\n%s", out)
	}
}

// TestConfig_CorrectKeyIsNotFlagged is the negative control for
// TestConfig_TypoIsFlagged: a detector that flags everything passes that test.
func TestConfig_CorrectKeyIsNotFlagged(t *testing.T) {
	doc := runConfigJSON(t, "[net]\nprobe_host = \"10.0.0.1\"\n")
	if len(doc.Unrecognized) != 0 {
		t.Errorf("unrecognized = %v on a config with only valid keys, want empty", doc.Unrecognized)
	}
}

// TestConfig_EnvMapKeysAreNotFlagged pins the assumption the unrecognized-key
// detector rests on: LaunchDefaults.Env is map[string]string, so the decoder
// consumes arbitrary user keys under it wholesale. A false "unrecognized key"
// on legitimate config is worse than the silence this PR removes.
func TestConfig_EnvMapKeysAreNotFlagged(t *testing.T) {
	doc := runConfigJSON(t, "[launch.defaults.env]\nFOO = \"bar\"\nANTHROPIC_THING = \"x\"\n")
	if len(doc.Unrecognized) != 0 {
		t.Errorf("unrecognized = %v for arbitrary [launch.defaults.env] keys, want empty", doc.Unrecognized)
	}
	e := findEntry(t, doc, "launch.defaults.env")
	if !e.Set {
		t.Errorf("launch.defaults.env set = false, want true")
	}
}

// TestConfig_ProvenanceMarker asserts BOTH directions on the same key. A
// marker hardcoded to "set" passes a one-sided test.
func TestConfig_ProvenanceMarker(t *testing.T) {
	set := runConfigJSON(t, "[net]\nprobe_host = \"10.0.0.1\"\n")
	if e := findEntry(t, set, "net.probe_host"); !e.Set || e.Value != "10.0.0.1" {
		t.Errorf("net.probe_host = %#v set=%v, want value 10.0.0.1 set=true", e.Value, e.Set)
	}
	// A sibling key inside the SAME present section must still read default —
	// provenance is per key, not per section.
	if e := findEntry(t, set, "net.probe_port"); e.Set {
		t.Errorf("net.probe_port set = true, want false (the section is present but this key is not)")
	}

	absent := runConfigJSON(t, "")
	if e := findEntry(t, absent, "net.probe_host"); e.Set {
		t.Errorf("net.probe_host set = true with no config file, want false")
	}

	setText := runConfig(t, "[net]\nprobe_host = \"10.0.0.1\"\n")
	if !strings.Contains(setText, "(set)") {
		t.Errorf("human output carries no (set) marker; got:\n%s", setText)
	}
	absentText := runConfig(t, "")
	if strings.Contains(absentText, "(set)") {
		t.Errorf("human output marks a key (set) with no config file at all; got:\n%s", absentText)
	}
	if !strings.Contains(absentText, "(default)") {
		t.Errorf("human output carries no (default) marker; got:\n%s", absentText)
	}
}

// TestConfig_RedactsDSN covers the security half: [sessions] dsn can carry a
// password, and this command's output is the kind of thing pasted into an
// issue.
func TestConfig_RedactsDSN(t *testing.T) {
	const body = "[sessions]\ndsn = \"postgres://u:hunter2@h:5433/db\"\n"

	out := runConfig(t, body)
	if strings.Contains(out, "hunter2") {
		t.Errorf("human output leaks the DSN password; got:\n%s", out)
	}
	// Signal-survival control: redaction that erases the line passes the
	// "does not contain hunter2" assertion while destroying the one thing the
	// command exists to report.
	if !strings.Contains(out, "sessions.dsn") {
		t.Errorf("redaction dropped the sessions.dsn line entirely; got:\n%s", out)
	}

	doc, raw := runConfigJSONRaw(t, body)
	if strings.Contains(raw, "hunter2") {
		t.Errorf("--json output leaks the DSN password; got:\n%s", raw)
	}
	e := findEntry(t, doc, "sessions.dsn")
	if !e.Set {
		t.Error("sessions.dsn set = false, want true — redaction must not cost the provenance signal")
	}
	if !e.Redacted {
		t.Error("sessions.dsn redacted = false, want true")
	}
	if e.Value != "(redacted)" {
		t.Errorf("sessions.dsn value = %#v, want \"(redacted)\"", e.Value)
	}
}

// TestConfig_UnsetDSNIsNotMarkedRedacted is the negative control for
// redaction: an absent secret has nothing to hide, and marking it redacted
// would misreport "unset" as "set but withheld".
func TestConfig_UnsetDSNIsNotMarkedRedacted(t *testing.T) {
	e := findEntry(t, runConfigJSON(t, ""), "sessions.dsn")
	if e.Redacted || e.Set {
		t.Errorf("sessions.dsn redacted=%v set=%v with no config file, want both false", e.Redacted, e.Set)
	}
}

// TestConfig_EnvMapValuesAreWithheld is the DSN threat model applied to the
// key most likely to hold a credential: [launch.defaults.env] is arbitrary
// environment injected into the launched harness, so ANTHROPIC_API_KEY and
// GH_TOKEN live there by construction. redactedKeys cannot reach into a map,
// so leafValue renders map leaves as sorted keys only.
//
// Both halves are load-bearing. Without the leak assertion the redaction can
// silently regress; without the survival assertion "withhold everything" —
// dropping the line, emptying the map — passes.
func TestConfig_EnvMapValuesAreWithheld(t *testing.T) {
	// Distinct, unlikely-to-collide values so "the raw output does not contain
	// this" is about THIS value, not an accidental absence. FOO is here to
	// prove the policy is every value, not just the secret-looking one.
	const body = "[launch.defaults.env]\nANTHROPIC_API_KEY = \"sk-ant-hunter2\"\nFOO = \"plainbarvalue\"\n"

	out := runConfig(t, body)
	for _, secret := range []string{"sk-ant-hunter2", "plainbarvalue"} {
		if strings.Contains(out, secret) {
			t.Errorf("human output leaks env value %q; got:\n%s", secret, out)
		}
	}
	// Signal survival: the key set is what makes the line worth printing.
	for _, key := range []string{"ANTHROPIC_API_KEY", "FOO"} {
		if !strings.Contains(out, key) {
			t.Errorf("human output dropped env key %q — withholding values must not cost the key set; got:\n%s", key, out)
		}
	}

	doc, raw := runConfigJSONRaw(t, body)
	for _, secret := range []string{"sk-ant-hunter2", "plainbarvalue"} {
		if strings.Contains(raw, secret) {
			t.Errorf("--json output leaks env value %q; got:\n%s", secret, raw)
		}
	}
	e := findEntry(t, doc, "launch.defaults.env")
	if !e.Set {
		t.Error("launch.defaults.env set = false, want true — withholding must not cost the provenance signal")
	}
	if !e.Redacted {
		t.Error("launch.defaults.env redacted = false, want true — the values were withheld and the JSON must say so")
	}
	keys, ok := e.Value.([]any)
	if !ok || len(keys) != 2 || keys[0] != "ANTHROPIC_API_KEY" || keys[1] != "FOO" {
		t.Errorf("launch.defaults.env value = %#v, want the sorted key list [ANTHROPIC_API_KEY FOO]", e.Value)
	}
}

// TestConfig_UnsetEnvMapIsNotMarkedRedacted is the negative control for the
// map policy, mirroring TestConfig_UnsetDSNIsNotMarkedRedacted: an empty map
// withheld nothing, and claiming otherwise misreports "unset" as "set but
// withheld".
func TestConfig_UnsetEnvMapIsNotMarkedRedacted(t *testing.T) {
	e := findEntry(t, runConfigJSON(t, ""), "launch.defaults.env")
	if e.Redacted || e.Set {
		t.Errorf("launch.defaults.env redacted=%v set=%v with no config file, want both false", e.Redacted, e.Set)
	}
}

func TestConfig_ProxyProfileNamesVisibleValuesWithheld(t *testing.T) {
	const opaqueValue = "opaque-proxy-value-that-must-not-appear"
	body := "[proxy.profiles.work]\nhttp_proxy = \"" + opaqueValue + "\"\n"

	text := runConfig(t, body)
	if strings.Contains(text, opaqueValue) {
		t.Fatalf("human config output leaked proxy value: %q", text)
	}
	if !strings.Contains(text, "work") {
		t.Fatalf("human config output hid configured profile name: %q", text)
	}

	doc, raw := runConfigJSONRaw(t, body)
	e := findEntry(t, doc, "proxy.profiles")
	if !e.Redacted || !e.Set {
		t.Fatalf("proxy.profiles redacted=%v set=%v, want both true", e.Redacted, e.Set)
	}
	if strings.Contains(raw, opaqueValue) {
		t.Fatalf("JSON config output leaked proxy value: %q", raw)
	}
}

// TestConfig_EveryBindableFieldIsTagged extends configStructSections' fatal
// from top-level sections to every field at every depth.
//
// BurntSushi binds an exported field with no `toml` tag by its Go name
// (type_fields.go), matched exactly and then case-insensitively. So an
// untagged `Foo string` is settable as `Foo = "x"`, decodes correctly, and —
// because it DID decode — is not reported under "unrecognized keys" either.
// That is the defect this command exists to remove, relocated one level down.
//
// walkStruct's tomlFieldName fallback keeps such a field visible, but cannot
// recover its (set)/(default) marker: config.Report.IsSet keys on the file's
// own spelling. So the fallback is the safety net and this test is the rule.
func TestConfig_EveryBindableFieldIsTagged(t *testing.T) {
	seen := map[reflect.Type]bool{}
	audited := 0

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		// Reach the struct behind a pointer, slice, array, or map —
		// [[launch.project]] elements are bindable the same way.
		for {
			switch typ.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
				typ = typ.Elem()
				continue
			}
			break
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			audited++
			if tag, _, _ := strings.Cut(f.Tag.Get("toml"), ","); tag == "" {
				t.Errorf("%s.%s has no toml tag: the decoder still binds it by Go name, so it is settable in config.toml "+
					"with no reliable provenance and no unrecognized-key warning. Give it `toml:\"<key>\"`, or `toml:\"-\"` "+
					"to keep it out of the file entirely.", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(config.Config{}), "config.Config")

	// Self-check: a derivation that visits nothing passes every assertion.
	if audited == 0 {
		t.Fatal("audited no fields — the walk derived nothing and would pass vacuously")
	}
}

// TestConfig_DecodeErrorSurfaces covers the fourth face of the overloaded zero
// value: a wrong-typed value aborts the decode partway, Load tolerates it with
// a warning, and every affected key then renders exactly like "never set".
func TestConfig_DecodeErrorSurfaces(t *testing.T) {
	const body = "[net]\nprobe_port = \"443\"\n"

	out := runConfig(t, body)
	if !strings.Contains(out, "decode error") {
		t.Errorf("human output does not surface the decode error; got:\n%s", out)
	}

	doc := runConfigJSON(t, body)
	if doc.DecodeError == "" {
		t.Error("decode_error is empty for a wrong-typed value")
	}
	if !doc.Found {
		t.Error("found = false for a file that exists but failed to parse")
	}
	// A decode aborts partway, so every key after the failure point is
	// trivially undecoded — reporting those as "unrecognized" would be a false
	// positive on a config whose only real fault is the type error.
	if len(doc.Unrecognized) != 0 {
		t.Errorf("unrecognized = %v alongside a decode error, want empty", doc.Unrecognized)
	}
}

// TestConfig_MissingFileIsNotAnError pins the tolerant posture: no config file
// is the normal defaults-only state, not a failure.
func TestConfig_MissingFileIsNotAnError(t *testing.T) {
	out := runConfig(t, "")
	if !strings.Contains(out, "not found — using defaults") {
		t.Errorf("output does not report the missing file as the defaults posture; got:\n%s", out)
	}
	if doc := runConfigJSON(t, ""); doc.Found {
		t.Error("found = true with no config file on disk")
	}
}

// TestConfig_AwkwardLeafTypes covers the three shapes a generic walk gets
// wrong: a *bool (explicit false must not collapse into unset), a
// map[string]string, and a []string. All three live under [launch.defaults].
func TestConfig_AwkwardLeafTypes(t *testing.T) {
	doc := runConfigJSON(t, "[launch.defaults]\nallow_danger = false\nadd_dir = [\"/tmp/a\"]\nenv = { A = \"1\" }\n")

	if e := findEntry(t, doc, "launch.defaults.allow_danger"); !e.Set || e.Value != false {
		t.Errorf("allow_danger = %#v set=%v, want false/true — an explicit false must stay distinguishable from unset", e.Value, e.Set)
	}
	if e := findEntry(t, doc, "launch.defaults.add_dir"); !e.Set {
		t.Errorf("add_dir set = false, want true")
	}
	if e := findEntry(t, doc, "launch.defaults.env"); !e.Set {
		t.Errorf("env set = false, want true")
	}

	// The nil *bool must render null, not false.
	absent := findEntry(t, runConfigJSON(t, ""), "launch.defaults.allow_danger")
	if absent.Value != nil {
		t.Errorf("allow_danger = %#v with no config file, want null (nil pointer, not false)", absent.Value)
	}
	// A nil []string must render [], never null.
	if e := findEntry(t, runConfigJSON(t, ""), "docs.roots"); e.Value == nil {
		t.Error("docs.roots = null for an unset slice, want []")
	}
}

// TestConfig_HostScalarsResolved pins the two resolutions the pre-fix command
// performed and a generic walk cannot: log_level "" means "off", and log_file
// "" means today's dated file under the config dir. The walk reports the
// stored value, so without the resolved block the rewrite would have silently
// dropped both.
func TestConfig_HostScalarsResolved(t *testing.T) {
	out := runConfig(t, "")
	if !strings.Contains(out, "log_level  off") {
		t.Errorf("output does not resolve an empty log_level to \"off\"; got:\n%s", out)
	}
	doc := runConfigJSON(t, "")
	if doc.HostResolved.LogLevel != "off" {
		t.Errorf("host_resolved.log_level = %q, want \"off\"", doc.HostResolved.LogLevel)
	}
	if !strings.Contains(doc.HostResolved.LogFile, "forgectl-") {
		t.Errorf("host_resolved.log_file = %q, want the dated auto path", doc.HostResolved.LogFile)
	}

	// Both directions: an explicit level must resolve to itself, not to the
	// fallback. A resolver hardcoded to "off" passes a one-sided test.
	explicit := runConfigJSON(t, "log_level = \"debug\"\nlog_file = \"-\"\n")
	if explicit.HostResolved.LogLevel != "debug" {
		t.Errorf("host_resolved.log_level = %q for an explicit debug, want \"debug\"", explicit.HostResolved.LogLevel)
	}
	if explicit.HostResolved.LogFile != "stderr" {
		t.Errorf("host_resolved.log_file = %q for log_file=\"-\", want \"stderr\"", explicit.HostResolved.LogFile)
	}
}

// TestConfig_NestedSectionRendered pins [review.gitea], the one SUBSECTION —
// nested under Review, so configStructSections' eleven top-level sections do
// not include it, and TestConfig_PrintsEverySection's anchored header count
// deliberately excludes it. It was omitted by the pre-fix renderer alongside
// the ten missing top-level sections, and only this test covers it.
func TestConfig_NestedSectionRendered(t *testing.T) {
	out := runConfig(t, "[review.gitea]\nhost = \"git.example\"\n")
	if !strings.Contains(out, "[review.gitea]") {
		t.Errorf("output has no [review.gitea] subsection header; got:\n%s", out)
	}
	if e := findEntry(t, runConfigJSON(t, "[review.gitea]\nhost = \"git.example\"\n"), "review.gitea.host"); !e.Set {
		t.Error("review.gitea.host set = false, want true")
	}
}
