package cli

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
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

func newLaunchInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold the [launch] section into config.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// GetBool only errors on an undefined or non-bool flag; from-claunch
			// is registered as Bool below, so the error is unreachable here.
			if fromClaunch, _ := cmd.Flags().GetBool("from-claunch"); fromClaunch {
				return runLaunchMigrate(cmd)
			}
			path, err := config.ConfigPath()
			if err != nil {
				return err
			}
			if err := refuseIfLaunchSection(path); err != nil {
				return err
			}
			if err := appendLaunchSection(path, launchScaffold); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added a [launch] section to %s\n", path)
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
func newLaunchMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Import an existing ~/.config/claunch/claunch.conf into config.toml's [launch] section",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLaunchMigrate(cmd)
		},
	}
}

// runLaunchMigrate migrates an existing legacy ~/.config/claunch/claunch.conf
// into config.toml's [launch] section, so `forgectl launch` stops falling
// back to the legacy file. It refuses to run when config.toml already has a
// [launch] section — import once, then edit config.toml directly. Shared by
// `forgectl launch migrate` and the deprecated `launch init --from-claunch`
// spelling.
func runLaunchMigrate(cmd *cobra.Command) error {
	slog.Debug("Preparing to import legacy claunch.conf into config.toml.")

	path, err := config.ConfigPath()
	if err != nil {
		return err
	}
	if err := refuseIfLaunchSection(path); err != nil {
		return err
	}

	lc, legacyPath, err := config.LoadLegacyLaunch()
	if err != nil {
		if errors.Is(err, config.ErrNoLegacyLaunch) {
			return err
		}
		return fmt.Errorf("legacy claunch.conf is malformed, not importing: %w", err)
	}
	if lc.IsZero() {
		return fmt.Errorf("legacy claunch.conf at %s has no [defaults] or [[project]] to import", legacyPath)
	}
	slog.Debug("Loaded legacy claunch.conf.", "path", legacyPath, "project_count", len(lc.Projects))

	if err := writeImportedLaunchSection(path, lc, legacyPath); err != nil {
		return err
	}

	slog.Info("Successfully imported legacy claunch.conf.", "legacy_path", legacyPath, "config_path", path, "project_count", len(lc.Projects))
	fmt.Fprintf(cmd.OutOrStdout(), "Imported %d launch profile(s) from %s into %s\n", len(lc.Projects), legacyPath, path)
	return nil
}

// writeImportedLaunchSection encodes lc as a [launch] TOML block, headed by a
// comment naming its legacy source, and appends it to config.toml at path.
// Shared by runLaunchMigrate (the explicit importer) and the automatic
// fallback-scenario migration (launch.go's autoMigrateFallback), which both
// import a legacy claunch.conf wholesale into an absent [launch] section.
func writeImportedLaunchSection(path string, lc config.LaunchConfig, legacyPath string) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(struct {
		Launch config.LaunchConfig `toml:"launch"`
	}{Launch: lc}); err != nil {
		return fmt.Errorf("encode imported launch config: %w", err)
	}
	header := fmt.Sprintf("\n# ── launch: imported from %s (forgectl launch migrate) ──\n", legacyPath)
	return appendLaunchSection(path, header+buf.String())
}

// refuseIfLaunchSection errors when config.toml at path already has a
// [launch] section — both `launch init` and its `--from-claunch` importer
// refuse to run over an existing profile rather than risk overwriting it. A
// missing file is not an error; the caller appends to it fresh.
func refuseIfLaunchSection(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}
	if hasLaunchSection(data) {
		return fmt.Errorf("config already has a [launch] section at %s (edit it with `forgectl launch edit`); refusing to overwrite an existing launch profile", path)
	}
	return nil
}

// appendLaunchSection appends content to the config.toml at path, creating
// the parent directory and the file if absent. Both `launch init` and its
// `--from-claunch` importer append a TOML block this way, preserving any
// sections already in the file.
func appendLaunchSection(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close() // the write error is the real failure; Close's is secondary
		return fmt.Errorf("write config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	return nil
}

// hasLaunchSection reports whether data already defines a [launch] table. It
// delegates to the generalized hasSection (init_cmd.go) — the same real-TOML-
// header match (rather than a loose substring that would also fire on comments,
// string values, or an unrelated [launcher] table), specialized to [launch].
func hasLaunchSection(data []byte) bool {
	return hasSection(data, "launch")
}
