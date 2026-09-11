package proxy

import (
	"errors"
	"maps"
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
	set, unset, err := LaunchEnv(launchFixture(config.ProxyProfile{
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
	if len(set) != len(want) {
		t.Fatalf("LaunchEnv set %d entries, want %d: %v", len(set), len(want), sortedKeys(set))
	}
	for key, value := range want {
		if set[key] != value {
			t.Errorf("set[%q] = %q, want %q", key, set[key], value)
		}
	}
	// A fully-populated profile leaves nothing to remove, so a caller can tell
	// "no removals needed" from "removals not computed" only if this is empty.
	if len(unset) != 0 {
		t.Errorf("LaunchEnv unset %v, want nothing for a fully-populated profile", unset)
	}
}

// TestLaunchEnvRemovesAbsentFieldsRatherThanEmptyingThem pins the semantic the
// rest of the codebase already documents: ProxyProfile says an omitted value
// UNSETS both spellings, and Use implements that with `unset`. Emptying instead
// would diverge for any consumer testing presence rather than truthiness — and
// for NO_PROXY it inverts the meaning, since an empty NO_PROXY says "no bypass
// exceptions" and would route loopback traffic through the proxy.
func TestLaunchEnvRemovesAbsentFieldsRatherThanEmptyingThem(t *testing.T) {
	set, unset, err := LaunchEnv(launchFixture(config.ProxyProfile{HTTPSProxy: "https://proxy.example:8443"}))
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}

	wantUnset := []string{"ALL_PROXY", "HTTP_PROXY", "NO_PROXY", "all_proxy", "http_proxy", "no_proxy"}
	if got := slices.Sorted(slices.Values(unset)); !slices.Equal(got, wantUnset) {
		t.Errorf("unset = %v, want %v", got, wantUnset)
	}
	for _, key := range wantUnset {
		if value, present := set[key]; present {
			t.Errorf("set[%q] = %q; an absent field must be removed, not assigned", key, value)
		}
	}
}

func TestLaunchEnvIsAbsentWithoutALaunchProfile(t *testing.T) {
	set, unset, err := LaunchEnv(config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: "http://proxy.example:8080"},
	}})
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}
	if set != nil || unset != nil {
		t.Fatalf("LaunchEnv = (%v, %v), want both nil when no launch profile is named", set, unset)
	}
}

// TestLaunchEnvRefusesAnUnknownProfile is the rule that matters most: a typo
// must not fall back to launching unproxied, which would look like success
// while reaching the network by a path the operator did not choose.
func TestLaunchEnvRefusesAnUnknownProfile(t *testing.T) {
	set, unset, err := LaunchEnv(config.ProxyConfig{
		Profiles:      map[string]config.ProxyProfile{"work": {HTTPProxy: "http://proxy.example:8080"}},
		LaunchProfile: "wrok",
	})
	if !errors.Is(err, config.ErrUnknownLaunchProfile) {
		t.Fatalf("err = %v, want config.ErrUnknownLaunchProfile", err)
	}
	if set != nil || unset != nil {
		t.Fatalf("a refused launch still returned an environment: (%v, %v)", set, unset)
	}
	// The name is not sensitive and naming it is what makes the error
	// actionable; the values are, and none are reachable here to leak.
	if got := err.Error(); !strings.Contains(got, `"wrok"`) {
		t.Errorf("err = %q, want it to name the missing profile", got)
	}
}

// TestLaunchEnvRefusesAnEmptyProfile also pins which sentinel it refuses with:
// ErrEmptyProfile's message directs the operator to `proxy off`, which is
// advice about the shell protocol and would not fix an empty launch profile.
func TestLaunchEnvRefusesAnEmptyProfile(t *testing.T) {
	_, _, err := LaunchEnv(launchFixture(config.ProxyProfile{}))
	if !errors.Is(err, config.ErrEmptyLaunchProfile) {
		t.Fatalf("err = %v, want config.ErrEmptyLaunchProfile", err)
	}
	if errors.Is(err, ErrEmptyProfile) {
		t.Errorf("err = %q, which sends the operator to the shell protocol's `proxy off`", err)
	}
	if got := err.Error(); !strings.Contains(got, `"work"`) {
		t.Errorf("err = %q, want it to name the profile", got)
	}
}

// TestLaunchEnvRefusesAValueTheExecCannotCarry mirrors Use's NUL refusal. An
// environment entry handed to exec is a NUL-terminated C string, so the byte
// would truncate the value rather than fail loudly.
func TestLaunchEnvRefusesAValueTheExecCannotCarry(t *testing.T) {
	_, _, err := LaunchEnv(launchFixture(config.ProxyProfile{HTTPProxy: "http://proxy.example:8080\x00evil"}))
	if !errors.Is(err, ErrUnrepresentable) {
		t.Fatalf("err = %v, want ErrUnrepresentable", err)
	}
}

func sortedKeys(env map[string]string) []string {
	return slices.Sorted(maps.Keys(env))
}
