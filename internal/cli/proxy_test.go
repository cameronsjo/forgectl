package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
)

func TestProxyUseEmitsOnlySelectedProfileScript(t *testing.T) {
	const selectedValue = "opaque-selected-proxy-value"
	const otherValue = "opaque-other-proxy-value"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"selected": {HTTPProxy: selectedValue},
		"other":    {HTTPProxy: otherValue},
	}}}}
	cmd := newProxyCmd(deps)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"use", "selected"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	if !strings.Contains(stdout.String(), "HTTP_PROXY='"+selectedValue+"'") {
		t.Fatalf("stdout missing selected profile assignment: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), otherValue) || strings.Contains(stderr.String(), otherValue) {
		t.Fatalf("unselected profile value leaked: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestProxyUseErrorsLeakNoProfileValues(t *testing.T) {
	const opaqueValue = "proxy-value-must-not-leak"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"empty":  {},
		"filled": {HTTPProxy: opaqueValue},
	}}}}
	for _, args := range [][]string{{"use", "missing"}, {"use", "empty"}, {"use"}, {"use", "filled", "extra"}} {
		cmd := newProxyCmd(deps)
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		if err == nil {
			t.Fatalf("args %q unexpectedly succeeded", args)
		}
		combined := stdout.String() + stderr.String() + err.Error()
		if strings.Contains(combined, opaqueValue) {
			t.Fatalf("args %q leaked proxy value: %q", args, combined)
		}
		if stdout.Len() != 0 {
			t.Fatalf("args %q wrote a partial script: %q", args, stdout.String())
		}
	}
}

func TestProxyOffNeedsNoConfiguration(t *testing.T) {
	cmd := newProxyCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"off"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		if !strings.Contains(stdout.String(), name) {
			t.Errorf("off script omitted %s: %q", name, stdout.String())
		}
	}
}

// jsonObjectKeys decodes one JSON object and returns its sorted top-level keys.
func jsonObjectKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, raw)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestProxyListJSON(t *testing.T) {
	const configuredValue = "opaque-config-value-must-not-appear"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work":  {HTTPProxy: configuredValue},
		"alpha": {HTTPProxy: configuredValue + "-alpha"},
	}}}}

	stdout, combined, err := runProxyCmd(t, newProxyCmd(deps), "list", "--json")
	if err != nil {
		t.Fatalf("proxy list --json: %v", err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, stdout)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2:\n%s", len(rows), stdout)
	}
	if got, want := jsonObjectKeys(t, rows[0]), []string{"name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("row keys = %v, want %v", got, want)
	}
	var decoded []proxyListRowJSON
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[0].Name != "alpha" || decoded[1].Name != "work" {
		t.Fatalf("names = %+v, want sorted alpha, work", decoded)
	}
	if strings.Contains(combined, configuredValue) {
		t.Fatalf("list --json leaked a profile value: %q", combined)
	}
}

func TestProxyListJSONEmptyIsArrayWithCleanStreams(t *testing.T) {
	cmd := newProxyCmd(module.Deps{})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"list", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("proxy list --json: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("stdout = %q, want []", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty under --json", errOut.String())
	}
}

func TestProxyStatusJSONMatched(t *testing.T) {
	const liveValue = "http://opaque-live-value.example:8080"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: liveValue},
	}}}}

	stdout, combined, err := runProxyCmd(t, newProxyStatusCmd(deps, fixtureLookup(bothSpellings("http_proxy", "HTTP_PROXY", liveValue))), "--json")
	if err != nil {
		t.Fatalf("proxy status --json: %v", err)
	}
	if got, want := jsonObjectKeys(t, json.RawMessage(stdout)), []string{"matched", "profile", "variables"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	var status proxyStatusJSON
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	want := proxyStatusJSON{Matched: true, Profile: "work", Variables: []proxyVariableJSON{
		{Name: "http_proxy", Set: true},
		{Name: "https_proxy", Set: false},
		{Name: "all_proxy", Set: false},
		{Name: "no_proxy", Set: false},
	}}
	if !reflect.DeepEqual(status, want) {
		t.Fatalf("status = %+v, want %+v", status, want)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatal(err)
	}
	var variables []json.RawMessage
	if err := json.Unmarshal(raw["variables"], &variables); err != nil {
		t.Fatal(err)
	}
	if got, want := jsonObjectKeys(t, variables[0]), []string{"name", "set"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("variable keys = %v, want %v", got, want)
	}
	if strings.Contains(combined, liveValue) || strings.Contains(combined, "opaque-live-value") {
		t.Fatalf("status --json leaked a proxy value: %q", combined)
	}
}

func TestProxyStatusJSONUnmatchedIsCategoryOnly(t *testing.T) {
	const configurationOnly = "http://opaque-config-only.example:8080"
	const environmentOnly = "http://opaque-env-only.example:8080"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: configurationOnly},
	}}}}

	stdout, combined, err := runProxyCmd(t,
		newProxyStatusCmd(deps, fixtureLookup(bothSpellings("http_proxy", "HTTP_PROXY", environmentOnly))), "--json")
	if err != nil {
		t.Fatalf("proxy status --json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got := string(raw["variables"]); got != "[]" {
		t.Fatalf("variables = %s, want [] (no per-variable facts without a match)", got)
	}
	if got := string(raw["matched"]); got != "false" {
		t.Fatalf("matched = %s, want false", got)
	}
	if got := string(raw["profile"]); got != `""` {
		t.Fatalf("profile = %s, want empty", got)
	}
	for _, v := range []string{configurationOnly, environmentOnly, "opaque-"} {
		if strings.Contains(combined, v) {
			t.Fatalf("status --json leaked %q: %q", v, combined)
		}
	}
}
