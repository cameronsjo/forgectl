package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
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

// The launcher's Claude inbox must not reach the new surface, or a Codex or
// pi agent there resolves the launcher's mail ledger and sends as the
// coordinator (security review on #674).
func TestSurfaceLaunchEnvironment_StripsTheLaunchersInbox(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDE_CODE_MESSAGING_SOCKET=/tmp/claude/inbox.sock",
		"CLAUDE_CODE_MESSAGING_TOKEN=secret",
	}
	want := []string{"PATH=/usr/bin:/bin"}
	if got := surfaceLaunchEnvironment(base); !slices.Equal(got, want) {
		t.Fatalf("surface environment = %q, want %q", got, want)
	}
}

// TestLaunchExec_StripsTheLaunchersInbox: a harness `forgectl launch` starts
// from a Claude shell must not inherit that session's inbox, or surface send
// in it would resolve as the coordinator.
func TestLaunchExec_StripsTheLaunchersInbox(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/claude/inbox.sock")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "secret")
	for _, harness := range []string{"claude", "codex", "pi"} {
		t.Run(harness, func(t *testing.T) {
			run := launchExecProbe(t, t.TempDir(), harness, nil)
			if len(run.env) == 0 {
				t.Fatal("the exec seam saw no environment")
			}
			for _, entry := range run.env {
				key, _, _ := strings.Cut(entry, "=")
				if slices.Contains(claudeMessagingEnv, key) {
					t.Errorf("%s reached the harness", key)
				}
			}
		})
	}
}

// mail repeats launch.DrainWorkerEnv rather than import launch.
func TestMailDrainWorkerEnvMatchesLaunch(t *testing.T) {
	if mail.EnvDrainWorker != launch.DrainWorkerEnv {
		t.Fatalf("mail.EnvDrainWorker = %q, launch.DrainWorkerEnv = %q", mail.EnvDrainWorker, launch.DrainWorkerEnv)
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
	got := paneIdentityEnv(markHerdrPane([]string{"PATH=/bin"}), func(k string) string { return own[k] })
	want := []string{"PATH=/bin", "HERDR_PANE_ID=w90:p1", "HERDR_TAB_ID=w90:t1", "HERDR_WORKSPACE_ID=w90"}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}

	got = paneIdentityEnv(markHerdrPane([]string{"HERDR_PANE_ID=set"}), func(k string) string { return own[k] })
	if got[0] != "HERDR_PANE_ID=set" || slices.Contains(got, "HERDR_PANE_ID=w90:p1") {
		t.Errorf("an invocation's own value was overridden: %q", got)
	}

	for _, bad := range []string{"", "w1;rm", "w1\x1b[0m", strings.Repeat("a", 65)} {
		got := paneIdentityEnv(markHerdrPane(nil), func(string) string { return bad })
		if len(got) != 0 {
			t.Errorf("value %q crossed: %q", bad, got)
		}
	}
}

// TestPaneIdentityEnv_OnlyForHerdr is the polish finding behind the marker: a
// tmux or cmux pane can inherit stale HERDR_* values from whatever started its
// server, so without the herdr backend's mark nothing crosses — and the mark
// itself never reaches the harness.
func TestPaneIdentityEnv_OnlyForHerdr(t *testing.T) {
	stale := func(string) string { return "w1:p9" }
	if got := paneIdentityEnv([]string{"PATH=/bin"}, stale); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Errorf("an unmarked invocation gained pane ids: %q", got)
	}
	got := paneIdentityEnv(markHerdrPane([]string{"PATH=/bin"}), stale)
	for _, e := range got {
		if strings.HasPrefix(e, paneMarkerEnv+"=") {
			t.Errorf("the marker reached the harness: %q", got)
		}
	}
}

// TestSurfaceLaunchEnvironment_StripsTmuxAndCmuxPanes is forgectl#1044: the
// launcher's tmux and cmux pane ids go like herdr's, and the variables naming
// the server they share stay.
func TestSurfaceLaunchEnvironment_StripsTmuxAndCmuxPanes(t *testing.T) {
	base := []string{
		"PATH=/bin",
		"TMUX=/tmp/tmux-501/default,123,0",
		"TMUX_PANE=%3",
		"CMUX_SOCKET_PATH=/tmp/cmux.sock",
		"CMUX_WORKSPACE_ID=0F1E2D3C-0000-0000-0000-000000000001",
		"CMUX_SURFACE_ID=0F1E2D3C-0000-0000-0000-000000000002",
		"CMUX_TAB_ID=0F1E2D3C-0000-0000-0000-000000000003",
		"CMUX_PANEL_ID=0F1E2D3C-0000-0000-0000-000000000004",
		"CMUX_TERMINAL_LIFECYCLE_ID=0F1E2D3C-0000-0000-0000-000000000005",
	}
	want := []string{"PATH=/bin", "TMUX=/tmp/tmux-501/default,123,0", "CMUX_SOCKET_PATH=/tmp/cmux.sock"}
	if got := surfaceLaunchEnvironment(base); !slices.Equal(got, want) {
		t.Fatalf("surface environment = %q, want %q", got, want)
	}
}

