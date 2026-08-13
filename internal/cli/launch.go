package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/step"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// skipLegacyMigrateEnv disables the automatic claunch.conf migration
// (fallback + shadow scenarios below) and restores the original warn-only
// behavior (legacyShadowWarning). Not a CLI flag — an operator who wants to
// keep hand-managing the legacy file sets this once in their shell profile.
const skipLegacyMigrateEnv = "FORGECTL_SKIP_LEGACY_MIGRATE"

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
	"which": true, "edit": true, "init": true, "doctor": true, "migrate": true,
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
	boundary := deps.LegacyBoundary
	cmd := &cobra.Command{
		Use:     "launch [harness args…]",
		Aliases: []string{"cl"},
		Short:   "Per-project launcher for Claude Code or Codex CLI",
		Long: `launch resolves a per-project profile from your working directory,
then execs the configured harness with that posture — no prompts.

  forgectl launch                 drop straight into the resolved profile
  forgectl launch <args…>          apply the profile and pass args through
  forgectl launch agents …         Claude-only agent-management passthrough

To resume or fork an earlier session, use "forgectl resume" — it discovers
sessions across repos, flags the live ones, and restores their tasks.

Run "forgectl launch which" to see the profile resolved for the current
directory. Profiles live in the [launch] section of config.toml — scaffold one
with "forgectl launch init".`,
		// Bare `forgectl launch` is handled by the Execute intercept, so this
		// RunE only fires if Cobra reaches it directly; keep it correct.
		RunE: func(_ *cobra.Command, args []string) error {
			return launchExec(boundary, cfg, args)
		},
	}
	cmd.AddCommand(
		newLaunchWhichCmd(boundary, cfg),
		newLaunchEditCmd(),
		newLaunchInitCmd(boundary),
		newLaunchDoctorCmd(boundary, cfg),
		newLaunchMigrateCmd(boundary),
	)
	applyAliases(cmd, launchAliases)
	return cmd
}

// runLaunch dispatches a `forgectl launch …` invocation. Own-verbs return
// handled=false and are left for the normal fang path (styled help); everything
// else (bare, builder, agents) execs claude directly.
func runLaunch(deps module.Deps, rest []string) (handled bool, err error) {
	if len(rest) > 0 && isOwnLaunchVerb(rest[0]) {
		return false, nil // own-verb → fang dispatches the launch subtree
	}
	return true, launchExec(deps.LegacyBoundary, deps.Cfg, rest)
}

