//go:build darwin

// This file holds the osascript-backed implementations of the two pasteboard
// types pbcopy/pbpaste cannot reach (issue #401): a file reference
// (public.file-url) and decoded image data (public.png / public.tiff /
// public.jpeg / public.gif). Both are unreachable through a stdin pipe, so
// they need AppleScript's `set the clipboard to` rather than the
// pbcopy/pbpaste pair Copy/Paste use.
//
// Split into its own darwin-gated file (rather than living inline in
// clip.go, which is GOOS-agnostic) so a non-darwin build never links in the
// osascript invocation path at all — file_other.go supplies the stub that
// keeps GOOS=linux/windows builds green.
package clip

import (
	"context"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// copyFileScript puts a POSIX file reference on the pasteboard. argv[0] is
// the path — passed as an osascript positional argument (`on run argv`)
// rather than interpolated into the script source, so a path containing
// quotes, backticks, or `$(...)` can never be read as AppleScript or shell
// syntax.
const copyFileScript = `on run argv
	set thePath to item 1 of argv
	set the clipboard to (POSIX file thePath)
end run`

// copyImageScript reads the file at argv[0] and places it on the pasteboard
// decoded as argv[1]'s AppleScript image class (PNGf, TIFF, JPEG, GIFf).
// Both values arrive as positional arguments for the same reason as
// copyFileScript. The trailing `else error` is load-bearing, not decorative:
// without it, a class value matching no branch runs to `end run` and exits
// 0, leaving the pasteboard's PRIOR contents in place while CopyImage logs
// success — a silent no-op that would misreport a stale clipboard as a
// freshly-copied image. imageClassForExt (clip.go) and this branch list are
// two files that must agree; the `else error` converts any future drift
// between them into a loud osascript failure instead of a quiet one.
const copyImageScript = `on run argv
	set thePath to item 1 of argv
	set theClass to item 2 of argv
	if theClass is "PNGf" then
		set the clipboard to (read (POSIX file thePath) as «class PNGf»)
	else if theClass is "TIFF" then
		set the clipboard to (read (POSIX file thePath) as «class TIFF»)
	else if theClass is "JPEG" then
		set the clipboard to (read (POSIX file thePath) as «class JPEG»)
	else if theClass is "GIFf" then
		set the clipboard to (read (POSIX file thePath) as «class GIFf»)
	else
		error "unsupported image class: " & theClass
	end if
end run`

// copyFileReference shells `osascript` to put a file reference for path on
// the pasteboard. The error is sanitized (termsafe.Error) and the path is
// quoted (termsafe.QuotePath) rather than let through raw: *exec.CommandError
// joins its Args verbatim, which would otherwise echo the unsanitized path —
// and the whole AppleScript source — straight to the terminal.
func copyFileReference(ctx context.Context, run exec.Runner, path string) error {
	if _, err := run.Run(ctx, "osascript", "-e", copyFileScript, "--", path); err != nil {
		return fmt.Errorf("osascript: %s: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	return nil
}

// copyImageReference shells `osascript` to decode the file at path as
// imageClass and place it on the pasteboard as image data. Same
// sanitization rationale as copyFileReference.
func copyImageReference(ctx context.Context, run exec.Runner, path, imageClass string) error {
	if _, err := run.Run(ctx, "osascript", "-e", copyImageScript, "--", path, imageClass); err != nil {
		return fmt.Errorf("osascript: %s: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	return nil
}
