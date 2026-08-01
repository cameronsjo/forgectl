package pr

// Test plan for admission.go
//
// LiveReviews (Classification: fail-safe tmux window count, ListWindows-only)
//   [x] Happy: 2 "pr-*" windows + 1 unrelated window, all in the client's
//       session → counts only the 2 prefixed windows
//   [x] Unhappy: list-windows errors (not "no server running") → (0, false) —
//       the caller MUST treat this as unreadable, never as a genuine zero
//   [x] Boundary: "pr-*" windows that exist in a DIFFERENT session are not
//       counted
//   [x] Happy: no windows at all (including "no server running") → genuine
//       zero, ok=true
//   [x] REGRESSION (fixes the fail-open bug the prior HasSession-gated
//       implementation had): a broken/missing tmux binary makes list-windows
//       itself error → (0, false), NOT a silently-trusted (0, true). Verified
//       by temporarily reverting to the old HasSession-based body: against
//       that code this exact fixture returns (0, true) — see the admission.go
//       doc comment for the full before/after trace.
//   [x] DOCUMENTED RESIDUAL, not closed by this file: a sibling session whose
//       name prefixes the client's tmux session (e.g. "forgectl-review" when
//       the client watches "forgectl") still reads as a genuine (0, true) —
//       empirically IDENTICAL to the prior HasSession-gated implementation,
//       because ListWindows's exact-match filtering was always what produced
//       the undercount, not HasSession's fuzzy existence check. The window
//       itself lands in the wrong session because Launch's own
//       `new-window -t c.tmuxSession` argument is an unqualified session
//       name subject to the same tmux fuzzy `-t` resolution — a separate bug
//       this admission-gate change does not touch. This test exists so a
//       future fix to Launch's targeting has a regression to flip green.
//
// Admit (Classification: concurrency gate, fail-closed, single resolution)
//   [x] Happy: no windows yet → genuine zero, full cap free, resolved max and
//       live both reported
//   [x] Boundary: live count already at/above max → 0 free, still ok
//   [x] Boundary: a non-positive cfgMax resolves to DefaultMaxConcurrentReviews
//       (Admit is now the ONLY place that resolves cfgMax — callers pass the
//       raw config value through)

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// winRow builds one list-windows -a fixture line: session, index "1", name,
// active "0", panes "1" — joined on tmux's field separator.
func winRow(session, name string) string {
	return strings.Join([]string{session, "1", name, "0", "1"}, "\x1f")
}

// listWindowsFake fakes only `tmux list-windows -a` with rows; every other
// tmux call (including has-session, kept only for callers that still probe
// it) succeeds as a no-op.
func listWindowsFake(rows ...string) *exec.FakeRunner {
	out := strings.Join(rows, "\n")
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return out, nil
		}
		return "", nil
	}}
}

func TestLiveReviews_CountsPrefixedWindowsInSession(t *testing.T) {
	fake := listWindowsFake(
		winRow("forgectl", "pr-a-b-1"),
		winRow("forgectl", "pr-c-d-2"),
		winRow("forgectl", "shell"),
	)
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 2 {
		t.Fatalf("LiveReviews() = (%d, %v), want (2, true)", n, ok)
	}
}

func TestLiveReviews_ListWindowsErrors(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			// TRAP: this string must NOT contain "no server running" — isNoServer
			// (internal/tmux/sessions.go) would convert that to (nil, nil) and this
			// test would assert the wrong thing.
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, false)", n, ok)
	}
}

func TestLiveReviews_IgnoresOtherSessions(t *testing.T) {
	fake := listWindowsFake(
		winRow("other-session", "pr-a-b-1"),
		winRow("other-session", "pr-c-d-2"),
	)
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, true)", n, ok)
	}
}

func TestLiveReviews_NoWindows_GenuineZero(t *testing.T) {
	fake := listWindowsFake() // empty output — no server, or a server with nothing live
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, true)", n, ok)
	}
}

