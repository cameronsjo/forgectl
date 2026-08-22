package cli

import (
	"bytes"
	"context"
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
