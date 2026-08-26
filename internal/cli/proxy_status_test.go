package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	proxypkg "github.com/cameronsjo/forgectl/internal/proxy"
)

// fixtureLookup reads a table instead of the process environment. Seeding the
// real environment with proxy variables would also seed net/http's
// process-wide proxy cache, which resolves once per process and would route
// every later test's requests through a host that does not exist.
func fixtureLookup(env map[string]string) proxypkg.Lookup {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

// bothSpellings is what a shell that evaluated `proxy use` would carry.
func bothSpellings(lower, upper, value string) map[string]string {
	return map[string]string{lower: value, upper: value}
}

// runProxyCmd executes one already-constructed subcommand and returns
// everything a value could escape through: stdout, and stdout plus stderr plus
// the default slog handler's output plus the returned error.
func runProxyCmd(t *testing.T, cmd *cobra.Command, args ...string) (stdout, combined string, err error) {
	t.Helper()

	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())

	combined = out.String() + errOut.String() + logs.String()
	if err != nil {
		combined += err.Error()
	}
	return out.String(), combined, err
}

func runProxyStatus(t *testing.T, deps module.Deps, env map[string]string) (stdout, combined string, err error) {
	t.Helper()
	return runProxyCmd(t, newProxyStatusCmd(deps, fixtureLookup(env)))
}

func TestProxyListPrintsSortedNamesOnly(t *testing.T) {
	const configuredValue = "opaque-config-value-must-not-appear"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work":  {HTTPProxy: configuredValue},
		"alpha": {HTTPProxy: configuredValue + "-alpha"},
	}}}}

	stdout, combined, err := runProxyCmd(t, newProxyCmd(deps), "list")
	if err != nil {
		t.Fatalf("proxy list: %v", err)
	}
	if want := "alpha\nwork\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if strings.Contains(combined, configuredValue) {
		t.Fatalf("list leaked a profile value: %q", combined)
	}
}

// TestProxyListWithoutConfigurationIsNotAnError keeps `list` usable as the
// discovery verb it exists to be: an absent [proxy] section is a state, not a
// failure, and stdout stays a clean name list for a caller piping it.
func TestProxyListWithoutConfigurationIsNotAnError(t *testing.T) {
	stdout, combined, err := runProxyCmd(t, newProxyCmd(module.Deps{}), "list")
	if err != nil {
		t.Fatalf("proxy list with no [proxy] section: %v", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no names on stdout", stdout)
	}
	if !strings.Contains(combined, noProfilesMessage) {
		t.Fatalf("no informational line was written: %q", combined)
	}
}

// TestProxyListEscapesHostileProfileNames covers the one caller-influenced
// string these verbs print. Profile names are config.toml keys, which a
// couriered or shared config authors.
func TestProxyListEscapesHostileProfileNames(t *testing.T) {
	const hostile = "work\x1b[2J\r\nfake-profile"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		hostile: {HTTPProxy: "http://work.example:8080"},
	}}}}

	stdout, _, err := runProxyCmd(t, newProxyCmd(deps), "list")
	if err != nil {
		t.Fatalf("proxy list: %v", err)
	}
	if strings.Contains(stdout, "\x1b") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("a hostile profile name reached the terminal intact: %q", stdout)
	}
}

// TestProxyStatusNamesTheMatchedProfileWithoutItsValues is the leak test for
// the match case, where the sentinel is live in BOTH directions at once: it is
// the configured value and the value the environment carries. Neither reading
// may reach any output surface.
func TestProxyStatusNamesTheMatchedProfileWithoutItsValues(t *testing.T) {
	const liveValue = "http://opaque-live-value.example:8080"
	const unmatchedValue = "http://opaque-config-only-value.example:3128"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: liveValue},
		"home": {HTTPProxy: unmatchedValue},
	}}}}

	stdout, combined, err := runProxyStatus(t, deps, bothSpellings("http_proxy", "HTTP_PROXY", liveValue))
	if err != nil {
		t.Fatalf("proxy status: %v", err)
	}
	want := "profile: work\nhttp_proxy: set\nhttps_proxy: unset\nall_proxy: unset\nno_proxy: unset\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	for _, sentinel := range []string{liveValue, unmatchedValue} {
		if strings.Contains(combined, sentinel) {
			t.Fatalf("status leaked %q: %s", sentinel, combined)
		}
	}
}