// launchExec is the resolve → exec path: it reduces the launch config against
// the cwd, resolves the claude binary, assembles the posture, merges env, and
// execs claude in place. On success it does not return (syscall.Exec replaces
// the process).
func launchExec(boundary *config.LegacyMigrationBoundary, cfg config.Config, args []string) error {
	effLaunch, notice := autoMigrateOrWarnLegacyLaunch(boundary, cfg)
	if notice != "" {
		fmt.Fprintln(os.Stderr, "forgectl: "+termsafe.SafeLine(notice))
	}
	cfg.Launch = effLaunch
	lc, _ := resolveLaunchConfig(boundary, cfg)

	cwd, err := os.Getwd()
	if err != nil {
		return termsafe.Error(fmt.Errorf("determine working directory: %w", err))
	}
	profile := launch.Resolve(lc, cwd)

	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.Harness == "codex" && len(args) > 0 && args[0] == "agents" {
		return fmt.Errorf(
			"`launch agents` is Claude-only and has no Codex adapter; invoke Codex directly or switch this launch profile to Claude",
		)
	}

	var binaryPath string
	if profile.Harness == "codex" {
		binaryPath, err = launch.CodexPath(lc.Defaults)
	} else {
		binaryPath, err = launch.ClaudePath(lc.Defaults)
	}
	if err != nil {
		return err
	}

	var harnessArgs []string
	switch {
	case len(args) == 0:
		if profile.Harness == "codex" {
			harnessArgs = launch.CodexSessionArgs(profile)
			// Codex has no equivalent of the Claude agents banner, so without
			// this a Codex launch leaves no record of the argv it ran with —
			// including the approval/sandbox posture, which is the part worth
			// auditing. stderr, so piped stdout stays clean.
			launch.HarnessBanner(os.Stderr, profile.Harness, harnessArgs)
		} else {
			harnessArgs = launch.SessionArgs(profile)
			// This is the ONLY path that emits
			// --allow-dangerously-skip-permissions into an INTERACTIVE session
			// (the scaffold ships allow_danger = true), and it is the one branch
			// that printed nothing at all once the interview form stopped
			// rendering ahead of syscall.Exec. Banner it like the agents branch
			// does, so the posture — and the resolved effort — stay visible.
			launch.Banner(os.Stderr, harnessArgs)
		}
	case args[0] == "agents":
		if launch.IsAgentsPassthrough(args) {
			harnessArgs = args // byte-clean: no injection, no banner
		} else {
			harnessArgs = launch.AgentsArgs(profile, args)
			launch.Banner(os.Stderr, harnessArgs)
		}
	default:
		if profile.Harness == "codex" {
			harnessArgs = launch.CodexExecArgs(profile, args)
			launch.HarnessBanner(os.Stderr, profile.Harness, harnessArgs)
		} else {
			harnessArgs = launch.BuilderArgs(profile, args)
		}
	}

	// Layer the profile env over the opt-in bench telemetry block (profile wins),
	// then merge that over the process env. When telemetry is off, TelemetryEnv is
	// nil and this reduces to the profile env alone.
	extra := launch.MergeMaps(bench.TelemetryEnv(cfg), profile.Env)
	env := launch.MergeEnv(os.Environ(), extra)
	slog.Debug("Preparing to exec harness.", "harness", termsafe.SafeLine(profile.Harness), "path", termsafe.QuotePath(binaryPath), "argc", len(harnessArgs), "match", termsafe.SafeLine(profile.Match))
	return launch.Exec(binaryPath, harnessArgs, env)
}

// legacyShadowWarning reports the one-line #114 fallback-cliff warning when
// config.toml declares a live [launch] section AND a legacy claunch.conf is
// still present on disk. resolveLaunchConfig returns config.toml's [launch]
// wholesale the instant it's non-zero — even a bare [launch.defaults]
// binary_path — so any [[project]] profiles left in the legacy file are
// silently orphaned: no error, no stderr, exit 0. This is presence-not-parse:
// the warning fires on the legacy file merely existing (its own os.Stat),
// regardless of whether it would even parse, because either way it's being
// ignored.
// Returns "" when there's nothing to warn about.
//
// The remedy MUST point at `forgectl launch edit`, not `launch init`: init's
// own RunE refuses with "config already has a [launch] section" (see
// launch_init.go) whenever cfg.Launch is non-zero — which is exactly the only
// state this warning ever fires in. Pointing at init here would be a
// guaranteed dead end.
func legacyShadowWarning(boundary *config.LegacyMigrationBoundary, cfg config.Config) string {
	if !cfg.HasLaunchSection() {
		return "" // legacy honored, nothing shadowed
	}
	if boundary == nil || boundary.Status == config.BoundaryNoSource {
		return ""
	}
	return "legacy claunch config at " + termsafe.QuotePath(boundary.LegacyPath) + " is present but ignored — config.toml's " +
		"[launch] section takes precedence; migrate its profiles into [launch] and remove it " +
		"(edit it with `forgectl launch edit`)"
}

