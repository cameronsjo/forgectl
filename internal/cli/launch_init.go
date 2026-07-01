package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	return &cobra.Command{
		Use:   "init",
		Short: "Scaffold the [launch] section into config.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create config directory: %w", err)
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("open config: %w", err)
			}
			if _, err := f.WriteString(launchScaffold); err != nil {
				_ = f.Close()
				return fmt.Errorf("write launch scaffold: %w", err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added a [launch] section to %s\n", path)
			return nil
		},
	}
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
