package cli

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
)

// launchScaffold is the [launch] section appended to config.toml by
// `forgectl launch init`. Values mirror the built-in defaults so an untouched
// scaffold is a no-op posture.
const launchScaffold = `
# ── launch: per-project Claude Code launcher (forgectl launch) ──────────────
# Resolution: expand ~, pick the [[launch.project]] whose match is the longest
# path-prefix of the real cwd, then merge over [launch.defaults].
#   scalars (model, permission_mode, allow_danger): project wins when set
#   env: merged, project wins on key collisions
#   add_dir: concatenated and de-duplicated
# Inspect the resolved profile with:  forgectl launch which

[launch.defaults]
model           = "opus"     # claude --model value (alias or full id)
permission_mode = "plan"     # launch always starts in plan
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this

# Per-project overrides — add as many [[launch.project]] blocks as you like.
# [[launch.project]]
# match   = "~/Projects/minute"
# model   = "sonnet"
# env     = { OTEL_EXPORTER = "otlp" }
# add_dir = ["~/Projects/minute/shared"]
`

func newLaunchInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold the [launch] section into config.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromClaunch, _ := cmd.Flags().GetBool("from-claunch"); fromClaunch {
				return runClaunchImport(cmd)
			}
			path, err := config.ConfigPath()
			if err != nil {
				return err
			}
			if data, err := os.ReadFile(path); err == nil {
				if hasLaunchSection(data) {
					return fmt.Errorf("config already has a [launch] section at %s (edit it with `forgectl launch edit`)", path)
				}
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("read config %s: %w", path, err)
			}
			if err := appendToConfig(path, launchScaffold); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added a [launch] section to %s\n", path)
			return nil
		},
	}
	cmd.Flags().Bool("from-claunch", false, "import an existing ~/.config/claunch/claunch.conf into config.toml")
	return cmd
}

// runClaunchImport migrates an existing legacy ~/.config/claunch/claunch.conf
// into config.toml's [launch] section, so `forgectl launch` stops falling
// back to the legacy file. It refuses to run when config.toml already has a
// [launch] section — import once, then edit config.toml directly.
func runClaunchImport(cmd *cobra.Command) error {
	slog.Debug("Preparing to import legacy claunch.conf into config.toml.")

	path, err := config.ConfigPath()
	if err != nil {
		slog.Error("Failed to determine config path.", "error", err)
		return err
	}
	if data, err := os.ReadFile(path); err == nil {
		if hasLaunchSection(data) {
			slog.Warn("Config already has a [launch] section.", "path", path)
			return fmt.Errorf("config already has a [launch] section at %s (edit it with `forgectl launch edit`); refusing to overwrite an existing launch profile", path)
		}
	} else if !os.IsNotExist(err) {
		slog.Warn("Failed to read config.", "path", path, "error", err)
		return fmt.Errorf("read config %s: %w", path, err)
	}

	lc, ok := config.LoadLegacyLaunch()
	if !ok {
		if verr := config.ValidateLegacyLaunch(); verr != nil {
			slog.Warn("Legacy claunch.conf is malformed.", "error", verr)
			return fmt.Errorf("legacy claunch.conf is malformed, not importing: %w", verr)
		}
		legacyPath, lerr := config.LegacyLaunchPath()
		if lerr != nil {
			slog.Error("Failed to determine legacy claunch path.", "error", lerr)
			return lerr
		}
		slog.Warn("No legacy claunch.conf found.", "path", legacyPath)
		return fmt.Errorf("no legacy claunch.conf found at %s", legacyPath)
	}
	legacyPath, err := config.LegacyLaunchPath()
	if err != nil {
		slog.Error("Failed to determine legacy claunch path.", "error", err)
		return err
	}
	if lc.IsZero() {
		slog.Warn("Legacy claunch.conf is empty.", "path", legacyPath)
		return fmt.Errorf("legacy claunch.conf at %s has no [defaults] or [[project]] to import", legacyPath)
	}
	slog.Debug("Loaded legacy claunch.conf.", "path", legacyPath, "project_count", len(lc.Projects))

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(struct {
		Launch config.LaunchConfig `toml:"launch"`
	}{Launch: lc}); err != nil {
		slog.Error("Failed to encode imported launch config.", "error", err)
		return fmt.Errorf("encode imported launch config: %w", err)
	}
	header := fmt.Sprintf("\n# ── launch: imported from %s (forgectl launch init --from-claunch) ──\n", legacyPath)

	if err := appendToConfig(path, header+buf.String()); err != nil {
		slog.Error("Failed to append imported config to config.toml.", "path", path, "error", err)
		return err
	}

	slog.Info("Successfully imported legacy claunch.conf.", "legacy_path", legacyPath, "config_path", path, "project_count", len(lc.Projects))
	fmt.Fprintf(cmd.OutOrStdout(), "Imported %d launch profile(s) from %s into %s\n", len(lc.Projects), legacyPath, path)
	return nil
}

// appendToConfig appends content to the config.toml at path, creating the
// parent directory and the file if absent. Both `launch init` and its
// `--from-claunch` importer append a TOML block this way, preserving any
// sections already in the file.
func appendToConfig(path, content string) error {
	slog.Debug("Preparing to append to config.toml.", "path", path, "content_length", len(content))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Error("Failed to create config directory.", "path", filepath.Dir(path), "error", err)
		return fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Error("Failed to open config file.", "path", path, "error", err)
		return fmt.Errorf("open config: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		slog.Error("Failed to write to config file.", "path", path, "error", err)
		return fmt.Errorf("write config: %w", err)
	}
	if err := f.Close(); err != nil {
		slog.Error("Failed to close config file.", "path", path, "error", err)
		return fmt.Errorf("close config: %w", err)
	}
	slog.Debug("Successfully appended to config.toml.", "path", path)
	return nil
}

// hasLaunchSection reports whether data already defines a [launch] table, by
// matching real TOML headers rather than a loose substring (which would also
// fire on comments, string values, or an unrelated [launcher] table).
func hasLaunchSection(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "[launch]" || strings.HasPrefix(t, "[launch.") || strings.HasPrefix(t, "[[launch.") {
			return true
		}
	}
	return false
}