// TestPaneIdentityEnv_PerBackend carries back the marked backend's own pane
// ids and no other manager's: a tmux pane started from inside cmux still holds
// the cmux ids of the server's starter, and those must not cross.
func TestPaneIdentityEnv_PerBackend(t *testing.T) {
	own := map[string]string{
		"TMUX_PANE":                  "%7",
		"CMUX_WORKSPACE_ID":          "ws-1",
		"CMUX_SURFACE_ID":            "sf-2",
		"CMUX_TAB_ID":                "tb-3",
		"CMUX_PANEL_ID":              "pn-4",
		"CMUX_TERMINAL_LIFECYCLE_ID": "lc-5",
		"HERDR_PANE_ID":              "w1:p1",
		"HERDR_TAB_ID":               "w1:t1",
		"HERDR_WORKSPACE_ID":         "w1",
	}
	getenv := func(k string) string { return own[k] }
	cases := map[string][]string{
		"tmux":  {"PATH=/bin", "TMUX_PANE=%7"},
		"cmux":  {"PATH=/bin", "CMUX_WORKSPACE_ID=ws-1", "CMUX_SURFACE_ID=sf-2", "CMUX_TAB_ID=tb-3", "CMUX_PANEL_ID=pn-4", "CMUX_TERMINAL_LIFECYCLE_ID=lc-5"},
		"herdr": {"PATH=/bin", "HERDR_PANE_ID=w1:p1", "HERDR_TAB_ID=w1:t1", "HERDR_WORKSPACE_ID=w1"},
		"other": {"PATH=/bin"},
	}
	for backend, want := range cases {
		if got := paneIdentityEnv(markPane([]string{"PATH=/bin"}, backend), getenv); !slices.Equal(got, want) {
			t.Errorf("%s: env = %q, want %q", backend, got, want)
		}
	}
}

// TestPaneIdentityEnv_LegacyMarker: an older launcher's herdr-only marker
// is stripped and still reads as a herdr launch.
func TestPaneIdentityEnv_LegacyMarker(t *testing.T) {
	got := paneIdentityEnv([]string{"PATH=/bin", legacyHerdrPaneMarkerEnv + "=1"}, func(k string) string {
		return map[string]string{"HERDR_PANE_ID": "w1:p1"}[k]
	})
	if want := []string{"PATH=/bin", "HERDR_PANE_ID=w1:p1"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}

// TestSurfaceInvocationRequest_StdoutIsATerminal is #816: the surface launches
// the harness into a TTY pane, so the request says its stdout is a terminal
// rather than leaving the zero value, which means "not a terminal" and lets
// --output-format alone select the print posture (forgectl#795).
//
// Mutation: drop StdoutTerminal from surfaceInvocationRequest and this fails.
func TestSurfaceInvocationRequest_StdoutIsATerminal(t *testing.T) {
	req := surfaceInvocationRequest(config.LaunchConfig{}, "/p", nil, nil, "")
	if !req.StdoutTerminal {
		t.Error("surface request leaves StdoutTerminal false; the harness runs in a TTY pane")
	}
	if req.Args != nil || req.CWD != "/p" {
		t.Errorf("request = %+v, want no args and CWD /p", req)
	}
}

// TestHcScrubCoversTheLaunchStrip pins the hc calls' environment scrub to the
// surface launch strip: every variable a launch strips as the launcher's,
// and every worker identity name forgectl sets, is one every hc call drops
// too (exec.IsLauncherSessionHandle). Adding a key to one side only fails
// here.
func TestHcScrubCoversTheLaunchStrip(t *testing.T) {
	var stripped []string
	for _, backend := range []string{"herdr", "hc", "tmux", "cmux"} {
		ids := exec.PaneIdentityEnv(backend)
		if len(ids) == 0 {
			t.Fatalf("no pane identity variables for %s", backend)
		}
		stripped = append(stripped, ids...)
	}
	stripped = append(stripped, claudeCodeChildSessionEnv)
	stripped = append(stripped, claudeMessagingEnv...)
	env := make([]string, 0, len(stripped))
	for _, k := range stripped {
		env = append(env, k+"=x")
	}
	if got := surfaceLaunchEnvironment(env); len(got) != 0 {
		t.Fatalf("the launch strip kept %v; this list no longer matches it", got)
	}
	identity := []string{mail.EnvWorker, mail.EnvLedger, mail.EnvInbox, mail.EnvDrainWorker, launch.DrainWorkerEnv}
	for _, k := range append(stripped, identity...) {
		if !exec.IsLauncherSessionHandle(k) {
			t.Errorf("%s is a launcher handle on the launch path but reaches every hc call", k)
		}
	}
	for _, k := range []string{"HC_SESSION", "HERDR_SOCKET_PATH", "TMUX", "CMUX_SOCKET_PATH", "PATH", "HOME"} {
		if exec.IsLauncherSessionHandle(k) {
			t.Errorf("%s names a shared server or the user, but hc calls drop it", k)
		}
	}
}
