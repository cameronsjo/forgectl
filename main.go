// Command forgectl is a personal dev-experience CLI for the headless
// workbench. Bare invocation opens a TUI menu (thumb mode); typed verbs drive
// it directly (power mode). See internal/cli and internal/tui.
package main

import (
	"context"
	"os"

	"github.com/charmbracelet/fang"

	"github.com/cameronsjo/forgectl/internal/cli"
	"github.com/cameronsjo/forgectl/internal/meta"
)

func main() {
	ctx := context.Background()
	root := cli.NewRoot()
	// fang owns version rendering; feed it our ldflags-injected values rather
	// than relying on its build-info fallback (which is empty under `go run`).
	if err := fang.Execute(ctx, root,
		fang.WithVersion(meta.Version),
		fang.WithCommit(meta.Commit),
	); err != nil {
		os.Exit(1)
	}
}
