package proxy

import (
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/config"
)

// ErrUnknownLaunchProfile reports a launch_profile naming a profile the config
// does not define. Refusing beats launching unproxied: a typo that fell back to
// the calling shell's variables would leave the harness reaching the network by
// a path the operator did not choose, and the launch would look successful.
// The message names the profile because a profile NAME is not sensitive — only
// its values are.
var ErrUnknownLaunchProfile = errors.New("proxy: launch_profile names no configured profile")

// ErrEmptyLaunchProfile reports a launch_profile naming a profile that sets no
// values. Distinct from ErrEmptyProfile, whose message sends the operator to
// `proxy off` — advice about the shell protocol that would neither explain nor
// fix an empty launch profile.
var ErrEmptyLaunchProfile = errors.New("proxy: launch_profile names a profile that sets no values")

// LaunchEnv returns the environment a launched harness needs to reach the
// network through pc.LaunchProfile, or nil when no launch profile is
// configured. It is the second sanctioned sink for profile values, alongside
// Use's shell protocol: both walk profileVariables, so neither can support a
// variable or a spelling the other silently misses.
//
// Every supported variable is present in the result, and an absent profile
// field maps to the EMPTY STRING rather than being omitted. That is what makes
// a launch deterministic: the merge that applies this map overrides the calling
// shell's snapshot key by key, so an omitted key would let a stale exported
// value survive into the child — the one thing a named profile exists to
// prevent. Empty reads as no-proxy in the consumers that matter (Go's
// net/http/httpproxy, libcurl, and the undici client Node harnesses run on),
// which is the same effect Use gets by unsetting the pair.
func LaunchEnv(pc config.ProxyConfig) (map[string]string, error) {
	if pc.LaunchProfile == "" {
		return nil, nil
	}

	profile, ok := pc.Profiles[pc.LaunchProfile]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownLaunchProfile, pc.LaunchProfile)
	}
	if profile.IsZero() {
		return nil, fmt.Errorf("%w: %q", ErrEmptyLaunchProfile, pc.LaunchProfile)
	}

	variables := profileVariables(profile)
	env := make(map[string]string, len(variables)*2)
	for _, v := range variables {
		// A NUL cannot survive the exec boundary any more than it can survive a
		// shell variable, so it is rejected here rather than becoming a silently
		// truncated value or an opaque EINVAL from the exec itself.
		if err := rejectNUL(v.value); err != nil {
			return nil, err
		}
		env[v.upper] = v.value
		env[v.lower] = v.value
	}
	return env, nil
}
