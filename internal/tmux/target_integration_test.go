//go:build unix

package tmux

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// requireTmuxEnv, when set to any non-empty value, turns this file's skips into
// failures. CI sets it; a developer box without tmux does not, so the tests stay
// skippable locally.
const requireTmuxEnv = "FORGECTL_REQUIRE_TMUX"

// skipOrFail is the loud-skip gate. These tests are the ONLY place the tmux
// target grammar is measured rather than described, so where they are meant to
// run, a skip and a pass must not look alike — an environment that lost tmux
// would otherwise retire the guarantee silently.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireTmuxEnv) != "" {
		t.Fatalf("%s=1 but the real-tmux grammar tests cannot run: "+format,
			append([]any{requireTmuxEnv}, args...)...)
	}
	t.Skipf(format, args...)
}

// isolatedTmux stands up a private tmux server on its own socket and returns a
// Client over it. Nothing here can reach the operator's real server: TMUX is
// cleared so we are not treated as being inside a client, and TMUX_TMPDIR moves
// the default socket into a temp root.
//
// Not t.TempDir(): macOS caps a Unix socket path at ~104 bytes and t.TempDir()
// embeds the full test name, so tmux's <root>/tmux-<uid>/default overflows it.
func isolatedTmux(t *testing.T) (*Client, internalexec.Runner, string) {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		skipOrFail(t, "tmux not installed")
	}
	runner := internalexec.OSRunner{}
	versionOut, err := runner.Run(context.Background(), tmuxBin, "-V")
	if err != nil {
		skipOrFail(t, "tmux -V: %v", err)
	}
	major, minor, _, err := parseTmuxVersion(versionOut)
	if err != nil || major < 2 || (major == 2 && minor < 2) {
		skipOrFail(t, "tmux 2.2+ required: %q", versionOut)
	}
	root, err := os.MkdirTemp("/tmp", "f237-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", root)
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), tmuxBin, "kill-server") })
	return New(runner, WithBins(tmuxBin, "sesh")), runner, tmuxBin
}

// TestNewWindowTargetResolutionIsolated is the forgectl#237 reproduction, run
// against a real tmux rather than a description of one.
//
// With ONE session present — `forgectl-review` — and no session named
// `forgectl`, it measures what each spelling of `new-window -t` actually does.
// The table this pins was first measured by hand on tmux 3.7b; running it here
// means a future tmux that changes the rules breaks the build instead of
// quietly restoring the bug. CI installs tmux and sets FORGECTL_REQUIRE_TMUX=1
// so that claim holds there — without both, this test skips and a skip is
// indistinguishable from a pass.
func TestNewWindowTargetResolutionIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()

	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "forgectl-review", "sleep 30"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// The three UNSAFE spellings. Each one lands a window in forgectl-review —
	// a session the caller did not name — because tmux falls through to prefix
	// resolution. The leading "=" alone does NOT prevent it: without the
	// session/window delimiter the operand is still read as a window target.
	for _, target := range []string{"forgectl", "forgectl:", "=forgectl"} {
		before := windowCount(t, c, "forgectl-review")
		if _, err := runner.Run(ctx, tmuxBin, "new-window", "-d", "-t", target, "sleep 30"); err != nil {
			t.Fatalf("new-window -t %q: unexpected refusal %v — if tmux stopped "+
				"prefix-matching here, the unsafe table below is stale and needs re-measuring", target, err)
		}
		if after := windowCount(t, c, "forgectl-review"); after != before+1 {
			t.Fatalf("new-window -t %q: forgectl-review windows %d → %d, want +1 "+
				"(this spelling is documented as landing in the sibling)", target, before, after)
		}
	}

	// The SAFE name spelling: exact-match modifier AND the trailing colon. tmux
	// refuses, because no session is literally named forgectl.
	before := windowCount(t, c, "forgectl-review")
	if _, err := runner.Run(ctx, tmuxBin, "new-window", "-d", "-t", "=forgectl:", "sleep 30"); err == nil {
		t.Fatal(`new-window -t "=forgectl:" succeeded; it must refuse when no session is named exactly forgectl`)
	}
	if after := windowCount(t, c, "forgectl-review"); after != before {
		t.Fatalf("the refused =forgectl: still created a window in forgectl-review (%d → %d)", before, after)
	}

	// What forgectl actually emits: the native session id plus the trailing
	// colon. Resolution is by identity, so no name — exact, prefix, or glob —
	// participates at all.
	session, err := c.EnsureSession(ctx, "forgectl", "")
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if session.Name != "forgectl" {
		t.Fatalf("EnsureSession returned %q; the prefix sibling answered for it", session.Name)
	}
	target, err := NewWindowSessionTarget(session.ID)
	if err != nil {
		t.Fatalf("NewWindowSessionTarget: %v", err)
	}
	siblingBefore := windowCount(t, c, "forgectl-review")
	exactBefore := windowCount(t, c, "forgectl")
	if _, err := runner.Run(ctx, tmuxBin, "new-window", "-d", "-t", target, "-n", "landed", "sleep 30"); err != nil {
		t.Fatalf("new-window -t %q: %v", target, err)
	}
	if got := windowCount(t, c, "forgectl-review"); got != siblingBefore {
		t.Errorf("the sibling gained a window (%d → %d); the native-id target must not reach it", siblingBefore, got)
	}
	if got := windowCount(t, c, "forgectl"); got != exactBefore+1 {
		t.Errorf("forgectl windows %d → %d, want +1", exactBefore, got)
	}
}

