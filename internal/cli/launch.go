package cli

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/step"
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
			return launchExec(cfg, args)
		},
	}
	cmd.AddCommand(
		newLaunchWhichCmd(cfg),
		newLaunchEditCmd(),
		newLaunchInitCmd(),
		newLaunchDoctorCmd(cfg),
		newLaunchMigrateCmd(),
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

// launchExec is the resolve → exec path: it reduces the launch config against
// the cwd, resolves the claude binary, assembles the posture, merges env, and
// execs claude in place. On success it does not return (syscall.Exec replaces
// the process).
func launchExec(cfg config.Config, args []string) error {
	effLaunch, notice := autoMigrateOrWarnLegacyLaunch(cfg)
	if notice != "" {
		fmt.Fprintln(os.Stderr, "forgectl: "+notice)
	}
	cfg.Launch = effLaunch
	lc, _ := resolveLaunchConfig(cfg)

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine working directory: %w", err)
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
	slog.Debug("Preparing to exec harness.", "harness", profile.Harness, "path", binaryPath, "argc", len(harnessArgs), "match", profile.Match)
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
func legacyShadowWarning(cfg config.Config) string {
	if cfg.Launch.IsZero() {
		return "" // legacy honored, nothing shadowed
	}
	path, err := config.LegacyLaunchPath()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return "" // no legacy file present — nothing to shadow
	}
	return "legacy claunch config at " + path + " is present but ignored — config.toml's " +
		"[launch] section takes precedence; migrate its profiles into [launch] and remove it " +
		"(edit it with `forgectl launch edit`)"
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
	switch legacy, legacyPath, err := config.LoadLegacyLaunch(); {
	case err == nil:
		slog.Debug("Using legacy claunch config (no [launch] section in config.toml).", "path", legacyPath)
		return legacy, legacyPath + " (legacy)"
	case !errors.Is(err, config.ErrNoLegacyLaunch):
		// A malformed or unreadable legacy file shouldn't block normal launch —
		// warn and fall through to config.toml (an absent file is silent).
		slog.Warn("Ignoring unreadable legacy claunch config.", "path", legacyPath, "error", err)
	}
	path, _ := config.ConfigPath()
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
func autoMigrateOrWarnLegacyLaunch(cfg config.Config) (config.LaunchConfig, string) {
	if os.Getenv(skipLegacyMigrateEnv) != "" {
		return cfg.Launch, legacyShadowWarning(cfg)
	}

	legacyPath, err := config.LegacyLaunchPath()
	if err != nil {
		return cfg.Launch, ""
	}
	if _, err := os.Stat(legacyPath); err != nil {
		return cfg.Launch, "" // no legacy file present — nothing to migrate
	}

	// A config.toml that fails to decode/validate isn't safe to migrate
	// against: Load() tolerates a decode error by silently returning whatever
	// the decoder populated before erroring, so cfg here may already be
	// missing sections the decoder never reached. Writing a migration on top
	// of that partial state would risk discarding them permanently. Mirrors
	// `launch doctor`'s own config.Validate() health check (launch_doctor.go)
	// — the automatic path previously skipped it entirely.
	if err := config.Validate(); err != nil {
		slog.Warn("Skipping automatic claunch.conf migration: config.toml failed to validate.",
			"path", legacyPath, "error", err)
		return cfg.Launch, ""
	}

	configPath, err := config.ConfigPath()
	if err != nil {
		return cfg.Launch, ""
	}

	// fallback is decided once, up front, from the caller's cfg.Launch — the
	// same snapshot taken at process start (execute.go) — and used to pick
	// BOTH which migration runs inside the lock AND how a failure/no-op
	// result is reported below, so the two decisions can never disagree with
	// each other even though the branch itself can still be stale by the
	// time the lock is acquired (autoMigrateFallback/autoMigrateShadow each
	// re-verify their own precondition immediately before writing).
	fallback := cfg.Launch.IsZero()

	var lc config.LaunchConfig
	var msg string
	var migrateErr error
	lockErr := config.WithFileLock(configPath, func() error {
		if fallback {
			lc, msg, migrateErr = autoMigrateFallback()
		} else {
			lc, msg, migrateErr = autoMigrateShadow(cfg, legacyPath)
		}
		return nil // migrateErr is reported below, not via the lock's own error
	})
	if lockErr != nil {
		slog.Warn("Automatic claunch.conf migration could not acquire its lock; falling back to legacy read-through.",
			"path", legacyPath, "error", lockErr)
		return cfg.Launch, ""
	}
	if migrateErr != nil {
		if fallback {
			slog.Warn("Automatic claunch.conf migration failed; falling back to legacy read-through.",
				"path", legacyPath, "error", migrateErr)
			return cfg.Launch, ""
		}
		slog.Warn("Automatic claunch.conf merge failed; showing the original shadow warning instead.",
			"path", legacyPath, "error", migrateErr)
		return cfg.Launch, legacyShadowWarning(cfg)
	}
	if msg == "" {
		// Either nothing to import/merge, or a concurrent forgectl process
		// already migrated (and renamed the legacy file away) while this one
		// waited for the lock — either way, nothing new to report.
		return cfg.Launch, ""
	}
	return lc, msg
}

// autoMigrateFallback runs the fallback-scenario migration: legacy
// claunch.conf present, no [launch] section yet. It imports the legacy
// config wholesale (writeImportedLaunchSection, the same path `launch
// migrate` uses) and backs the legacy file up. Returns a zero LaunchConfig
// and "" (not an error) when the legacy file decodes cleanly but has nothing
// to import — the caller treats that as a no-op, matching runLaunchMigrate's
// own IsZero refusal. The same zero/""/nil result also covers "a concurrent
// forgectl process already migrated while this one waited for the lock" —
// see the two re-checks below.
//
// MUST be called only while holding config.WithFileLock on config.toml
// (autoMigrateOrWarnLegacyLaunch is the only caller, and does): the caller's
// cfg.Launch.IsZero() check that routed here ran at process start, before
// the lock was acquired, and can be stale by the time this function runs —
// this is the read-decide-write critical section the lock exists to
// serialize.
func autoMigrateFallback() (config.LaunchConfig, string, error) {
	lc, legacyPath, err := config.LoadLegacyLaunch()
	if err != nil {
		if errors.Is(err, config.ErrNoLegacyLaunch) {
			// A concurrent process already migrated and renamed the legacy
			// file away while we waited for the lock — nothing left to do.
			return config.LaunchConfig{}, "", nil
		}
		return config.LaunchConfig{}, "", err
	}
	if lc.IsZero() {
		return config.LaunchConfig{}, "", nil
	}

	path, err := config.ConfigPath()
	if err != nil {
		return config.LaunchConfig{}, "", err
	}
	// Re-check immediately before writing, now that the lock is held: mirrors
	// refuseIfLaunchSection's own guard on the manual importer
	// (runLaunchMigrate) — the automatic path must carry the same safety
	// property, not trust a snapshot taken before this lock was acquired. A
	// hit here means another process added [launch] (by any means) between
	// our caller's stale check and now; bail cleanly rather than duplicate
	// the section.
	if err := refuseIfLaunchSection(path); err != nil {
		return config.LaunchConfig{}, "", nil
	}
	if err := writeImportedLaunchSection(path, lc, legacyPath); err != nil {
		return config.LaunchConfig{}, "", err
	}
	if err := backupAndRemoveLegacy(legacyPath); err != nil {
		return config.LaunchConfig{}, "", err
	}

	slog.Info("Automatically migrated legacy claunch.conf into config.toml.",
		"legacy_path", legacyPath, "config_path", path, "project_count", len(lc.Projects))
	msg := fmt.Sprintf(
		"migrated %d profile(s) from claunch.conf into config.toml's [launch] section (old file kept as claunch.conf.bak)",
		len(lc.Projects))
	return lc, msg, nil
}

// autoMigrateShadow runs the shadow-scenario migration (#114 automated):
// config.toml already has a live [launch] section AND a legacy claunch.conf
// is still present. config.MergeLegacyIntoLaunch does the additive-only
// merge; when it contributes nothing new the legacy file is simply retired
// (renamed, no rewrite needed since the merged value equals cfg.Launch).
//
// MUST be called only while holding config.WithFileLock on config.toml —
// see autoMigrateFallback's doc comment for why (the same stale-snapshot
// hazard applies here). The re-load of the legacy file below doubles as the
// re-check: a concurrent process racing the same merge already renamed it
// away, so a second racer's LoadLegacyLaunch fails with ErrNoLegacyLaunch
// and bails cleanly instead of re-deriving and re-writing a merge against a
// legacy file that's already gone.
func autoMigrateShadow(cfg config.Config, legacyPath string) (config.LaunchConfig, string, error) {
	legacy, _, err := config.LoadLegacyLaunch()
	if err != nil {
		if errors.Is(err, config.ErrNoLegacyLaunch) {
			return config.LaunchConfig{}, "", nil
		}
		return config.LaunchConfig{}, "", err
	}

	merged, added := config.MergeLegacyIntoLaunch(cfg, legacy)
	if added > 0 {
		path, err := config.ConfigPath()
		if err != nil {
			return config.LaunchConfig{}, "", err
		}
		if err := replaceLaunchSection(path, merged, legacyPath); err != nil {
			return config.LaunchConfig{}, "", err
		}
	}
	if err := backupAndRemoveLegacy(legacyPath); err != nil {
		return config.LaunchConfig{}, "", err
	}

	var msg string
	if added == 0 {
		msg = "legacy config fully superseded, removed."
		slog.Info("Legacy claunch.conf was fully shadowed by config.toml's [launch] section; removed.",
			"legacy_path", legacyPath)
	} else {
		msg = fmt.Sprintf(
			"merged %d addition(s) from claunch.conf into config.toml's [launch] section (old file kept as claunch.conf.bak)",
			added)
		slog.Info("Automatically merged legacy claunch.conf into config.toml's [launch] section.",
			"legacy_path", legacyPath, "added", added)
	}
	return merged, msg, nil
}

// backupAndRemoveLegacy renames the legacy config file at path to
// "<path>.bak" — the legacy claunch.conf is never hard-deleted by an
// automatic migration, only moved aside, so an operator who wants the
// original back can always find it.
func backupAndRemoveLegacy(path string) error {
	if err := os.Rename(path, path+".bak"); err != nil {
		return fmt.Errorf("back up legacy config %s: %w", path, err)
	}
	return nil
}

// replaceLaunchSection rewrites config.toml at path, dropping every line
// belonging to any [launch]-family table (wherever it appears in the file —
// TOML tables need not be contiguous) and splicing in a freshly encoded
// [launch] block for merged at the position of the first one found. Used
// only by the shadow-scenario auto-migration, where cfg.Launch is already
// non-zero, so a rewrite (not append) is required — appending a second
// [launch.defaults] table would be invalid TOML (a redefined table).
// Everything outside the launch tables — comments, other sections — is
// preserved verbatim, including a comment block that sits directly above the
// next section's header (see the backward-correction pass below).
//
// Table boundaries are resolved with tomlLineScanner (launch_scan.go), not a
// naive "line starts with [ and ends with ]" check: that misses a header
// carrying a trailing comment (`[bench] # …` never ends in "]", so the
// scanner never notices the boundary and silently drops everything after it
// — including unrelated sections — until some other header happens to
// parse) and can misfire inside a multi-line string or array. When the scan
// can't unambiguously resolve a line (or the file ends mid multi-line
// string/array), this returns an error and writes nothing — fail safe,
// never fail silent; the caller (autoMigrateShadow) treats that exactly like
// any other migration failure and leaves config.toml untouched.
//
// The write itself is atomic (writeConfigAtomic): a temp file in the same
// directory, renamed over path, so a process killed mid-write can never
// leave config.toml truncated or empty with no recovery copy.
func replaceLaunchSection(path string, merged config.LaunchConfig, legacyPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")

	// dropped[i] tracks whether line i belongs to a [launch]-family table and
	// should be removed; commentOrBlank[i] flags a stand-alone comment or
	// blank line (never true while a multi-line string/array is still open —
	// that's string/array content, not a real comment, however it looks
	// trimmed).
	dropped := make([]bool, len(lines))
	commentOrBlank := make([]bool, len(lines))

	var scanner tomlLineScanner
	inLaunch := false
	for i, line := range lines {
		mid := scanner.inTripleBasic || scanner.inTripleLiteral || scanner.bracketDepth > 0
		trimmed := strings.TrimSpace(line)
		commentOrBlank[i] = !mid && (trimmed == "" || strings.HasPrefix(trimmed, "#"))

		table, ok := scanner.scanLine(line)
		if !ok {
			return fmt.Errorf(
				"rewrite config %s: line %d has TOML this scanner cannot unambiguously resolve (%q); refusing to guess",
				path, i+1, line)
		}
		if table != "" {
			inLaunch = isLaunchTable(table)
		}
		dropped[i] = inLaunch
	}
	if scanner.pending() {
		return fmt.Errorf("rewrite config %s: file ends inside an unterminated multi-line string or array; refusing to guess", path)
	}

	// A blank/comment-only line takes on the classification of whatever
	// substantive line follows it, not whatever came before it. In raw file
	// order a comment documenting the NEXT section still reads as "inside"
	// whatever table preceded it until that table's header is reached — so
	// without this correction, a comment block written directly above (say)
	// [bench] would be misattributed to [launch] and dropped right along
	// with it, purely because of where it happens to sit in the byte stream.
	for i := len(dropped) - 2; i >= 0; i-- {
		if commentOrBlank[i] {
			dropped[i] = dropped[i+1]
		}
	}

	var kept []string
	firstLaunchIdx := -1
	for i, line := range lines {
		if dropped[i] {
			if firstLaunchIdx == -1 {
				firstLaunchIdx = len(kept)
			}
			continue // dropped — the merged block below replaces it
		}
		kept = append(kept, line)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(struct {
		Launch config.LaunchConfig `toml:"launch"`
	}{Launch: merged}); err != nil {
		return fmt.Errorf("encode merged launch config: %w", err)
	}
	header := fmt.Sprintf("# ── launch: merged with %s (forgectl launch migrate) ──", legacyPath)
	block := append([]string{header}, strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")...)

	var out []string
	if firstLaunchIdx == -1 {
		// Shouldn't happen — this is only called when cfg.Launch is non-zero,
		// i.e. an existing launch table was already found by resolveLaunchConfig
		// — but append rather than drop the merge if it ever does.
		out = append(kept, "")
		out = append(out, block...)
	} else {
		out = append(out, kept[:firstLaunchIdx]...)
		out = append(out, block...)
		out = append(out, kept[firstLaunchIdx:]...)
	}

	if err := writeConfigAtomic(path, []byte(strings.Join(out, "\n"))); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
