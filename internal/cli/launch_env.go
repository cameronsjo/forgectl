package cli

import (
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
// One composer, three call sites — the in-place launch, the resume, and the
// surface launch. Three paths each merging their own set is how the surface
// launch came to miss telemetry entirely, and how any later injected default
// would reach one path and quietly skip the other two. `forgectl pr` starts a
// harness in a tmux window and is NOT covered: tmux windows inherit the tmux
// server's environment and NewWindow takes no env argument.
func injectedLaunchEnv(cfg config.Config) (set map[string]string, unset []string, err error) {
	proxySet, proxyUnset, err := proxy.LaunchEnv(cfg.Proxy)
	if err != nil {
		return nil, nil, err
	}
	// Disjoint key sets today; the proxy layer is second so a future overlap
	// resolves toward the more specific network posture.
	return launch.MergeMaps(bench.TelemetryEnv(cfg), proxySet), proxyUnset, nil
}
