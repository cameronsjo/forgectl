package cli

import (
	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/proxy"
)

// injectedLaunchEnv composes the environment forgectl adds to every harness it
// starts: bench telemetry, and the proxy profile named by [proxy]
// launch_profile. Callers layer the result UNDER a launch profile's own env, so
// a profile value still beats an injected default.
//
// One composer, three call sites — the in-place launch, the resume, and the
// surface launch. Three paths each merging their own set is how the surface
// launch came to miss telemetry entirely, and how any later injected default
// would reach one path and quietly skip the other two.
func injectedLaunchEnv(cfg config.Config) (map[string]string, error) {
	proxyEnv, err := proxy.LaunchEnv(cfg.Proxy)
	if err != nil {
		return nil, err
	}
	// Disjoint key sets today; the proxy layer is second so a future overlap
	// resolves toward the more specific network posture.
	return launch.MergeMaps(bench.TelemetryEnv(cfg), proxyEnv), nil
}
