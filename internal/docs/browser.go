package docs

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// ErrNoCMUXWorkspace means the caller did not provide the workspace identity
// cmux requires to place a browser pane without guessing or changing focus.
var ErrNoCMUXWorkspace = errors.New("cmux workspace is not available")

// OpenBrowser launches url in the system browser: `open` on macOS, `xdg-open`
// elsewhere. Mirrors internal/bench's openCommand/Open pattern (same
// GOOS-keyed opener, same delegation through exec.Runner) — the docs module
// doesn't reimplement it because the two commands' browser-open needs are
// otherwise unrelated (bench opens a fixed set of named UIs; docs opens
// whatever loopback address it just bound), so sharing a helper wasn't worth
// a cross-module dependency for one GOOS switch.
func OpenBrowser(ctx context.Context, runner exec.Runner, url string) error {
	return runner.RunInteractive(ctx, openCommand(), url)
}

// OpenCMUXPreview opens url in a new right-hand browser pane in workspaceID.
// The explicit workspace prevents a concurrent cmux session from receiving the
// pane, and --focus false leaves the invoking terminal in control of the
// foreground docs server (where Ctrl-C owns shutdown).
func OpenCMUXPreview(ctx context.Context, runner exec.Runner, workspaceID, url string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return ErrNoCMUXWorkspace
	}
	_, err := runner.Run(ctx, "cmux", "new-pane",
		"--workspace", workspaceID,
		"--type", "browser",
		"--direction", "right",
		"--url", url,
		"--focus", "false",
		"--json",
	)
	return err
}

func openCommand() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}
