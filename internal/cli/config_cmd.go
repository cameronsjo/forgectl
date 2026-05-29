package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
)

func newConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "config",
		Short:   "Show the active configuration and config file path",
		Aliases: []string{"cfg"},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			cfg := config.Load()

			path, err := config.ConfigPath()
			if err != nil {
				fmt.Fprintf(out, "config file: (unavailable: %s)\n\n", err)
			} else if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				fmt.Fprintf(out, "config file: %s (not found — using defaults)\n\n", path)
			} else {
				fmt.Fprintf(out, "config file: %s\n\n", path)
			}

			logLevel := cfg.LogLevel
			if logLevel == "" {
				logLevel = "off"
			}

			fmt.Fprintf(out, "  no_icons   %v\n", cfg.NoIcons)
			fmt.Fprintf(out, "  log_level  %s\n", logLevel)
			fmt.Fprintf(out, "  log_file   %s\n", config.ResolvedLogPath(cfg.LogFile))
			return nil
		},
	}
}
