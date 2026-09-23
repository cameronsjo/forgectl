package proxy

import (
	"github.com/cameronsjo/forgectl/internal/config"
)

// LaunchEnv returns the environment a launched harness needs to reach the
// network through pc.LaunchProfile: the variables to set, and the variables to
// REMOVE from the inherited environment. Both are empty when no launch profile
// is configured. It is the second sanctioned sink for profile values, alongside
// Use's shell protocol: both walk profileVariables, so neither can support a
// variable or a spelling the other silently misses.
//
// An absent profile field removes both spellings rather than setting them
// empty, which is the same thing Use does and the invariant ProxyProfile
// documents. That buys two things and not a third:
//
//   - Determinism. The merge overrides key by key, so an untouched key would
//     let a stale exported value survive into the harness.
//   - Agreement between the two sinks, for a consumer that tests PRESENCE
//     rather than truthiness. `proxy use` unsets; so does this.
//
// It does NOT protect loopback, and an earlier version of this comment claimed
// it did. Measured 2026-09-11 with curl 8.7.1: absent and empty no_proxy are
// equivalent, and neither bypasses loopback. Removing the shell's
// NO_PROXY=localhost sends the harness's loopback requests to the proxy just
// as emptying it would. config.ErrLaunchProfileNoBypass is what actually
// closes that, by refusing a profile that routes traffic and names no bypass
// list — so by the time this function runs, NoProxy is always non-empty.
func LaunchEnv(pc config.ProxyConfig) (set map[string]string, unset []string, err error) {
	profile, ok, err := pc.ResolveLaunchProfile()
	if err != nil || !ok {
		return nil, nil, err
	}

	variables := profileVariables(profile)
	set = make(map[string]string, len(variables)*2)
	unset = make([]string, 0, len(variables)*2)
	for _, v := range variables {
		if v.value == "" {
			unset = append(unset, v.upper, v.lower)
			continue
		}
		// A NUL cannot survive the exec boundary any more than it can survive a
		// shell variable, so it is rejected here rather than becoming a silently
		// truncated value or an opaque EINVAL from the exec itself.
		if err := rejectNUL(v.value); err != nil {
			return nil, nil, err
		}
		set[v.upper] = v.value
		set[v.lower] = v.value
	}
	return set, unset, nil
}
