package launch

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// proxySentinel is a value no display surface has any reason to carry, so a
// leak is unambiguous about where it came from.
const proxySentinel = "http://sentinel-proxy.example:8080"

// TestBuildInvocation_InjectedProxyReachesTheExecEnvOnly walks the disclosure
// boundary the launch proxy profile opens. Profile values are sensitive by the
// config type's own contract — every rendering verb answers "[redacted]" — and
// injecting them into a launched environment adds a second sanctioned sink
// alongside the shell protocol. The sink must work, and it must be the only
// place the value appears: the banner and `launch which` both render launch
// state to a terminal an operator may be sharing.
func TestBuildInvocation_InjectedProxyReachesTheExecEnvOnly(t *testing.T) {
	target := projectDir(t)

	built, err := BuildInvocation(InvocationRequest{
		Config: parityConfig(target),
		CWD:    target,
		// Both halves of the merge have something to do: a stale proxy the
		// injected value must override, and a bypass list the removal must take
		// out. A base carrying no NO_PROXY would let a broken StripEnv pass.
		BaseEnv: []string{
			"HTTPS_PROXY=http://stale-shell-proxy.example:9999",
			"NO_PROXY=localhost,127.0.0.1",
			"PATH=/usr/bin",
		},
		InjectedEnv: map[string]string{
			"HTTPS_PROXY": proxySentinel,
			"https_proxy": proxySentinel,
		},
		UnsetEnv: []string{"NO_PROXY", "no_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"},
		Resolve:  fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}

	// The sink itself. Without this the leak assertions below would pass on an
	// invocation that never carried the value at all.
	if !slices.Contains(built.Invocation.Env, "HTTPS_PROXY="+proxySentinel) {
		t.Fatalf("exec env is missing the injected proxy, so this test cannot detect a leak: %v", built.Invocation.Env)
	}
	if slices.Contains(built.Invocation.Env, "HTTPS_PROXY=http://stale-shell-proxy.example:9999") {
		t.Error("the calling shell's stale proxy survived into the child; the injected value must override it")
	}

	// Removal, not emptying. An empty NO_PROXY means "no bypass exceptions", so
	// leaving `NO_PROXY=` behind would route loopback traffic — including the
	// bench OTLP endpoint this same composer injects — through the proxy.
	for _, e := range built.Invocation.Env {
		if key, _, _ := strings.Cut(e, "="); key == "NO_PROXY" || key == "no_proxy" {
			t.Errorf("exec env still carries %q; an absent profile field must remove the variable", e)
		}
	}
	if !slices.Contains(built.Invocation.Env, "PATH=/usr/bin") {
		t.Error("the removal took an unrelated variable with it")
	}

	// The display surfaces. The profile carries no proxy env, which is what
	// keeps `launch which --json` env_keys (built from Profile.Env) clear of
	// it, and the banner renders argv rather than environment.
	for key := range built.Profile.Env {
		if strings.Contains(strings.ToLower(key), "proxy") {
			t.Errorf("Profile.Env carries %q; injected env must not land in the profile that `launch which` renders", key)
		}
	}

	banner := &bytes.Buffer{}
	EmitBanner(banner, built)
	if strings.Contains(banner.String(), proxySentinel) {
		t.Errorf("the launch banner leaked the proxy value: %q", banner.String())
	}
	if banner.Len() == 0 {
		t.Error("the banner rendered nothing, so its leak assertion passed vacuously")
	}
}
