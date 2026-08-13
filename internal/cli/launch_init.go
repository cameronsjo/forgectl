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
# ── launch: per-project Claude/Codex launcher (forgectl launch) ─────────────
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

[launch.defaults]
harness         = "claude"   # "claude" (default) or "codex"
model           = "opus"     # remove or replace with a Codex model when harness = "codex"
# effort        = "medium"   # low|medium|high|xhigh|max; unset = derived from model
permission_mode = "plan"     # launch always starts in plan
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this
# Codex-native settings (used when harness = "codex"):
# approval_policy  = "on-request"
# sandbox          = "read-only"     # launch always starts non-writing
# codex_binary_path = ""      # $FORGECTL_CODEX_BIN overrides this

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
		return result.Err
	}
	slog.Info("Successfully imported legacy claunch.conf.", "legacy_path", termsafe.QuotePath(boundary.LegacyPath), "config_path", termsafe.QuotePath(boundary.ConfigPath), "project_count", len(result.Effective.Projects))
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