// TestExactActionsIsolated drives resolve → act by native id against a real
// server, including the destructive paths, and proves the reported
// rename/kill reach onto a sibling is closed.
func TestExactActionsIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()

	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "forgectl-review", "sleep 30"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// The issue's exact reproduction: `rename-session -t forgectl` renamed
	// forgectl-review, and `kill-session -t <renamed>` then killed it. Neither
	// can happen now, because resolution never reaches tmux's grammar.
	absent := SessionIdentity{
		Generation: ServerGeneration{Selector: c.currentSelector(), PID: "1", StartTime: "1"},
		ID:         "$99", Name: "forgectl",
	}
	if _, err := c.ResolveSessionExact(ctx, "forgectl"); err == nil {
		t.Fatal("resolved a session named forgectl; the prefix sibling answered for it")
	}
	if err := c.RenameSession(ctx, absent, "taken"); err == nil {
		t.Fatal("renamed a session that does not exist")
	}
	if err := c.KillSession(ctx, absent); err == nil {
		t.Fatal("killed a session that does not exist")
	}
	if _, err := c.ResolveSessionExact(ctx, "forgectl-review"); err != nil {
		t.Fatalf("the sibling was renamed or killed out from under us: %v", err)
	}

	// Now the same operations against a session that really is there.
	created, err := c.CreateSession(ctx, "forgectl", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := ValidateSessionID(created.ID); err != nil {
		t.Fatalf("real tmux returned an id this package rejects: %v", err)
	}

	// The duplicate-session classifier, against the real diagnostic. A pinned
	// string is only worth as much as the tmux that produces it.
	if _, err := c.CreateSession(ctx, "forgectl", ""); err == nil {
		t.Fatal("created a second session named forgectl")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create error = %v, want the typed duplicate classification", err)
	}
	adopted, err := c.EnsureSession(ctx, "forgectl", "")
	if err != nil {
		t.Fatalf("EnsureSession over an existing session: %v", err)
	}
	if adopted.ID != created.ID {
		t.Fatalf("EnsureSession returned %s, want the existing %s", adopted.ID, created.ID)
	}

	if err := c.RenameSession(ctx, created, "forgectl-renamed"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	// The identity still names the same object after the rename — that is the
	// point of targeting ids.
	back, err := c.RevalidateSession(ctx, created)
	if err != nil {
		t.Fatalf("revalidate after rename: %v", err)
	}
	if back.Name != "forgectl-renamed" {
		t.Fatalf("revalidated name = %q, want the current name", back.Name)
	}
	if err := c.KillSession(ctx, created); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if _, err := c.ResolveSessionExact(ctx, "forgectl-review"); err != nil {
		t.Fatalf("the sibling did not survive the kill: %v", err)
	}
}

func windowCount(t *testing.T, c *Client, session string) int {
	t.Helper()
	windows, err := c.ListWindows(context.Background())
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	n := 0
	for _, w := range windows {
		if w.Session == session {
			n++
		}
	}
	return n
}
