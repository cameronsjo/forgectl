package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// launchScaffold is the [launch] section appended to config.toml by
// `forgectl launch init`. Values mirror the built-in defaults so an untouched
// scaffold is a no-op posture.
const launchScaffold = `
# ── launch: per-project Claude/Codex/Pi launcher (forgectl launch) ──────────
# Resolution: expand ~, pick the [[launch.project]] whose match is the longest
# path-prefix of the real cwd, then merge over [launch.defaults].
#   scalars: project wins when set; Claude remains the compatibility default
#   env: merged, project wins on key collisions
#   add_dir: concatenated and de-duplicated
#   effort: derived from the FINAL model last, so a project overriding only
#           model re-derives — sonnet high, opus/fable medium, anything else
#           no --effort at all (your settings.json effortLevel stays in charge).
#           Set effort at either layer to override the derived level.
# Inspect the resolved profile with:  forgectl launch which

[launch]
# Local launch statistics — OFF unless you set this to true, and never turned
# on by an upgrade, a migration, or an environment variable.
#
# When on, forgectl appends one line to
# ${XDG_STATE_HOME:-~/.local/state}/forgectl/launch-usage.jsonl immediately
# before each harness exec it attempts. That line holds exactly seven fields:
#   schema_version  the row format
#   ts              a UTC timestamp, to the second
#   event           always "exec_attempt"
#   harness         "claude", "codex", or "pi"
#   model           the resolved model, "" for the harness default
#   session_mode    new | resume | fork | unknown
#   posture         default | builder | agents
# Nothing else: no directory, project, repository, branch, session id, harness
# arguments, prompt, environment, tasks, host, user, or pid — and no hash of
# any of them.
#
# Those seven are still sensitive. Exact timestamps describe when you work, a
# model label can name an internal deployment, and the session and posture
# counts describe how you work. Aggregating locally does not change that.
#
# Nothing is uploaded: no network call, no device identifier, no import of the
# retired claunch wrapper's log. Rows are kept until you delete them. Setting
# this back to false stops new rows but neither hides nor removes old ones —
# delete them yourself, while no forgectl launch, resume, or stats is running:
#   rm -- "${XDG_STATE_HOME:-$HOME/.local/state}/forgectl/launch-usage.jsonl"
#   rm -- "${XDG_STATE_HOME:-$HOME/.local/state}/forgectl/launch-usage.jsonl.lock"
# Deletion is permanent. Read the aggregate with "forgectl launch stats"; the
# file itself is plain JSON Lines, so it is also your export.
usage_stats = false

[launch.defaults]
harness         = "claude"   # "claude" (default), "codex", or "pi"
model           = "opus"     # remove or replace for Codex/Pi
# effort        = "medium"   # low|medium|high|xhigh|max; unset = derived from model
permission_mode = "plan"     # Claude starts in plan
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this
# Codex-native settings (used when harness = "codex"):
# approval_policy  = "on-request"
# sandbox          = "read-only"     # launch always starts non-writing
# codex_binary_path = ""      # $FORGECTL_CODEX_BIN overrides this
# Pi-native settings (used when harness = "pi"):
# provider       = "lm-studio" # optional; unset = Pi's configured/default provider
# pi_binary_path = ""          # $FORGECTL_PI_BIN overrides this

# Per-project overrides — add as many [[launch.project]] blocks as you like.
# [[launch.project]]
# match   = "~/Projects/minute"
# model   = "sonnet"
# effort  = "xhigh"          # omit to take sonnet's derived "high"
# env     = { OTEL_EXPORTER = "otlp" }
# add_dir = ["~/Projects/minute/shared"]
`

func newLaunchInitCmd(boundary *config.LegacyMigrationBoundary) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold the [launch] section into config.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// GetBool only errors on an undefined or non-bool flag; from-claunch
			// is registered as Bool below, so the error is unreachable here.
			if fromClaunch, _ := cmd.Flags().GetBool("from-claunch"); fromClaunch {
				return termsafe.Error(runLaunchMigrate(cmd, boundary))
			}
			if err := refuseConfigMutationForLegacyBoundary(boundary); err != nil {
				return termsafe.Error(err)
			}
			path := ""
			if boundary != nil {
				path = boundary.ConfigPath
			} else {
				var err error
				path, err = config.ConfigPath()
				if err != nil {
					return termsafe.Error(err)
				}
			}
			action, err := updateConfigLocked(path, nativeConfigWriterOps(), func(raw []byte) ([]byte, error) {
				if hasLaunchSection(raw) {
					return nil, fmt.Errorf("config already has a [launch] section at %s (edit it with `forgectl launch edit`); refusing to overwrite an existing launch profile", termsafe.QuotePath(path))
				}
				return append(raw, []byte(launchScaffold)...), nil
			})
			if err != nil && !visibleWithoutDirectoryDurability(action, err) {
				return termsafe.Error(err)
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "forgectl: config is visible, but directory durability and cross-process locking are unavailable on this platform")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added a [launch] section to %s\n", termsafe.QuotePath(path))
			return nil
		},
	}
	cmd.Flags().Bool("from-claunch", false, "deprecated: use `forgectl launch migrate`; import an existing ~/.config/claunch/claunch.conf into config.toml")
	return cmd
}

