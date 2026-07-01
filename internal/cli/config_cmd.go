package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
)

func newConfigCmd(cfg config.Config) *cobra.Command {
	return &cobra.Command{
		Use:     "config",
		Short:   "Show the active configuration and config file path",
		Aliases: []string{"cfg"},
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Debug("Preparing to display configuration.")
			out := cmd.OutOrStdout()

			path, err := config.ConfigPath()
			if err != nil {
				fmt.Fprintf(out, "config file: (unavailable: %s)\n\n", err)
				slog.Warn("Config path unavailable.", "error", err)
			} else if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				fmt.Fprintf(out, "config file: %s (not found — using defaults)\n\n", path)
				slog.Debug("Config file not found, using defaults.", "path", path)
			} else {
				fmt.Fprintf(out, "config file: %s\n\n", path)
				slog.Debug("Config file loaded.", "path", path)
			}

			logLevel := cfg.LogLevel
			if logLevel == "" {
				logLevel = "off"
			}

			fmt.Fprintf(out, "  no_icons   %v\n", cfg.NoIcons)
			fmt.Fprintf(out, "  log_level  %s\n", logLevel)
			fmt.Fprintf(out, "  log_file   %s\n", config.ResolvedLogPath(cfg.LogFile))

			// Resolved launch defaults (built-in fallbacks applied). Per-project
			// overrides are shown by `forgectl launch which`.
			ld := launch.DefaultsProfile(cfg.Launch)
			// Resolve the actual exec target so the display honors the same
			// precedence as launch (FORGECTL_CLAUDE_BIN > binary_path > PATH).
			claudeBin, cerr := launch.ClaudePath(cfg.Launch.Defaults)
			if cerr != nil {
				claudeBin = fmt.Sprintf("(unresolved: %s)", cerr)
			}
			fmt.Fprintf(out, "\n  launch.model         %s\n", ld.Model)
			fmt.Fprintf(out, "  launch.permission    %s\n", ld.PermissionMode)
			fmt.Fprintf(out, "  launch.allow_danger  %v\n", ld.AllowDanger)
			fmt.Fprintf(out, "  launch.claude_bin    %s\n", claudeBin)
			fmt.Fprintf(out, "  launch.projects      %d configured\n", len(cfg.Launch.Projects))

			slog.Info("Successfully displayed configuration.", "no_icons", cfg.NoIcons, "log_level", logLevel)
			return nil
		},
	}
}