// TestLiveReviews_BrokenTmux_FailsClosed is the actual behavioral regression
// this file's admission.go change produces: a broken/missing tmux binary now
// surfaces as unreadable (ok=false) rather than a trusted genuine zero. Under
// the PRIOR HasSession-gated implementation this exact fixture returned
// (0, true) — HasSession's own Run() error was swallowed into "session
// absent," short-circuiting before ListWindows ever ran, so a caller admitted
// the full cap and PrepareMany cloned every selected PR before Launch's own
// tmux call finally failed. Verified by hand against the reverted body; see
// the LiveReviews doc comment for the trace.
func TestLiveReviews_BrokenTmux_FailsClosed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New(`exec: "tmux": executable file not found in $PATH`)
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, false) — a broken tmux binary must fail closed, not read as a trusted zero", n, ok)
	}
}

// TestLiveReviews_SiblingPrefixSession_KnownResidual documents, rather than
// closes, the CRITICAL trigger: a sibling session whose name PREFIXES the
// client's tmux session (e.g. a worktree session named after its directory,
// per internal/projects/projects.go's filepath.Base(dir) naming) is where
// Launch's own `new-window -t c.tmuxSession` — an unqualified session name,
// subject to tmux's exact→fnmatch→prefix `-t` resolution — deposits review
// windows whenever no session literally named c.tmuxSession exists yet.
//
// This test asserts the CURRENT (unresolved) behavior: LiveReviews reads a
// genuine (0, true), identical to the pre-fix HasSession-gated
// implementation, because the exact-match filter below was always what
// produced the undercount — HasSession's fuzzy existence check never gated
// the count itself. Closing this requires a fix to Launch's own tmux target
// (qualifying it, or verifying exact session existence before dispatch),
// which is out of scope for this admission-only change. Flip this test's
// assertion (want live windows counted, or ok=false on ambiguity — whichever
// that fix decides) once Launch's targeting is fixed.
func TestLiveReviews_SiblingPrefixSession_KnownResidual(t *testing.T) {
	// Only "forgectl-review" is alive; the client watches "forgectl", which
	// has no literal session — mirrors the live-verified repro exactly.
	fake := listWindowsFake(winRow("forgectl-review", "pr-o-r-1"))
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, true) — this is the KNOWN, undischarged residual; "+
			"if this now fails, Launch's tmux targeting was fixed and this test's assertion should flip", n, ok)
	}
}

func TestAdmit_NoWindowsYet_FullCapFree(t *testing.T) {
	fake := listWindowsFake()
	c := New(fake, WithTmuxSession("forgectl"))
	max, live, free, ok := c.Admit(context.Background(), 4)
	if !ok || max != 4 || live != 0 || free != 4 {
		t.Fatalf("Admit(ctx, 4) = (max=%d, live=%d, free=%d, ok=%v), want (4, 0, 4, true)", max, live, free, ok)
	}
}

func TestAdmit_LiveAtOrAboveMax(t *testing.T) {
	fake := listWindowsFake(
		winRow("forgectl", "pr-a-b-1"),
		winRow("forgectl", "pr-c-d-2"),
	)
	c := New(fake, WithTmuxSession("forgectl"))
	max, live, free, ok := c.Admit(context.Background(), 2)
	if !ok || max != 2 || live != 2 || free != 0 {
		t.Fatalf("Admit(ctx, 2) = (max=%d, live=%d, free=%d, ok=%v), want (2, 2, 0, true)", max, live, free, ok)
	}
}

func TestAdmit_NonPositiveCfgMaxDefaults(t *testing.T) {
	fake := listWindowsFake()
	c := New(fake, WithTmuxSession("forgectl"))
	for _, cfgMax := range []int{0, -1, -100} {
		max, live, free, ok := c.Admit(context.Background(), cfgMax)
		if !ok || max != DefaultMaxConcurrentReviews || live != 0 || free != DefaultMaxConcurrentReviews {
			t.Errorf("Admit(ctx, %d) = (max=%d, live=%d, free=%d, ok=%v), want (%d, 0, %d, true)",
				cfgMax, max, live, free, ok, DefaultMaxConcurrentReviews, DefaultMaxConcurrentReviews)
		}
	}
}

func TestAdmit_ListWindowsErrors_FailsClosed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	max, live, free, ok := c.Admit(context.Background(), 4)
	if ok || live != 0 || free != 0 || max != 4 {
		t.Fatalf("Admit(ctx, 4) = (max=%d, live=%d, free=%d, ok=%v), want (4, 0, 0, false)", max, live, free, ok)
	}
}
