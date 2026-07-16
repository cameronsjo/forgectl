package docs

import (
	"context"
	"runtime"

	"github.com/cameronsjo/forgectl/internal/exec"
)

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

func openCommand() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}
