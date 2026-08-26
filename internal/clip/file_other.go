//go:build !darwin

// Non-darwin stub for file_darwin.go: osascript doesn't exist off macOS, so
// these keep GOOS=linux/windows builds compiling without linking in any
// osascript invocation path. Client.CopyFile/CopyImage already fail fast on
// c.goos != "darwin" before reaching these, so they exist purely as the
// build-tag counterpart — they are never expected to run.
package clip

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func copyFileReference(_ context.Context, _ exec.Runner, _ string) error {
	return errMacOSOnly
}

func copyImageReference(_ context.Context, _ exec.Runner, _, _ string) error {
	return errMacOSOnly
}
