package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
)

func newLaunchEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open config.toml (where the [launch] section lives) in $EDITOR",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := config.ConfigPath()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create config directory: %w", err)
			}
			name, args := editorCommand(os.Getenv("EDITOR"), path)
			ed := exec.Command(name, args...)
			ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
			return ed.Run()
		},
	}
}

// editorCommand resolves the argv to open path in $EDITOR. EDITOR may carry
// flags (e.g. "code --wait", "vim -O"), so it is split on whitespace with the
// first field as the executable. An empty OR whitespace-only value falls back to
// vi — strings.Fields("   ") is empty, so guarding on len avoids a fields[0]
// panic.
func editorCommand(editorEnv, path string) (name string, args []string) {
	fields := strings.Fields(editorEnv)
	if len(fields) == 0 {
		fields = []string{"vi"}
	}
	return fields[0], append(fields[1:], path)
}
