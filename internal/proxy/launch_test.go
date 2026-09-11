package proxy

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func launchFixture(profile config.ProxyProfile) config.ProxyConfig {
	return config.ProxyConfig{
		Profiles:      map[string]config.ProxyProfile{"work": profile},
		LaunchProfile: "work",
	}
}

func TestLaunchEnvSetsBothSpellingsForEveryVariable(t *testing.T) {
	env, err := LaunchEnv(launchFixture(config.ProxyProfile{
		HTTPProxy:  "http://proxy.example:8080",
		HTTPSProxy: "https://proxy.example:8443",
		AllProxy:   "socks5://proxy.example:1080",
		NoProxy:    "localhost,127.0.0.1",
	}))
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}

	want := map[string]string{
		"HTTP_PROXY":  "http://proxy.example:8080",
		"http_proxy":  "http://proxy.example:8080",
		"HTTPS_PROXY": "https://proxy.example:8443",
		"https_proxy": "https://proxy.example:8443",
		"ALL_PROXY":   "socks5://proxy.example:1080",
		"all_proxy":   "socks5://proxy.example:1080",
		"NO_PROXY":    "localhost,127.0.0.1",
		"no_proxy":    "localhost,127.0.0.1",
	}
	if len(env) != len(want) {
		t.Fatalf("LaunchEnv returned %d entries, want %d: %v", len(env), len(want), sortedKeys(env))
	}
	for key, value := range want {
		if env[key] != value {
			t.Errorf("env[%q] = %q, want %q", key, env[key], value)
		}
	}
}

// TestLaunchEnvCarriesAbsentFieldsAsEmpty pins the determinism the setting
// exists for. An omitted key would leave the calling shell's stale export
// alive in the child, because the merge that applies this map overrides key by
// key and cannot remove what it never names.
func TestLaunchEnvCarriesAbsentFieldsAsEmpty(t *testing.T) {
	env, err := LaunchEnv(launchFixture(config.ProxyProfile{HTTPSProxy: "https://proxy.example:8443"}))
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}

	for _, key := range []string{"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy", "HTTP_PROXY", "http_proxy"} {
		value, present := env[key]
		if !present {
			t.Errorf("env is missing %q, so a stale exported value would survive into the child", key)
			continue
		}
		if value != "" {
			t.Errorf("env[%q] = %q, want the empty string", key, value)
		}
	}
}

func TestLaunchEnvIsAbsentWithoutALaunchProfile(t *testing.T) {
	env, err := LaunchEnv(config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: "http://proxy.example:8080"},
	}})
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}
	if env != nil {
		t.Fatalf("LaunchEnv = %v, want nil when no launch profile is named", env)
	}
}

// TestLaunchEnvRefusesAnUnknownProfile is the rule that matters most: a typo
// must not fall back to launching unproxied, which would look like success
// while reaching the network by a path the operator did not choose.
func TestLaunchEnvRefusesAnUnknownProfile(t *testing.T) {
	env, err := LaunchEnv(config.ProxyConfig{
		Profiles:      map[string]config.ProxyProfile{"work": {HTTPProxy: "http://proxy.example:8080"}},
		LaunchProfile: "wrok",
	})
	if !errors.Is(err, ErrUnknownLaunchProfile) {
		t.Fatalf("err = %v, want ErrUnknownLaunchProfile", err)
	}
	if env != nil {
		t.Fatalf("a refused launch still returned an environment: %v", env)
	}
	// The name is not sensitive and naming it is what makes the error
	// actionable; the values are, and none are reachable here to leak.
	if got := err.Error(); !strings.Contains(got, `"wrok"`) {
		t.Errorf("err = %q, want it to name the missing profile", got)
	}
}

func TestLaunchEnvRefusesAnEmptyProfile(t *testing.T) {
	_, err := LaunchEnv(config.ProxyConfig{
		Profiles:      map[string]config.ProxyProfile{"work": {}},
		LaunchProfile: "work",
	})
	if !errors.Is(err, ErrEmptyProfile) {
		t.Fatalf("err = %v, want ErrEmptyProfile", err)
	}
}

// TestLaunchEnvRefusesAValueTheExecCannotCarry mirrors Use's NUL refusal. An
// environment entry handed to exec is a NUL-terminated C string, so the byte
// would truncate the value rather than fail loudly.
func TestLaunchEnvRefusesAValueTheExecCannotCarry(t *testing.T) {
	_, err := LaunchEnv(launchFixture(config.ProxyProfile{HTTPProxy: "http://proxy.example:8080\x00evil"}))
	if !errors.Is(err, ErrUnrepresentable) {
		t.Fatalf("err = %v, want ErrUnrepresentable", err)
	}
}

func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
