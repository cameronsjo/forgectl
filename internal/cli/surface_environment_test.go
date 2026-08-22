package cli

import (
	"slices"
	"testing"
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
