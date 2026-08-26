//go:build darwin

// Test plan for file_darwin.go / the CopyFile/CopyImage half of clip.go
// (Classification: ops layer)
//
// Client.CopyFile (darwin)
//
//	[x] Happy: shells `osascript -e <script> -- <path>` with path as a
//	    trailing positional argument, never interpolated into the script
//	    source
//
// Client.CopyImage (darwin)
//
//	[x] Happy: shells osascript with the path and the AppleScript image
//	    class resolved from the extension (.png -> PNGf, .jpg -> JPEG, ...)
//	[x] Unhappy: every class imageClassForExt (clip.go) can produce has a
//	    matching branch in copyImageScript — the two files are edited
//	    separately, and a class with no branch falls through to a silent
//	    no-op rather than an error (see the script's own comment)
//	[x] Happy: a real pasteboard smoke test for CopyFile, opt-in via
//	    FORGECTL_PASTEBOARD_TESTS=1, best-effort restores the prior
//	    clipboard contents
package clip

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestCopyImageScript_HasABranchForEveryMappedClass guards the coupling the
// security review flagged: imageClassForExt (clip.go) and copyImageScript's
// if/else-if chain (file_darwin.go) are two separately-edited files that
// must agree, and copyImageScript's `else error` only fires for a class no
// existing branch names. Adding an entry to imageClassForExt without a
// matching `else if theClass is "..."` branch would otherwise ship a class
// value that silently no-ops instead of erroring — this test catches that
// class of drift by asserting every mapped class appears as a quoted
// AppleScript literal in the script source.
func TestCopyImageScript_HasABranchForEveryMappedClass(t *testing.T) {
	for _, class := range imageClassForExt {
		want := fmt.Sprintf("%q", class)
		if !strings.Contains(copyImageScript, want) {
			t.Errorf("copyImageScript has no branch for class %s (from imageClassForExt) — add `else if theClass is %s`", class, want)
		}
	}
}

func TestCopyFile_ShellsOsascriptWithPathAsArgv(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("darwin"))

	if err := c.CopyFile(context.Background(), "/tmp/report.pdf"); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	call := fake.Last()
	if call.Name != "osascript" {
		t.Errorf("call.Name = %q, want %q", call.Name, "osascript")
	}
	if len(call.Args) == 0 || call.Args[len(call.Args)-1] != "/tmp/report.pdf" {
		t.Errorf("call.Args = %v, want path as the trailing argv element", call.Args)
	}
	// The path must arrive as a positional argument, not interpolated into
	// the -e script source — otherwise a path containing AppleScript syntax
	// (quotes, semicolons) could escape into the script itself.
	for _, arg := range call.Args {
		if arg != "/tmp/report.pdf" && strings.Contains(arg, "/tmp/report.pdf") {
			t.Errorf("path leaked into script source: %q", arg)
		}
	}
}

func TestCopyImage_ResolvesExtensionToAppleScriptClass(t *testing.T) {
	cases := []struct {
		path      string
		wantClass string
	}{
		{"/tmp/a.png", "PNGf"},
		{"/tmp/a.PNG", "PNGf"},
		{"/tmp/a.tif", "TIFF"},
		{"/tmp/a.tiff", "TIFF"},
		{"/tmp/a.jpg", "JPEG"},
		{"/tmp/a.jpeg", "JPEG"},
		{"/tmp/a.gif", "GIFf"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := New(fake, WithGOOS("darwin"))

			if err := c.CopyImage(context.Background(), tc.path); err != nil {
				t.Fatalf("CopyImage: %v", err)
			}

			call := fake.Last()
			if call.Name != "osascript" {
				t.Errorf("call.Name = %q, want %q", call.Name, "osascript")
			}
			found := false
			for _, arg := range call.Args {
				if arg == tc.wantClass {
					found = true
				}
			}
			if !found {
				t.Errorf("call.Args = %v, want the %q class among them", call.Args, tc.wantClass)
			}
		})
	}
}

// TestCopyFile_RealPasteboard_Smoke exercises the real osascript pasteboard
// path end to end. It writes a scratch file, copies a file reference to it,
// and reads the pasteboard back via `osascript -e "the clipboard as «class
// furl»"` to confirm a file URL landed. t.Cleanup best-effort restores
// whatever text `pbpaste` reported beforehand — best-effort because
// `exec.Runner.Run` trims trailing newlines, so a prior clipboard value
// ending in one or more newlines is NOT restored byte-for-byte, and a
// SIGKILL or a `-timeout` panic skips the cleanup entirely, leaving the
// file reference on the developer's actual clipboard.
//
// Opt-in via FORGECTL_PASTEBOARD_TESTS=1 (unset by default) rather than an
// opt-out: this test reads and overwrites the real system pasteboard, which
// a plain `go test ./...` on a darwin dev machine should not do implicitly.
// It additionally skips when the session has no accessible pasteboard (no
// login window session, e.g. a bare CI runner) — osascript's clipboard
// calls fail loudly there rather than silently no-op, which is the signal
// this test uses to skip rather than fail.
func TestCopyFile_RealPasteboard_Smoke(t *testing.T) {
	if os.Getenv("FORGECTL_PASTEBOARD_TESTS") == "" {
		t.Skip("set FORGECTL_PASTEBOARD_TESTS=1 to exercise the real system pasteboard")
	}

	prior, priorErr := exec.OSRunner{}.Run(context.Background(), "pbpaste")

	dir := t.TempDir()
	path := filepath.Join(dir, "smoke.txt")
	if err := os.WriteFile(path, []byte("forgectl y file smoke test"), 0o600); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}

	c := New(exec.OSRunner{})
	if err := c.CopyFile(context.Background(), path); err != nil {
		t.Skipf("no accessible pasteboard in this session, skipping: %v", err)
	}

	t.Cleanup(func() {
		if priorErr == nil {
			_, _ = exec.OSRunner{}.RunWithInput(context.Background(), prior, "pbcopy")
		}
	})

	out, err := exec.OSRunner{}.Run(context.Background(), "osascript", "-e", `the clipboard as «class furl»`)
	if err != nil {
		t.Skipf("could not read back pasteboard, skipping: %v", err)
	}
	if !strings.Contains(out, filepath.Base(path)) {
		t.Errorf("pasteboard file URL = %q, want it to reference %q", out, filepath.Base(path))
	}
}
