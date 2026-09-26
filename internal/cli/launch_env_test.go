package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// proxyLaunchCfg is a config whose launch profile resolves. It carries a
// no_proxy because config.ErrLaunchProfileNoBypass refuses one without it.
func proxyLaunchCfg() config.Config {
	return config.Config{Proxy: config.ProxyConfig{
		Profiles: map[string]config.ProxyProfile{"work": {
			HTTPSProxy: "https://proxy.example:8443",
			NoProxy:    "localhost,127.0.0.1",
		}},
		LaunchProfile: "work",
	}}
}

// TestInjectedWindowEnv_EmitsRemovalsAsEmpty pins the one place the tmux sink
// deliberately differs from the exec paths. tmux new-window can set a variable
// and cannot unset one, so a variable the composer wants gone is emitted empty
// — which still overrides whatever the tmux server has been holding since it
// started, and which behaves the same for the HTTP clients this exists for.
func TestInjectedWindowEnv_EmitsRemovalsAsEmpty(t *testing.T) {
	entries, err := injectedWindowEnv(proxyLaunchCfg())
	if err != nil {
		t.Fatalf("injectedWindowEnv: %v", err)
	}

	// The set half. Without this the removal assertions below could pass on a
	// composer that produced nothing at all.
	if !slices.Contains(entries, "HTTPS_PROXY=https://proxy.example:8443") {
		t.Fatalf("entries carry no configured proxy, so this test cannot detect a missing removal: %v", entries)
	}
	for _, want := range []string{"HTTP_PROXY=", "http_proxy=", "ALL_PROXY=", "all_proxy="} {
		if !slices.Contains(entries, want) {
			t.Errorf("entries are missing %q; a stale tmux-server value would survive into the review", want)
		}
	}
	// Every entry must be a well-formed assignment, because the tmux boundary
	// refuses anything else and a refusal here would kill the review.
	for _, e := range entries {
		if key, _, found := strings.Cut(e, "="); !found || key == "" {
			t.Errorf("entry %q is not a KEY=VALUE assignment", e)
		}
	}
}

func TestInjectedWindowEnv_IsEmptyWithoutAnyInjection(t *testing.T) {
	entries, err := injectedWindowEnv(config.Config{})
	if err != nil {
		t.Fatalf("injectedWindowEnv: %v", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want nil so the window keeps inheriting the tmux server env", entries)
	}
}

// TestInjectedWindowEnv_CarriesTheRefusal is why the pr client resolves this at
// dispatch rather than at construction: the refusal has to reach the verb that
// starts a harness.
func TestInjectedWindowEnv_CarriesTheRefusal(t *testing.T) {
	cfg := proxyLaunchCfg()
	cfg.Proxy.LaunchProfile = "wrok"
	if _, err := injectedWindowEnv(cfg); !errors.Is(err, config.ErrUnknownLaunchProfile) {
		t.Fatalf("err = %v, want config.ErrUnknownLaunchProfile", err)
	}
}

// TestInjectedWindowEnv_RefusesCredentialsOnArgv pins the argv sink's own
// check. Window entries reach tmux as `-e KEY=VAL` arguments, readable by any
// local user through ps. The launch-profile refusal covers the proxy values;
// this covers every other injected URL, such as a telemetry endpoint.
func TestInjectedWindowEnv_RefusesCredentialsOnArgv(t *testing.T) {
	cfg := config.Config{Bench: config.BenchConfig{ //nolint:gosec // G101: a fake credential the refusal must catch
		Telemetry:    true,
		OTLPEndpoint: "http://u:secretpw@collector.example:4317",
	}}
	_, err := injectedWindowEnv(cfg)
	if !errors.Is(err, errWindowEnvCredentials) {
		t.Fatalf("err = %v, want errWindowEnvCredentials", err)
	}
	if strings.Contains(err.Error(), "secretpw") {
		t.Errorf("message leaked the credential: %q", err)
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Errorf("message = %q, want it to name the variable", err)
	}

	// Negative control: the same config with no userinfo passes, and so does an
	// '@' in a value that is not a URL.
	cfg.Bench.OTLPEndpoint = "http://collector.example:4317"
	if _, err := injectedWindowEnv(cfg); err != nil {
		t.Fatalf("injectedWindowEnv without userinfo: %v", err)
	}
	withAt := proxyLaunchCfg()
	p := withAt.Proxy.Profiles["work"]
	p.NoProxy = "localhost,a@b.example"
	withAt.Proxy.Profiles["work"] = p
	if _, err := injectedWindowEnv(withAt); err != nil {
		t.Fatalf("injectedWindowEnv with an '@' in no_proxy: %v", err)
	}
}

// TestInjectedLaunchKeys_NamesBothDirectionsWithoutValues pins what `launch
// which` renders. Before this existed no verb could answer "will my proxy be
// applied?", because the injected block is deliberately absent from the launch
// profile — which is what keeps its values off every display surface. Naming
// the keys answers the question and leaks nothing.
func TestInjectedLaunchKeys_NamesBothDirectionsWithoutValues(t *testing.T) {
	keys, err := injectedLaunchKeys(proxyLaunchCfg())
	if err != nil {
		t.Fatalf("injectedLaunchKeys: %v", err)
	}

	if !slices.Contains(keys, "HTTPS_PROXY") {
		t.Errorf("keys = %v, want the set variable named", keys)
	}
	// A removal changes the launch as much as a set, so it is part of the
	// answer; the `-` prefix is what distinguishes the two.
	if !slices.Contains(keys, "-ALL_PROXY") {
		t.Errorf("keys = %v, want a removed variable named with a - prefix", keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "proxy.example") || strings.Contains(k, "=") {
			t.Errorf("key %q carries a value; this surface is names only", k)
		}
	}
}

func TestInjectedLaunchKeys_CarriesTheRefusal(t *testing.T) {
	cfg := proxyLaunchCfg()
	cfg.Proxy.LaunchProfile = "wrok"
	if _, err := injectedLaunchKeys(cfg); !errors.Is(err, config.ErrUnknownLaunchProfile) {
		t.Fatalf("err = %v, want config.ErrUnknownLaunchProfile", err)
	}
}

// TestInjectedWindowEnv_RefusesSlashVariantCredentials pins the forms Go's
// url.Parse reads as userless but curl and Node read as user:pass@host. The
// OTLP endpoint reaches argv only through injectedWindowEnv, so this is its
// only refusal.
func TestInjectedWindowEnv_RefusesSlashVariantCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"http:/u:secretpw@collector.example:4317",   //nolint:gosec // G101: a fake credential the refusal must catch
		"http:///u:secretpw@collector.example:4317", //nolint:gosec // G101: a fake credential the refusal must catch
	} {
		cfg := config.Config{Bench: config.BenchConfig{Telemetry: true, OTLPEndpoint: endpoint}}
		_, err := injectedWindowEnv(cfg)
		if !errors.Is(err, errWindowEnvCredentials) {
			t.Fatalf("injectedWindowEnv(%q) err = %v, want errWindowEnvCredentials", endpoint, err)
		}
	}
}

