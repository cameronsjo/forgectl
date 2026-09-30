package cli

import (
	"slices"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestSurfaceLaunchEnvironment_StripsOnlyTheClaudeChildMarker pins Cameron's
// ruling for forgectl#363: a new surface is independent, so it must not inherit
// Claude's child-session classification. Every other inherited value remains
// byte-for-byte and in order, including similarly named keys.
func TestSurfaceLaunchEnvironment_StripsOnlyTheClaudeChildMarker(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_CHILD_SESSION_DETAIL=keep-me",
		"AUTH_TOKEN=preserved",
		"CLAUDE_CODE_CHILD_SESSION=duplicate-must-also-go",
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDE_CODE_CHILD_SESSION_DETAIL=keep-me",
		"AUTH_TOKEN=preserved",
	}

	got := surfaceLaunchEnvironment(base)
	if !slices.Equal(got, want) {
		t.Fatalf("surface environment = %q, want %q", got, want)
	}

	// The filter must not mutate the process snapshot it was handed.
	if base[1] != "CLAUDE_CODE_CHILD_SESSION=1" {
		t.Errorf("input environment was mutated: %q", base)
	}
}

func TestSurfaceLaunchEnvironment_WithoutMarkerIsUnchanged(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin", "HOME=/Users/example"}
	got := surfaceLaunchEnvironment(base)
	if !slices.Equal(got, base) {
		t.Fatalf("surface environment = %q, want unchanged %q", got, base)
	}
}

// TestSurfaceInvocationRequest_StdoutIsATerminal is #816: the surface launches
// the harness into a TTY pane, so the request says its stdout is a terminal
// rather than leaving the zero value, which means "not a terminal" and lets
// --output-format alone select the print posture (forgectl#795).
//
// Mutation: drop StdoutTerminal from surfaceInvocationRequest and this fails.
func TestSurfaceInvocationRequest_StdoutIsATerminal(t *testing.T) {
	req := surfaceInvocationRequest(config.LaunchConfig{}, "/p", nil, nil)
	if !req.StdoutTerminal {
		t.Error("surface request leaves StdoutTerminal false; the harness runs in a TTY pane")
	}
	if req.Args != nil || req.CWD != "/p" {
		t.Errorf("request = %+v, want no args and CWD /p", req)
	}
}
