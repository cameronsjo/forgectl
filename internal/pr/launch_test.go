package pr

// Test plan for launch.go
//
// PostReview (Classification: SECURITY INVARIANT — no post without approval)
//   [x] Approval declined → posted=false and NO `gh pr review` argv reaches
//       the Runner
//   [x] Headless → staged only, no gate shown, no post
//   [x] Non-interactive (no TTY) → staged only, no post
//   [x] Approval granted (+TTY, not headless) → posts with exact argv
// Launch (Classification: ops layer, tmux dispatch)
//   [x] Agent A (InlineSeeded) → tmux new-window argv with claude + -p prompt
//   [x] Agent A hardened: no --allow-dangerously-skip-permissions, --permission-mode plan
//       (even though the launch default posture is AllowDanger=true)
//   [x] Agent B (BareTUIEscalation) → "not yet wired" error, NO tmux call
//   [x] Dry-run session (no workspace) → refused

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func postClient(fake *exec.FakeRunner, approve bool, tty bool) *Client {
	return New(fake,
		WithSessionsDir(os.TempDir()),
		WithApprover(func(string) (bool, error) { return approve, nil }),
		WithTTYCheck(func() bool { return tty }),
	)
}

var testSess = Session{Ref: Ref{Owner: "o", Repo: "r", Number: 9}, Workspace: "/tmp/forgectl-x"}

func TestPostReview_DeclinedDoesNotPost(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := postClient(fake, false, true)
	posted, err := c.PostReview(context.Background(), testSess, "the review", false)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if posted {
		t.Error("declined approval must not post")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no argv should reach the Runner on decline; got %+v", fake.Calls)
	}
}

func TestPostReview_HeadlessStagesOnly(t *testing.T) {
	fake := &exec.FakeRunner{}
	// approve=true, tty=true — but headless flag must still suppress the post.
	c := postClient(fake, true, true)
	posted, err := c.PostReview(context.Background(), testSess, "the review", true)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if posted || len(fake.Calls) != 0 {
		t.Errorf("headless must stage only; posted=%v calls=%+v", posted, fake.Calls)
	}
}

func TestPostReview_NoTTYStagesOnly(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := postClient(fake, true, false) // approve would say yes, but no TTY
	posted, err := c.PostReview(context.Background(), testSess, "the review", false)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if posted || len(fake.Calls) != 0 {
		t.Errorf("non-interactive must stage only; posted=%v calls=%+v", posted, fake.Calls)
	}
}

func TestPostReview_ApprovedPosts(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := postClient(fake, true, true)
	posted, err := c.PostReview(context.Background(), testSess, "the review body", false)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if !posted {
		t.Fatal("approved review should post")
	}
	last := fake.Last()
	if last.Name != "gh" {
		t.Fatalf("expected gh post; got %+v", last)
	}
	want := []string{"pr", "review", "9", "--repo", "o/r", "--comment", "--body", "the review body"}
	if !equalArgs(last.Args, want) {
		t.Errorf("post argv = %v, want %v", last.Args, want)
	}
}

func TestLaunch_InlineDispatch(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	fake := &exec.FakeRunner{}
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "claude"}

	if err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	call := fake.Last()
	if call.Name != "tmux" || call.Args[0] != "new-window" {
		t.Fatalf("expected tmux new-window; got %+v", call)
	}
	if !contains(call.Args, "pr-42") || !contains(call.Args, ws) || !contains(call.Args, claudeBin) {
		t.Errorf("tmux argv missing window/workspace/claude: %v", call.Args)
	}
	if !contains(call.Args, "-p") || !contains(call.Args, reviewPrompt) {
		t.Errorf("tmux argv missing seeded -p prompt: %v", call.Args)
	}
	if !contains(call.Args, "--") {
		t.Errorf("tmux argv missing -- terminator before claude: %v", call.Args)
	}
	// SECURITY: the review agent must launch HARDENED even though the launch
	// default posture is AllowDanger=true (builtinAllowDanger). A leaked
	// --allow-dangerously-skip-permissions would let the agent ignore the
	// deny-by-default workspace allowlist. Assert it is forced off and plan mode on.
	if contains(call.Args, "--allow-dangerously-skip-permissions") {
		t.Errorf("clean-room review must never skip permissions; argv: %v", call.Args)
	}
	if !argPair(call.Args, "--permission-mode", "plan") {
		t.Errorf("clean-room review must force --permission-mode plan; argv: %v", call.Args)
	}
}

// argPair reports whether args contains flag immediately followed by value.
func argPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestLaunch_AgentBNotWired(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithSessionsDir(os.TempDir()))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "escalation"}

	if err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Error("agent B (escalation) should be refused as not-yet-wired")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("not-yet-wired agent must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

func TestLaunch_DryRunSessionRefused(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithSessionsDir(os.TempDir()))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Agent: "claude"} // no workspace
	if err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Error("a session with no workspace (dry-run) should be refused")
	}
}