// newLaunchMigrateCmd builds `forgectl launch migrate` — the discoverable
// spelling of the one-shot claunch.conf importer. `launch init --from-claunch`
// remains a deprecated alias for the same runLaunchMigrate logic so existing
// docs/muscle memory don't break.
func newLaunchMigrateCmd(boundary *config.LegacyMigrationBoundary) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Import an existing ~/.config/claunch/claunch.conf into config.toml's [launch] section",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return termsafe.Error(runLaunchMigrate(cmd, boundary))
		},
	}
}

// runLaunchMigrate migrates an existing legacy ~/.config/claunch/claunch.conf
// into config.toml's [launch] section, so `forgectl launch` stops falling
// back to the legacy file. It refuses to run when config.toml already has a
// [launch] section — import once, then edit config.toml directly. Shared by
// `forgectl launch migrate` and the deprecated `launch init --from-claunch`
// spelling.
func runLaunchMigrate(cmd *cobra.Command, boundary *config.LegacyMigrationBoundary) error {
	slog.Debug("Preparing to import legacy claunch.conf into config.toml.")
	owned := false
	if boundary == nil {
		env, err := config.CaptureEnvSnapshot()
		if err != nil {
			return err
		}
		boundary, err = config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
		if err != nil {
			return err
		}
		owned = true
	}
	if owned {
		defer boundary.Close() //nolint:errcheck
	}
	result := migrateLegacyExplicit(boundary, nativeMigrationTxnOps())
	if result.Err != nil {
		if errors.Is(result.Err, config.ErrLegacyMalformed) {
			return fmt.Errorf("legacy claunch.conf is malformed, not importing: %w", result.Err)
		}
		if errors.Is(result.Err, config.ErrNoLegacyLaunch) {
			// #417: forgectl migrates the historical claunch.conf format only.
			// A claunch that has since moved to a config.toml of its own is
			// present, not absent — name it rather than send the operator
			// looking for a file that is sitting right there.
			if sibling := boundary.UnmigratableSiblingPath(); sibling != "" {
				return fmt.Errorf("no legacy claunch.conf found, but %s is present — forgectl migrates the historical claunch.conf format only, so settle that file by hand",
					termsafe.QuotePath(sibling))
			}
		}
		if errors.Is(result.Err, config.ErrLegacyUnsupportedFields) {
			// #417: importing would render [launch] from a partial decode and
			// drop the rest. Name the fields so the operator can settle them
			// by hand; forgectl migrates the historical format only.
			// The key names are read off the error rather than re-derived
			// from boundary.Source: one source for the list, and no second
			// dereference of a snapshot this arm does not otherwise touch.
			return fmt.Errorf("legacy claunch.conf carries settings forgectl cannot represent, not importing: %s",
				termsafe.SafeLine(result.Err.Error()))
		}
		return result.Err
	}
	slog.Info("Successfully imported legacy claunch.conf.", "legacy_path", termsafe.QuotePath(boundary.LegacyPath), "config_path", termsafe.QuotePath(boundary.ConfigPath), "project_count", len(result.Effective.Projects))
	// result.Notice is deliberately not reused here: the explicit surface
	// prints the resolved paths the notice has no room for. A zero-profile
	// import is legitimate ([defaults] and no [[project]]) and would otherwise
	// read as a no-op, so it gets its own line.
	if len(result.Effective.Projects) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Imported launch defaults (no project profiles) from %s into %s\n", termsafe.QuotePath(boundary.LegacyPath), termsafe.QuotePath(boundary.ConfigPath))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Imported %d launch profile(s) from %s into %s\n", len(result.Effective.Projects), termsafe.QuotePath(boundary.LegacyPath), termsafe.QuotePath(boundary.ConfigPath))
	return nil
}

// hasLaunchSection reports whether data already defines a [launch] table. It
// delegates to the generalized hasSection (init_cmd.go) — the same real-TOML-
// header match (rather than a loose substring that would also fire on comments,
// string values, or an unrelated [launcher] table), specialized to [launch].
func hasLaunchSection(data []byte) bool {
	return hasSection(data, "launch")
}
