package cli

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/proxy"
)

// injectedLaunchEnv composes the environment forgectl adds to every harness it
// starts: the variables to set — bench telemetry, plus the proxy profile named
// by [proxy] launch_profile — and the variables to remove from the inherited
// environment. Callers layer set UNDER a launch profile's own env, so a profile
// value still beats an injected default; removals apply to the inherited
// snapshot only, for the same reason.
//
// One composer, four call sites — the in-place launch, the resume, the surface
// launch, and (through injectedWindowEnv below) the clean-room reviewer's tmux
// window. Four paths each merging their own set is how the surface launch came
// to miss telemetry entirely, and how any later injected default would reach
// one path and quietly skip the rest.
func injectedLaunchEnv(cfg config.Config) (set map[string]string, unset []string, err error) {
	proxySet, proxyUnset, err := proxy.LaunchEnv(cfg.Proxy)
	if err != nil {
		return nil, nil, err
	}
	// Disjoint key sets today; the proxy layer is second so a future overlap
	// resolves toward the more specific network posture.
	return launch.MergeMaps(bench.TelemetryEnv(cfg), proxySet), proxyUnset, nil
}

// injectedLaunchKeys names the variables a launch will inject, sorted, with no
// values — what `launch which` renders so an operator can answer "will my proxy
// actually be applied?" without reading the source. Before this existed no verb
// answered it: the injected block is deliberately absent from the launch
// profile, which is what keeps its values off every display surface.
//
// A removal is named too, because it is equally part of the answer: a variable
// forgectl takes OUT of the inherited environment changes the launch just as
// much as one it puts in. The rendering distinguishes them with a `-` prefix.
func injectedLaunchKeys(cfg config.Config) ([]string, error) {
	set, unset, err := injectedLaunchEnv(cfg)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(set)+len(unset))
	keys = append(keys, slices.Sorted(maps.Keys(set))...)
	for _, k := range slices.Sorted(slices.Values(unset)) {
		keys = append(keys, "-"+k)
	}
	return keys, nil
}

// errWindowEnvCredentials reports an injected URL carrying user:pass@ on its
// way to a tmux window. Window entries reach tmux as `-e KEY=VAL` arguments,
// readable by any local user through ps. The message names the variable,
// never its value.
var errWindowEnvCredentials = errors.New(
	"launch environment puts credentials in a URL, which a review window would expose on the command line; " +
		"remove the user:pass@ part")

// errWindowEnvQuery reports an injected URL carrying a query string on its way
// to a tmux window. A query is where an ingest key usually rides
// (`?api_key=…`), and it would sit on the command line for as long as tmux
// runs (#529).
//
// The rule is "no query", not "no secret-looking query", because nothing here
// can tell a key from an innocent parameter, and none of the URLs this
// composer emits (a proxy, an OTLP collector) needs one. A secret in the URL
// PATH is not caught: it cannot be told from an ordinary path. The OTLP
// endpoint is treated as non-secret configuration, which is why `launch
// doctor` prints it; a collector that needs a key should take it from the
// harness's own environment or a header config, never from this URL.
var errWindowEnvQuery = errors.New(
	"launch environment puts a query string in a URL, which a review window would expose on the command line; " +
		"remove the ?… part and pass any key another way")

// injectedWindowEnv flattens the same composer into the `KEY=VALUE` entries a
// tmux review window is created with, so `forgectl pr` reaches the network by
// the posture the operator configured rather than whatever the tmux server
// inherited when it started.
//
// A variable the composer wants REMOVED is emitted EMPTY here, because tmux
// new-window can set a variable and cannot unset one. That is a real
// difference from the exec paths, and it is behaviourally equivalent for the
// clients this exists for: measured 2026-09-11 with curl 8.7.1, an absent and
// an empty no_proxy behave identically. It is NOT equivalent for a consumer
// that tests presence rather than truthiness — the residual, stated rather
// than papered over. Emitting empty still beats omitting: it overrides a stale
// value the tmux server has been holding since it started.
func injectedWindowEnv(cfg config.Config) ([]string, error) {
	set, unset, err := injectedLaunchEnv(cfg)
	if err != nil {
		return nil, err
	}
	if len(set) == 0 && len(unset) == 0 {
		return nil, nil
	}
	entries := make([]string, 0, len(set)+len(unset))
	for _, k := range slices.Sorted(maps.Keys(set)) {
		// Only a value with a scheme is read as a URL here. A scheme-less
		// proxy value was already checked by the launch-profile refusal, and
		// reading every value as a URL would misread an '@' in no_proxy.
		// HasURLScheme, not "://", so the one-slash form `http:/u:p@h` that
		// curl accepts is checked too.
		if v := set[k]; config.HasURLScheme(v) {
			if config.URLHasUserinfo(v) {
				return nil, fmt.Errorf("%w: %s", errWindowEnvCredentials, k)
			}
			if strings.Contains(v, "?") {
				return nil, fmt.Errorf("%w: %s", errWindowEnvQuery, k)
			}
		}
		entries = append(entries, k+"="+set[k])
	}
	for _, k := range slices.Sorted(slices.Values(unset)) {
		entries = append(entries, k+"=")
	}
	return entries, nil
}
