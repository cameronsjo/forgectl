// Package module defines the manifest contract for forgectl's compile-time
// modules (ADR-0005). A module is data plus constructors: one Manifest per
// domain, aggregated in internal/cli's explicit allModules() slice. The domain
// package beneath a module stays a plain library — the manifest wires only the
// CLI surface. Only internal/cli (and tests) import this package.
package module

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/step"
)

// Tier sorts modules into the load-bearing core and the individually
// evictable extensions. The registry test pins the total count and the exact
// core set — a new module or a tier change is a deliberate test edit in the
// same diff (ADR-0005's tier policy).
type Tier int

const (
	// TierCore marks load-bearing daily verbs.
	TierCore Tier = iota
	// TierExtension marks conveniences; individually evictable.
	TierExtension
)

// Deps carries what a module constructor needs: the loaded config and the
// process runner (exec.OSRunner in Execute; exec.FakeRunner in tests).
type Deps struct {
	Cfg    config.Config
	Runner exec.Runner
}

// Manifest declares one module: its canonical verb, tier, config-section
// claim, alias surfaces, and constructors. There is deliberately no Summary
// field — cobra's Short on the constructed command is the one-line
// description (read it via New(deps).Short); a second surface would drift.
type Manifest struct {
	// Name is the canonical top-level verb: "tmux", "pr", "y".
	Name string
	// Tier is the module's growth-policy slot (core vs extension).
	Tier Tier
	// ConfigKey is the config.toml section this module owns ("net",
	// "launch"); "" claims none. The registry test enforces that every
	// struct-kind Config section is claimed by exactly one module.
	ConfigKey string
	// GroupAliases are cobra Aliases appended to the parent command
	// (launch→"cl", workflow→"flow").
	GroupAliases []string
	// ArgvTokens are pre-cobra argv spellings normalizeArgs converges on
	// Name (tmux→"tm"). Only tmux declares any today — forgiveness-for-all
	// is a flagged follow-on (ADR-0005).
	ArgvTokens []string
	// SubAliases maps each canonical subverb to its aliases — the shared
	// applyAliases helper's input (absorbs forgive's per-module maps).
	SubAliases map[string][]string
	// New constructs the module's parent command.
	New func(Deps) *cobra.Command
	// Steps returns the workflow step verbs this module contributes to the
	// data plane; nil for most modules.
	Steps func(Deps) step.Registry
}