// TestInjectedWindowEnv_RefusesQueryStringOnArgv pins #529's second half: a
// URL's query string is where an ingest key usually rides
// (https://ingest.example/v1?api_key=…), and the userinfo check never looked
// there. The message names the variable, never the value.
func TestInjectedWindowEnv_RefusesQueryStringOnArgv(t *testing.T) {
	cfg := config.Config{Bench: config.BenchConfig{
		Telemetry:    true,
		OTLPEndpoint: "https://ingest.example/v1?api_key=secretkey123", //nolint:gosec // G101: a fake key the refusal must catch
	}}
	_, err := injectedWindowEnv(cfg)
	if !errors.Is(err, errWindowEnvQuery) {
		t.Fatalf("err = %v, want errWindowEnvQuery", err)
	}
	if strings.Contains(err.Error(), "secretkey123") {
		t.Errorf("message leaked the key: %q", err)
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Errorf("message = %q, want it to name the variable", err)
	}
}

// TestInjectedWindowEnv_RefusesSchemelessQuery pins a review follow-up: an
// endpoint without a scheme ("host:4318/v1?api_key=…") skipped the query
// check because only scheme'd values were read as URLs. The refusal also
// names the config setting the value came from, not just the env var.
func TestInjectedWindowEnv_RefusesSchemelessQuery(t *testing.T) {
	cfg := config.Config{Bench: config.BenchConfig{
		Telemetry:    true,
		OTLPEndpoint: "ingest.example:4318/v1?api_key=secretkey123", //nolint:gosec // G101: a fake key the refusal must catch
	}}
	_, err := injectedWindowEnv(cfg)
	if !errors.Is(err, errWindowEnvQuery) {
		t.Fatalf("err = %v, want errWindowEnvQuery", err)
	}
	if strings.Contains(err.Error(), "secretkey123") {
		t.Errorf("message leaked the key: %q", err)
	}
	if !strings.Contains(err.Error(), "[bench].otlp_endpoint") {
		t.Errorf("message = %q, want it to name the config setting", err)
	}
}

func TestEndpointForDisplay_HidesQuery(t *testing.T) {
	if got := endpointForDisplay("https://ingest.example/v1?api_key=k"); got != "https://ingest.example/v1?[query hidden]" {
		t.Errorf("got %q", got)
	}
	if got := endpointForDisplay("http://localhost:16317"); got != "http://localhost:16317" {
		t.Errorf("got %q", got)
	}
}

// TestInjectedWindowEnv_RefusesSchemelessCredentials pins a security-review
// follow-up: an endpoint with userinfo but no scheme skipped the credentials
// check, which was gated on a scheme. Only the no_proxy list may hold an '@'.
func TestInjectedWindowEnv_RefusesSchemelessCredentials(t *testing.T) {
	cfg := config.Config{Bench: config.BenchConfig{
		Telemetry:    true,
		OTLPEndpoint: "user:s3cr3t@collector:4318", //nolint:gosec // G101: a fake credential the refusal must catch
	}}
	_, err := injectedWindowEnv(cfg)
	if !errors.Is(err, errWindowEnvCredentials) {
		t.Fatalf("err = %v, want errWindowEnvCredentials", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("message leaked the credential: %q", err)
	}
}

func TestEndpointForDisplay_HidesUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"http://u:s3cr3t@collector:4318": "http://[userinfo hidden]@collector:4318", //nolint:gosec // G101: a fake credential the display must hide
		"u:s3cr3t@collector:4318":        "[userinfo hidden]@collector:4318",        //nolint:gosec // G101: a fake credential the display must hide
	} {
		if got := endpointForDisplay(in); got != want {
			t.Errorf("endpointForDisplay(%q) = %q, want %q", in, got, want)
		}
	}
}