// TestProxyStatusMismatchIsCategoryOnly seeds a sentinel that exists only in
// the environment and one that exists only in the configuration. The verdict
// must name neither, and must not hint at the shape of the mismatch: which
// variable diverged is itself a fact about a configured value.
func TestProxyStatusMismatchIsCategoryOnly(t *testing.T) {
	const environmentOnly = "http://opaque-environment-only.example:8080"
	const configurationOnly = "http://opaque-configuration-only.example:3128"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: configurationOnly},
	}}}}

	stdout, combined, err := runProxyStatus(t, deps, bothSpellings("http_proxy", "HTTP_PROXY", environmentOnly))
	if err != nil {
		t.Fatalf("proxy status: %v", err)
	}
	if stdout != noMatchMessage+"\n" {
		t.Fatalf("stdout = %q, want exactly the no-match line", stdout)
	}
	for _, sentinel := range []string{environmentOnly, configurationOnly} {
		if strings.Contains(combined, sentinel) {
			t.Fatalf("status leaked %q: %s", sentinel, combined)
		}
	}
}

// TestProxyStatusWithoutConfigurationIsCategoryOnly is the unset-config case:
// a live environment and no [proxy] section at all.
func TestProxyStatusWithoutConfigurationIsCategoryOnly(t *testing.T) {
	const environmentOnly = "http://opaque-unconfigured-environment.example:8080"

	stdout, combined, err := runProxyStatus(t, module.Deps{},
		bothSpellings("http_proxy", "HTTP_PROXY", environmentOnly))
	if err != nil {
		t.Fatalf("proxy status with no [proxy] section: %v", err)
	}
	if stdout != noMatchMessage+"\n" {
		t.Fatalf("stdout = %q, want exactly the no-match line", stdout)
	}
	if strings.Contains(combined, environmentOnly) {
		t.Fatalf("status leaked the live environment value: %s", combined)
	}
}

// TestProxyStatusRejectsHalfAppliedEnvironment is the case the verb was filed
// for: a shell carrying part of a profile is not carrying that profile, and
// that state was previously invisible.
func TestProxyStatusRejectsHalfAppliedEnvironment(t *testing.T) {
	const liveValue = "http://opaque-half-applied.example:8080"
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: liveValue, HTTPSProxy: "https://opaque-half-applied.example:8443"},
	}}}}

	stdout, combined, err := runProxyStatus(t, deps, bothSpellings("http_proxy", "HTTP_PROXY", liveValue))
	if err != nil {
		t.Fatalf("proxy status: %v", err)
	}
	if stdout != noMatchMessage+"\n" {
		t.Fatalf("stdout = %q, want exactly the no-match line", stdout)
	}
	if strings.Contains(combined, liveValue) {
		t.Fatalf("status leaked the half-applied value: %s", combined)
	}
}

// TestProxyStatusReadsTheProcessEnvironment is the wiring control for the
// injected lookup: without it, every test above would pass against a status
// verb that reads nothing. It uses ALL_PROXY alone, the one supported variable
// net/http does not consult, so seeding the real environment cannot disturb
// another test's HTTP client.
func TestProxyStatusReadsTheProcessEnvironment(t *testing.T) {
	const liveValue = "socks5://opaque-live-all-proxy.example:1080"
	t.Setenv("all_proxy", liveValue)
	t.Setenv("ALL_PROXY", liveValue)
	for _, name := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "no_proxy", "NO_PROXY"} {
		t.Setenv(name, "")
	}
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"socks": {AllProxy: liveValue},
	}}}}

	stdout, combined, err := runProxyCmd(t, newProxyStatusCmd(deps, os.LookupEnv))
	if err != nil {
		t.Fatalf("proxy status: %v", err)
	}
	if !strings.Contains(stdout, "profile: socks\n") || !strings.Contains(stdout, "all_proxy: set\n") {
		t.Fatalf("status did not read the process environment: %q", stdout)
	}
	if strings.Contains(combined, liveValue) {
		t.Fatalf("status leaked the live environment value: %s", combined)
	}
}

func TestProxyReadVerbsRejectArguments(t *testing.T) {
	deps := module.Deps{Cfg: config.Config{Proxy: config.ProxyConfig{Profiles: map[string]config.ProxyProfile{
		"work": {HTTPProxy: "http://work.example:8080"},
	}}}}
	for _, args := range [][]string{{"list", "work"}, {"status", "work"}} {
		if _, _, err := runProxyCmd(t, newProxyCmd(deps), args...); err == nil {
			t.Errorf("args %q unexpectedly succeeded", args)
		}
	}
}
