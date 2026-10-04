package cli

import (
	"slices"
	"strings"
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

// TestSurfaceLaunchEnvironment_StripsTheLaunchersHerdrPane is the T0 finding
// I1: a worker launched from a herdr pane must not carry that pane's ids, or
// its agent-state reports land on the launcher's pane. The server path stays,
// because launcher and worker share the server.
func TestSurfaceLaunchEnvironment_StripsTheLaunchersHerdrPane(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"HERDR_PANE_ID=w83:p5",
		"HERDR_TAB_ID=w83:t5",
		"HERDR_WORKSPACE_ID=w83",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
		"HERDR_ENV=1",
	}
	want := []string{"PATH=/usr/bin:/bin", "HERDR_SOCKET_PATH=/tmp/herdr.sock", "HERDR_ENV=1"}
	if got := surfaceLaunchEnvironment(base); !slices.Equal(got, want) {
		t.Fatalf("surface environment = %q, want %q", got, want)
	}
}

// TestPaneIdentityEnv carries the new pane's own ids, and only those, and
// never overrides a value the invocation already set.
func TestPaneIdentityEnv(t *testing.T) {
	own := map[string]string{
		"HERDR_PANE_ID":      "w90:p1",
		"HERDR_TAB_ID":       "w90:t1",
		"HERDR_WORKSPACE_ID": "w90",
		"SECRET":             "must-not-cross",
	}
	got := paneIdentityEnv([]string{"PATH=/bin"}, func(k string) string { return own[k] })
	want := []string{"PATH=/bin", "HERDR_PANE_ID=w90:p1", "HERDR_TAB_ID=w90:t1", "HERDR_WORKSPACE_ID=w90"}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}

	got = paneIdentityEnv([]string{"HERDR_PANE_ID=set"}, func(k string) string { return own[k] })
	if got[0] != "HERDR_PANE_ID=set" || slices.Contains(got, "HERDR_PANE_ID=w90:p1") {
		t.Errorf("an invocation's own value was overridden: %q", got)
	}

	for _, bad := range []string{"", "w1;rm", "w1\x1b[0m", strings.Repeat("a", 65)} {
		got := paneIdentityEnv(nil, func(string) string { return bad })
		if len(got) != 0 {
			t.Errorf("value %q crossed: %q", bad, got)
		}
	}
}
