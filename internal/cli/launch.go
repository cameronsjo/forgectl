package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/step"
)

// launchAliases maps each canonical launch subcommand to its accepted
// aliases — migrated here from forgive.LaunchAliases at conversion. The `cl`
// shorthand for the group itself is a GroupAlias on the manifest (and a
// Cobra alias in newLaunchCmd's literal), not listed here. Separate var for
// the same initialization-cycle reason as yAliases.
var launchAliases = map[string][]string{
	"which": {"config"},
}

// launchModule declares the Claude Code launcher core module (ADR-0005):
// owns the [launch] config section. The pre-Cobra launchIntercept in
// execute.go stays host-owned and hardcoded — it is dispatch-pipeline
// plumbing, not module surface (ADR-0005 §Future work). The launch step
// stub's contribution arrives with the step-plane inversion.
var launchModule = module.Manifest{
	Name:         "launch",
	Tier:         module.TierCore,
	ConfigKey:    "launch",
	GroupAliases: []string{"cl"},
	SubAliases:   launchAliases,
	New:          newLaunchCmd,
	Steps: func(module.Deps) step.Registry {
		return launch.Steps()
	},
}

// ownLaunchVerbs are the canonical `forgectl launch <verb>` tokens handled by
// the Cobra launch subtree (styled help/usage) rather than passed through to
// claude; subcommand aliases are resolved via isOwnLaunchVerb.
// version/completion are intentionally absent — forgectl owns those at the root.
var ownLaunchVerbs = map[string]bool{
	"which": true, "edit": true, "init": true, "doctor": true,
	"help": true, "--help": true, "-h": true,
}

// isOwnLaunchVerb reports whether tok routes to the Cobra launch subtree — a
// canonical own-verb, or any subcommand alias registered in launchAliases
// (the single source of truth, so a new alias there is recognized here
// without a matching edit).
func isOwnLaunchVerb(tok string) bool {
	if ownLaunchVerbs[tok] {
		return true
	}
	for _, aliases := range launchAliases {
		for _, a := range aliases {
			if a == tok {
				return true
			}
		}
	}
	return false
}

// newLaunchCmd builds the `launch` parent command (alias `cl`). Own-verbs are
// attached as subcommands for styled help; the bare/builder/agents passthrough
// is intercepted in Execute before Cobra ever parses, so
// `forgectl launch --model sonnet -p hi` stays byte-clean.
func newLaunchCmd(deps module.Deps) *cobra.Command {
	cfg := deps.Cfg
	cmd := &cobra.Command{
		Use:     "launch [claude args…]",
		Aliases: []string{"cl"},
		Short:   "Per-project launcher for Claude Code",
		Long: `launch resolves a per-project profile from your working directory,
runs a short guided launch, then execs claude.

  forgectl launch                 interactive launcher (Model, New/Resume/Fork)
  forgectl launch <claude args…>  apply the profile and pass your args through
  forgectl launch agents …        inject the agents posture; passthrough on --json

Run "forgectl launch which" to see the profile resolved for the current
directory. Profiles live in the [launch] section of config.toml — scaffold one
with "forgectl launch init".`,
		// Bare `forgectl launch` is handled by the Execute intercept (interview),
		// so this RunE only fires if Cobra reaches it directly; keep it correct.
		RunE: func(_ *cobra.Command, args []string) error {
			return launchExec(cfg, args)
		},
	}
	cmd.AddCommand(
		newLaunchWhichCmd(cfg),
		newLaunchEditCmd(),
		newLaunchInitCmd(),
		newLaunchDoctorCmd(cfg),
	)
	applyAliases(cmd, launchAliases)
	return cmd
}

// runLaunch dispatches a `forgectl launch …` invocation. Own-verbs return
// handled=false and are left for the normal fang path (styled help); everything
// else (bare, builder, agents) execs claude directly.
func runLaunch(cfg config.Config, rest []string) (handled bool, err error) {
	if len(rest) > 0 && isOwnLaunchVerb(rest[0]) {
		return false, nil // own-verb → fang dispatches the launch subtree
	}
	return true, launchExec(cfg, rest)
}

// launchExec is the resolve → (interview) → exec path: it reduces the launch
// config against the cwd, resolves the claude binary, assembles the posture for
// the requested mode, merges env, and execs claude in place. On success it does
// not return (syscall.Exec replaces the process).
func launchExec(cfg config.Config, args []string) error {
	lc, _ := resolveLaunchConfig(cfg)

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine working directory: %w", err)
	}
	profile := launch.Resolve(lc, cwd)

	claudePath, err := launch.ClaudePath(lc.Defaults)
	if err != nil {
		return err
	}

	var claudeArgs []string
	switch {
	case len(args) == 0:
		choice := launch.Choice{Model: profile.Model, Mode: launch.New}
		if launch.IsInteractiveTTY() {
			if choice, err = launch.Interview(profile); err != nil {
				return err
			}
		}
		claudeArgs = launch.SessionArgs(profile, choice.Model, choice.Mode)
	case args[0] == "agents":
		if launch.IsAgentsPassthrough(args) {
			claudeArgs = args // byte-clean: no injection, no banner
		} else {
			claudeArgs = launch.AgentsArgs(profile, args)
			launch.Banner(os.Stderr, claudeArgs)
		}
	default:
		claudeArgs = launch.BuilderArgs(profile, args)
	}

	// Layer the profile env over the opt-in bench telemetry block (profile wins),
	// then merge that over the process env. When telemetry is off, TelemetryEnv is
	// nil and this reduces to the profile env alone.
	extra := launch.MergeMaps(bench.TelemetryEnv(cfg), profile.Env)
	env := launch.MergeEnv(os.Environ(), extra)
	slog.Debug("Preparing to exec claude.", "path", claudePath, "argc", len(claudeArgs), "match", profile.Match)
	return launch.Exec(claudePath, claudeArgs, env)
}

// resolveLaunchConfig returns the [launch] section from config.toml plus a
// human source label. When that section is absent it falls back to a legacy
// ~/.config/claunch/claunch.conf (zero-migration grace); when neither exists it
// returns the empty config and points at where `forgectl launch init` writes.
func resolveLaunchConfig(cfg config.Config) (config.LaunchConfig, string) {
	if !cfg.Launch.IsZero() {
		path, _ := config.ConfigPath()
		return cfg.Launch, path
	}
	if legacy, ok := config.LoadLegacyLaunch(); ok {
		path, _ := config.LegacyLaunchPath()
		slog.Debug("Using legacy claunch config (no [launch] section in config.toml).", "path", path)
		return legacy, path + " (legacy)"
	}
	path, _ := config.ConfigPath()
	return cfg.Launch, path + " (missing — built-in defaults)"
}
