package proxy

import (
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// envLookup builds a Lookup over a fixed table, so no test here touches the
// process environment.
func envLookup(env map[string]string) Lookup {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

// appliedEnv is what a shell that evaluated `proxy use` would carry: every
// configured value in both spellings, every omitted value absent.
func appliedEnv(profile config.ProxyProfile) map[string]string {
	env := map[string]string{}
	for _, v := range profileVariables(profile) {
		if v.value == "" {
			continue
		}
		env[v.lower] = v.value
		env[v.upper] = v.value
	}
	return env
}

func TestNamesAreSortedAndConfigurationFree(t *testing.T) {
	got := Names(map[string]config.ProxyProfile{
		"work": {HTTPProxy: "a"}, "home": {HTTPProxy: "b"}, "alpha": {},
	})
	if want := []string{"alpha", "home", "work"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	if got := Names(nil); len(got) != 0 {
		t.Fatalf("Names(nil) = %v, want empty", got)
	}
}

func TestMatchIdentifiesTheAppliedProfile(t *testing.T) {
	work := config.ProxyProfile{HTTPProxy: "http://work.example:8080", NoProxy: "localhost"}
	profiles := map[string]config.ProxyProfile{
		"work": work,
		"home": {HTTPProxy: "http://home.example:3128"},
	}

	name, ok := Match(profiles, envLookup(appliedEnv(work)))
	if !ok || name != "work" {
		t.Fatalf("Match = (%q, %v), want (\"work\", true)", name, ok)
	}
}

func TestMatchRejectsAHalfAppliedEnvironment(t *testing.T) {
	work := config.ProxyProfile{
		HTTPProxy:  "http://work.example:8080",
		HTTPSProxy: "https://work.example:8443",
	}
	profiles := map[string]config.ProxyProfile{"work": work}

	env := appliedEnv(work)
	delete(env, "https_proxy")
	if name, ok := Match(profiles, envLookup(env)); ok {
		t.Fatalf("a missing lowercase spelling still matched %q", name)
	}

	env = appliedEnv(work)
	delete(env, "HTTPS_PROXY")
	if name, ok := Match(profiles, envLookup(env)); ok {
		t.Fatalf("a missing uppercase spelling still matched %q", name)
	}

	env = appliedEnv(work)
	env["no_proxy"] = "localhost"
	env["NO_PROXY"] = "localhost"
	if name, ok := Match(profiles, envLookup(env)); ok {
		t.Fatalf("an environment carrying an unconfigured variable still matched %q", name)
	}
}

func TestMatchIgnoresAnEmptyProfileAndAnEmptyEnvironment(t *testing.T) {
	// An empty profile is indistinguishable from off, the same reason Use
	// refuses it — so a bare environment must not report it as active.
	profiles := map[string]config.ProxyProfile{"empty": {}}
	if name, ok := Match(profiles, envLookup(nil)); ok {
		t.Fatalf("an empty profile matched a bare environment as %q", name)
	}
	if name, ok := Match(nil, envLookup(nil)); ok {
		t.Fatalf("an unconfigured [proxy] matched as %q", name)
	}
}

func TestMatchTreatsAnEmptyAssignmentAsUnset(t *testing.T) {
	work := config.ProxyProfile{HTTPProxy: "http://work.example:8080"}
	env := appliedEnv(work)
	env["no_proxy"] = ""
	env["NO_PROXY"] = ""
	if _, ok := Match(map[string]config.ProxyProfile{"work": work}, envLookup(env)); !ok {
		t.Fatal("an empty NO_PROXY assignment was read as a configured value")
	}
}

func TestMatchIsStableAcrossIdenticalProfiles(t *testing.T) {
	shared := config.ProxyProfile{HTTPProxy: "http://shared.example:8080"}
	profiles := map[string]config.ProxyProfile{"zulu": shared, "alpha": shared}
	for range 20 {
		if name, ok := Match(profiles, envLookup(appliedEnv(shared))); !ok || name != "alpha" {
			t.Fatalf("Match = (%q, %v), want the first sorted name (\"alpha\", true)", name, ok)
		}
	}
}

func TestEnvironmentReportsPresenceInProtocolOrder(t *testing.T) {
	got := Environment(envLookup(map[string]string{
		"http_proxy": "http://work.example:8080",
		"HTTP_PROXY": "http://work.example:8080",
		"ALL_PROXY":  "socks5://work.example:1080", // uppercase spelling alone still counts
		"no_proxy":   "",                           // present but empty is unset
	}))
	want := []Variable{
		{Name: "http_proxy", Set: true},
		{Name: "https_proxy", Set: false},
		{Name: "all_proxy", Set: true},
		{Name: "no_proxy", Set: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Environment = %v, want %v", got, want)
	}
}
