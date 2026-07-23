package preflight

import (
	"encoding/json"
	"sort"
	"strings"
)

// ChangeSet is the diff between a project's current effective enabledPlugins
// and Cut A's computed target: the plugins to enable and the plugins to
// disable.
type ChangeSet struct {
	Enable  []string
	Disable []string
}

// Aligned reports whether current already matches target — no changes
// needed.
func (c ChangeSet) Aligned() bool {
	return len(c.Enable) == 0 && len(c.Disable) == 0
}

// Diff computes the enable/disable change-set turning current into target.
// Pure and table-testable: no I/O, no catalog or settings knowledge — just
// two maps. Enable lists every key target wants true that current doesn't
// already have true; Disable lists every key current has true that target
// doesn't want true (target omits it, or target has it false) — applying
// the change-set makes current's true-valued keys exactly match target's.
func Diff(current, target map[string]bool) ChangeSet {
	var enable, disable []string
	for key, want := range target {
		if want && !current[key] {
			enable = append(enable, key)
		}
	}
	for key, have := range current {
		if have && !target[key] {
			disable = append(disable, key)
		}
	}
	sort.Strings(enable)
	sort.Strings(disable)
	return ChangeSet{Enable: enable, Disable: disable}
}

// Target computes Cut A's deterministic alignment target: every catalog
// core-tier plugin, folded with the project's own committed settings.json
// enabledPlugins entries (locked design decision 2 — "the repo baseline
// survives by inclusion"). A committed entry, true or false, is a
// deliberate per-repo choice and wins over the catalog default for that
// key. Plugins that are neither catalog-core nor named by the committed
// file are left out of the target entirely — Cut A does not attempt
// project-applicability judgment for extension-tier or non-catalog plugins
// (deferred to cadence:catalog, per the issue's Cut A scope).
func Target(catalogCore, committedProject map[string]bool) map[string]bool {
	target := make(map[string]bool, len(catalogCore)+len(committedProject))
	for k, v := range catalogCore {
		target[k] = v
	}
	for k, v := range committedProject {
		target[k] = v
	}
	return target
}

// FilterMarketplaces computes the extraKnownMarketplaces entries --apply may
// safely write: for every plugin key in target with want=true, its
// marketplace name (everything after "@") is looked up in trusted — the
// caller's user/local-scope marketplace registry (see TrustedMarketplaces),
// the ONLY legitimate provenance for a marketplace SOURCE (a github repo, a
// local directory, …). Target's fold-in can pull in a plugin@marketplace
// from a project's own committed .claude/settings.json (locked decision 2 —
// "the repo baseline survives by inclusion"), but that fold-in must never be
// able to inject the marketplace SOURCE that plugin resolves against: a
// plugin can't load without a known marketplace, so smuggling in a
// trusted-looking marketplace definition via a cloned repo's committed
// settings.json is the actual attack surface, not the plugin name itself.
//
// A target plugin whose marketplace has no trusted registration is returned
// in unregistered (sorted) instead of marketplaces — its enabledPlugins
// entry still rides Target's fold-in, but preflight writes nothing for its
// marketplace, so it stays invisible to nobody: the CLI surfaces
// unregistered explicitly, and Claude Code's own install/marketplace-trust
// gate keeps the plugin from loading until a human registers that
// marketplace themselves.
func FilterMarketplaces(target map[string]bool, trusted map[string]json.RawMessage) (marketplaces map[string]json.RawMessage, unregistered []string) {
	marketplaces = map[string]json.RawMessage{}
	for key, want := range target {
		if !want {
			continue
		}
		_, name, ok := strings.Cut(key, "@")
		if !ok || name == "" {
			continue
		}
		if src, known := trusted[name]; known {
			marketplaces[name] = src
		} else {
			unregistered = append(unregistered, key)
		}
	}
	sort.Strings(unregistered)
	return marketplaces, unregistered
}
