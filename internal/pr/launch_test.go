package pr

// Test plan for launch.go
//
// PostReview (Classification: SECURITY INVARIANT — no post without approval)
//   [x] Approval declined → posted=false and NO `gh pr review` argv reaches
//       the Runner
//   [x] Headless → staged only, no gate shown, no post
//   [x] Non-interactive (no TTY) → staged only, no post
//   [x] Approval granted (+TTY, not headless) → posts with exact argv
//   [x] Local session → refused outright, NO Runner call at all
// Launch (Classification: ops layer, tmux dispatch)
//   [x] Agent A (InlineSeeded) → tmux new-window argv with claude + -p prompt
//   [x] Agent A hardened: no --allow-dangerously-skip-permissions, --permission-mode plan
//       (even though the launch default posture is AllowDanger=true)
//   [x] Agent B (BareTUIEscalation) → "not yet wired" error, NO tmux call
//   [x] Dry-run session (no workspace) → refused
// windowName (Classification: tmux window identity)
//   [x] Includes Owner, not just Number — a local-mode Ref and a PR-mode Ref
//       that derive the same Number must not collide on window name
//   [x] Includes Repo, not just Owner — two repos under one owner that derive
//       the same Number must not collide on window name

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

func TestPostReview_LocalSessionRefused(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := postClient(fake, true, true) // approve=true, tty=true — must still refuse
	localSess := Session{Ref: Ref{Owner: "local", Repo: "abc1234", Number: 1}, Workspace: "/tmp/forgectl-x"}

	posted, err := c.PostReview(context.Background(), localSess, "the review", false)
	if err == nil {
		t.Fatal("expected PostReview to refuse a local session")
	}
	if posted {
		t.Error("a local session must never post")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no argv should reach the Runner for a local session; got %+v", fake.Calls)
	}
}

// TestPostReview_ReloadedLocalSessionStillRefused guards the reload path: a
// Session reconstituted from a breadcrumb (loadSession) never carries any
// in-process-only fields, same as HeadRef/HeadOid. The guard must still
// catch this case via the persisted Ref.IsLocal(), or a future verb built on
// the loadSession pattern would silently defeat PostReview's safety
// invariant.
func TestPostReview_ReloadedLocalSessionStillRefused(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := postClient(fake, true, true)
	reloaded := Session{Ref: Ref{Owner: "local", Repo: "abc1234", Number: 1}, Workspace: "/tmp/forgectl-x"} // as loadSession produces

	posted, err := c.PostReview(context.Background(), reloaded, "the review", false)
	if err == nil {
		t.Fatal("expected PostReview to refuse a reload-reconstituted local session")
	}
	if posted || len(fake.Calls) != 0 {
		t.Errorf("posted=%v calls=%+v, want refused with zero Runner calls", posted, fake.Calls)
	}
}

func TestWindowName_OwnerDistinguishesLocalFromPRMode(t *testing.T) {
	// A local-mode Ref and a PR-mode Ref that derive the identical Number must
	// not collide on tmux window name — Owner ("local" vs a real GitHub owner)
	// is what disambiguates them.
	local := Ref{Owner: "local", Repo: "abc1234", Number: 42}
	prMode := Ref{Owner: "someowner", Repo: "somerepo", Number: 42}
	if windowName(local) == windowName(prMode) {
		t.Errorf("windowName collided: local=%q prMode=%q", windowName(local), windowName(prMode))
	}
	if windowName(local) != "pr-local-abc1234-42" {
		t.Errorf("windowName(local) = %q, want %q", windowName(local), "pr-local-abc1234-42")
	}
}

// TestWindowName_RepoDistinguishesCrossRepo guards the cross-repo collision
// this fix targets: two repos under the same owner (o/a#42 and o/b#42) must
// not collide on tmux window name — Repo is what disambiguates them, since
// Owner and Number alone are identical between the two Refs.
func TestWindowName_RepoDistinguishesCrossRepo(t *testing.T) {
	a := Ref{Owner: "o", Repo: "a", Number: 42}
	b := Ref{Owner: "o", Repo: "b", Number: 42}
	if windowName(a) == windowName(b) {
		t.Errorf("windowName collided across repos: a=%q b=%q", windowName(a), windowName(b))
	}
	if windowName(a) != "pr-o-a-42" {
		t.Errorf("windowName(a) = %q, want %q", windowName(a), "pr-o-a-42")
	}
	if windowTarget := (&Client{tmuxSession: "forgectl"}).windowTarget(a); windowTarget != "forgectl:"+windowName(a) {
		t.Errorf("windowTarget(a) = %q, want %q", windowTarget, "forgectl:"+windowName(a))
	}
}

// TestWindowName_SanitizesDotsInRepo guards against a tmux target-parsing
// hazard: a literal "." in a window name is mis-resolved by tmux as the
// window.pane separator (empirically verified: `select-window -t
// sess:pr-o-foo.bar-42` resolves to window="pr-o-foo", pane="bar-42", not
// the intended window). A dotted repo name (legal on GitHub) must have its
// dots replaced so the resulting window name targets cleanly.
func TestWindowName_SanitizesDotsInRepo(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "foo.bar", Number: 42}
	if got, want := windowName(ref), "pr-o-foo-bar-42"; got != want {
		t.Errorf("windowName(dotted repo) = %q, want %q", got, want)
	}
}

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
	if !contains(call.Args, "pr-o-r-42") || !contains(call.Args, ws) || !contains(call.Args, claudeBin) {
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

func TestLaunchInline_LocalSessionAddsFindingsDirAndPrompt(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	findingsDir := t.TempDir()

	// Local session: --add-dir must carry findingsDir, and the prompt must be
	// the local (offline) variant, not the PR reviewPrompt.
	fake := &exec.FakeRunner{}
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws := fakeWorkspace(t)
	localSess := Session{Ref: Ref{Owner: "local", Repo: "abc1234", Number: 1}, Workspace: ws, Agent: "claude", FindingsDir: findingsDir}

	if err := c.Launch(context.Background(), localSess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	call := fake.Last()
	if !argPair(call.Args, "--add-dir", findingsDir) {
		t.Errorf("local session argv missing --add-dir %s: %v", findingsDir, call.Args)
	}
	if !contains(call.Args, localReviewPrompt(findingsDir)) {
		t.Errorf("local session argv missing localReviewPrompt: %v", call.Args)
	}
	if contains(call.Args, reviewPrompt) {
		t.Errorf("local session must not use the PR reviewPrompt: %v", call.Args)
	}

	// Non-local session: no --add-dir for any findings-shaped path, PR prompt used.
	fake2 := &exec.FakeRunner{}
	c2 := New(fake2, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws2 := fakeWorkspace(t)
	prSess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws2, Agent: "claude"}
	if err := c2.Launch(context.Background(), prSess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	call2 := fake2.Last()
	if contains(call2.Args, "--add-dir") {
		t.Errorf("non-local session argv must not carry --add-dir: %v", call2.Args)
	}
	if !contains(call2.Args, reviewPrompt) {
		t.Errorf("non-local session argv missing reviewPrompt: %v", call2.Args)
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
