package cli

import (
	"context"
	"os"

	"github.com/charmbracelet/fang"

	"github.com/cameronsjo/forgectl/internal/meta"
)

// Execute is the binary's entrypoint. It normalizes argv (forgiveness layer)
// then hands off to fang for styled help/errors/version. M5 extends this with
// the bare-invoke → TUI and unknown-verb → TUI fallthroughs.
func Execute(ctx context.Context) error {
	root := NewRoot()
	root.SetArgs(normalizeArgs(os.Args[1:]))
	return fang.Execute(ctx, root,
		fang.WithVersion(meta.Version),
		fang.WithCommit(meta.Commit),
	)
}
