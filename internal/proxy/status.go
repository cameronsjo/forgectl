package proxy

import (
	"sort"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Lookup resolves one environment variable, mirroring os.LookupEnv. Passing
// the lookup in keeps every comparison here in memory: nothing in this file
// reads a process environment, renders a value, or returns one.
type Lookup func(name string) (string, bool)

// Variable is one supported proxy variable's presence in an environment. It
// deliberately carries no value — presence is the whole reportable shape.
type Variable struct {
	// Name is the lowercase spelling, which is also the display name.
	Name string
	// Set reports that at least one spelling carries a non-empty value.
	Set bool
}

// Names returns the configured profile names in sorted order. Names are
// user-authored config keys, not values, and are the one part of a profile
// safe to display.
func Names(profiles map[string]config.ProxyProfile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Environment reports set/unset for every supported proxy variable, in the
// same fixed order Use writes them. A variable present but empty counts as
// unset: an empty assignment routes no traffic, so calling it "set" would
// misreport the shape it exists to describe.
func Environment(lookup Lookup) []Variable {
	variables := profileVariables(config.ProxyProfile{})
	states := make([]Variable, 0, len(variables))
	for _, v := range variables {
		states = append(states, Variable{
			Name: v.lower,
			Set:  environmentValue(lookup, v.lower) != "" || environmentValue(lookup, v.upper) != "",
		})
	}
	return states
}

// Match returns the name of the configured profile the environment currently
// carries. A profile matches only when every supported variable holds its
// configured value in BOTH spellings — so a half-applied environment matches
// nothing, which is the state this verb exists to make visible.
//
// A profile with no values never matches: it is indistinguishable from off,
// the same reason Use refuses it. When two profiles are byte-identical the
// first in sorted order wins, so the answer is stable across runs.
func Match(profiles map[string]config.ProxyProfile, lookup Lookup) (string, bool) {
	for _, name := range Names(profiles) {
		profile := profiles[name]
		if profile.IsZero() {
			continue
		}
		if profileMatchesEnvironment(profile, lookup) {
			return name, true
		}
	}
	return "", false
}

// profileMatchesEnvironment compares in memory only; neither the configured
// value nor the environment's is returned or rendered on any outcome.
func profileMatchesEnvironment(profile config.ProxyProfile, lookup Lookup) bool {
	for _, v := range profileVariables(profile) {
		if environmentValue(lookup, v.lower) != v.value {
			return false
		}
		if environmentValue(lookup, v.upper) != v.value {
			return false
		}
	}
	return true
}

// environmentValue collapses absent and empty into one state, matching the
// config side where an omitted profile value means "unset both spellings".
func environmentValue(lookup Lookup, name string) string {
	value, ok := lookup(name)
	if !ok {
		return ""
	}
	return value
}