// resolveLaunchConfig returns the [launch] section from config.toml plus a
// human source label. When that section is absent it falls back to a legacy
// ~/.config/claunch/claunch.conf (zero-migration grace); when neither exists it
// returns the empty config and points at where `forgectl launch init` writes.
func resolveLaunchConfig(boundary *config.LegacyMigrationBoundary, cfg config.Config) (config.LaunchConfig, string) {
	path := ""
	if boundary != nil {
		path = boundary.ConfigPath
	} else {
		path, _ = config.ConfigPath()
	}
	if cfg.HasLaunchSection() {
		return cfg.Launch, path
	}
	var legacy config.LaunchConfig
	var err error
	legacyPath := ""
	if boundary != nil {
		legacyPath = boundary.LegacyPath
		legacy, err = boundary.LoadReadOnlyLegacy()
	} else {
		legacy, legacyPath, err = config.LoadLegacyLaunch()
	}
	switch {
	case err == nil:
		slog.Debug("Using legacy claunch config (no [launch] section in config.toml).", "path", termsafe.QuotePath(legacyPath))
		return legacy, legacyPath + " (legacy)"
	case !errors.Is(err, config.ErrNoLegacyLaunch):
		// A malformed or unreadable legacy file shouldn't block normal launch —
		// warn and fall through to config.toml (an absent file is silent).
		slog.Warn("Ignoring unreadable legacy claunch config.", "path", termsafe.QuotePath(legacyPath), "error", termsafe.SafeLine(err.Error()))
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		// config.toml exists but declares no [launch] section (and there is no
		// legacy claunch.conf). Distinguish this from a truly absent file so the
		// label doesn't send the reader chasing a phantom missing-file problem (#57).
		return cfg.Launch, path + " (no [launch] section — built-in defaults)"
	case os.IsNotExist(err):
		return cfg.Launch, path + " (missing — built-in defaults)"
	default:
		// A permission failure (or other stat error) is not the same as an
		// absent file — surface it distinctly rather than let it masquerade
		// as "missing" and send the reader chasing the wrong fix.
		return cfg.Launch, path + " (unreadable: " + err.Error() + " — built-in defaults)"
	}
}

// autoMigrateOrWarnLegacyLaunch is the entry point every launch surface
// (bare exec, which, doctor) calls before resolving a profile. When a legacy
// claunch.conf is present it migrates it automatically instead of just
// warning about it:
//
//   - fallback scenario (cfg.Launch.IsZero()): import the legacy file
//     wholesale into a fresh [launch] section (the same logic `launch
//     migrate` runs on demand) — see autoMigrateFallback.
//   - shadow scenario (cfg.Launch non-zero, #114's "present but ignored"
//     warning): additively merge the legacy file into [launch] — see
//     autoMigrateShadow. This can never clobber or duplicate anything
//     already set, so it's always safe to run without asking.
//
// Either way the legacy file is renamed to claunch.conf.bak (never deleted)
// so the warning stops recurring once there's nothing left to migrate.
// FORGECTL_SKIP_LEGACY_MIGRATE=1 disables all of this and restores the
// original warn-only behavior (legacyShadowWarning).
//
// Returns the effective LaunchConfig for this invocation — cfg.Launch,
// rewritten in place when a migration just ran, so callers don't need to
// re-read config.toml — plus a message to print, or "" when there's nothing
// to report.
func autoMigrateOrWarnLegacyLaunch(boundary *config.LegacyMigrationBoundary, cfg config.Config) (config.LaunchConfig, string) {
	if os.Getenv(skipLegacyMigrateEnv) != "" {
		if !cfg.HasLaunchSection() && boundary != nil {
			if fallback, err := boundary.LoadReadOnlyLegacy(); err == nil {
				return fallback, ""
			}
		}
		return cfg.Launch, legacyShadowWarning(boundary, cfg)
	}
	result := migrateLegacyAutomatically(boundary, cfg, nativeMigrationTxnOps())
	if result.Err != nil {
		slog.Warn("Automatic claunch.conf migration did not fully retire the source.",
			"path", func() string {
				if boundary == nil {
					return ""
				}
				return termsafe.QuotePath(boundary.LegacyPath)
			}(), "error", termsafe.SafeLine(result.Err.Error()), "commit", result.Commit, "backup", result.Backup, "retirement", result.Retirement)
	}
	return result.Effective, result.Notice
}
