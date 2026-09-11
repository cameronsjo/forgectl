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
// documents. Empty is not a synonym for absent here: NO_PROXY's meaning is
// inverted relative to the other three — an empty NO_PROXY says "no bypass
// exceptions", so a profile that sets https_proxy and omits no_proxy would
// clobber the shell's NO_PROXY=localhost and route loopback traffic through
// the corporate proxy. Removal is what makes a profile switch deterministic
// AND leaves no half-applied variable behind.
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
