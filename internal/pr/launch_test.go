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
// ReviewWindowName (Classification: tmux window identity)
//   [x] Structural coupling: its output always starts with admission.go's
//       reviewWindowPrefix — guards against the two drifting apart silently
//       (mutation-verified: renaming the literal alone left every admission
//       test green, since reviewWindowPrefix stayed unchanged and nothing
//       asserted the two matched)
//   [x] Separation, keying, grammar, and fail-closed behavior live in
//       window_name_test.go, sessionkey_test.go, and sessionname_test.go

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

func postClient(fake *exec.FakeRunner, approve bool, tty bool) *Client {
	return New(fake,
		WithSessionsDir(os.TempDir()),
		WithApprover(func(string) (bool, error) { return approve, nil }),
		WithTTYCheck(func() bool { return tty }),
	)
}

var testSess = Session{Ref: Ref{Owner: "o", Repo: "r", Number: 9}, Workspace: "/tmp/forgectl-x"}

func successfulLaunchRunner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 {
			switch args[0] {
			case "-V":
				return "tmux 3.7b", nil
			case "display-message":
				return "123\x1f456\x1f@0", nil
			case "list-sessions":
				// Empty server: EnsureSession creates the review session.
				return "", nil
			case "new-session":
				// `new-session -P -F` returns the generation plus the new
				// session's native id, which is what the dispatch targets.
				return "123\x1f456\x1f$1", nil
			case "new-window":
				return "123\x1f456\x1f@1", nil
			}
		}
		return "", nil
	}}
}

func TestPostReview_LocalSessionRefused(t *testing.T) {
	fake := successfulLaunchRunner()
	c := postClient(fake, true, true) // approve=true, tty=true — must still refuse
	localSess := Session{Ref: mustLocalRef("abc1234", 1), Workspace: "/tmp/forgectl-x"}

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
// in-process-only fields, same as HeadRef/HeadOid. The guard must still catch
// this case via Ref.IsLocal(), which loadSession restores from the
// breadcrumb's Local field, or a future verb built on the loadSession pattern
// would silently defeat PostReview's safety invariant.
func TestPostReview_ReloadedLocalSessionStillRefused(t *testing.T) {
	fake := successfulLaunchRunner()
	c := postClient(fake, true, true)
	reloaded := Session{Ref: mustLocalRef("abc1234", 1), Workspace: "/tmp/forgectl-x"} // as loadSession produces

	posted, err := c.PostReview(context.Background(), reloaded, "the review", false)
	if err == nil {
		t.Fatal("expected PostReview to refuse a reload-reconstituted local session")
	}
	if posted || len(fake.Calls) != 0 {
		t.Errorf("posted=%v calls=%+v, want refused with zero Runner calls", posted, fake.Calls)
	}
}

// The local-vs-PR-mode and cross-repo collisions these tests used to pin by
// literal name (pr-local-abc1234-42, pr-o-a-42) are pinned harder in
// window_name_test.go, which asserts separation for the exact adversarial
// pair — same owner spelling, same repo, same number — rather than for two
// refs that already differed on their face.

// TestNewWindowTarget_IsSessionIDWithTrailingColon pins the dispatch
// destination. The colon is what makes the operand session-qualified for
// new-window; without it tmux reads the value as a window target and resolves
// it by prefix (forgectl#237).
func TestNewWindowTarget_IsSessionIDWithTrailingColon(t *testing.T) {
	session := tmux.SessionIdentity{ID: "$4", Name: "forgectl"}
	got, err := newWindowTarget(session)
	if err != nil {
		t.Fatalf("newWindowTarget: %v", err)
	}
	if got != "$4:" {
		t.Fatalf("newWindowTarget = %q, want %q", got, "$4:")
	}
	if strings.Contains(got, "forgectl") {
		t.Fatalf("newWindowTarget = %q; the session NAME must never reach a -t operand", got)
	}
}

// The dotted-repo hazard — tmux reads "." as the window.pane separator, so
// `select-window -t sess:pr-o-foo.bar-42` resolved to window="pr-o-foo",
// pane="bar-42" — is now excluded by the generated grammar rather than by a
// dot-to-hyphen rewrite, and asserted in sessionname_test.go against the whole
// tmux target charset. window_name_test.go covers the recollision that rewrite
// used to cause, between repos "a.b" and "a-b".

// TestWindowName_StructurallyCoupledToReviewWindowPrefix guards the
// admission gate's discrimination: LiveReviews (admission.go) counts live
// reviews by matching reviewWindowPrefix against tmux window names, so every
// windowName output MUST start with that exact constant, built structurally
// rather than re-hardcoded — a re-hardcoded literal can drift from the
// constant with no compiler or test signal (verified: it did, silently,
// before this fix).
func TestWindowName_StructurallyCoupledToReviewWindowPrefix(t *testing.T) {
	refs := []Ref{
		{Owner: "local", Repo: "abc1234", Number: 42},
		{Owner: "o", Repo: "a", Number: 1},
		{Owner: "o", Repo: "foo.bar", Number: 7},
	}
	for _, ref := range refs {
		got, err := ReviewWindowName(ref)
		if err != nil {
			t.Errorf("ReviewWindowName(%+v): %v", ref, err)
			continue
		}
		if !strings.HasPrefix(got, reviewWindowPrefix) {
			t.Errorf("ReviewWindowName(%+v) = %q, want prefix %q", ref, got, reviewWindowPrefix)
		}
	}
}

func TestPostReview_DeclinedDoesNotPost(t *testing.T) {
	fake := successfulLaunchRunner()
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
	fake := successfulLaunchRunner()
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
	fake := successfulLaunchRunner()
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
	fake := successfulLaunchRunner()
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

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "claude"}

	dispatch, err := c.Launch(context.Background(), sess, config.Config{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if dispatch.Ref != sess.Ref || dispatch.WindowID != "123\x1f456\x1f@1" {
		t.Errorf("dispatch = %+v, want exact generation-qualified identity", dispatch)
	}
	call := fake.Last()
	if call.Name != "tmux" || call.Args[0] != "new-window" {
		t.Fatalf("expected tmux new-window; got %+v", call)
	}
	if !contains(call.Args, mustWindowName(t, sess.Ref)) || !contains(call.Args, ws) || !contains(call.Args, claudeBin) {
		t.Errorf("tmux argv missing window/workspace/claude: %v", call.Args)
	}
	if !contains(call.Args, "-p") || !contains(call.Args, reviewPrompt) {
		t.Errorf("tmux argv missing seeded -p prompt: %v", call.Args)
	}
	if !contains(call.Args, "--") {
		t.Errorf("tmux argv missing -- terminator before claude: %v", call.Args)
	}
	if len(call.Args) < 4 || !equalArgs(call.Args[:4], []string{"new-window", "-P", "-F", tmux.IdentityFormat}) {
		t.Errorf("tmux argv missing exact -P/-F identity capture: %v", call.Args)
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
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "escalation"}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Error("agent B (escalation) should be refused as not-yet-wired")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("not-yet-wired agent must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

// TestLaunch_CodexRefusedForRemotePRHead is the SECURITY INVARIANT the
// use-based boundary rests on. A remote PR head is a third party's content,
// and the Codex agent cannot be confined on `codex exec`, so dispatch must not
// happen at all — the assertion is on the behavior (zero Runner calls), not
// the message, because a well-worded error that still launched would be the
// whole bug.
//
// This test replaces one that asserted the opposite (that a remote Codex
// review dispatched with a compensating sandbox). That posture was the finding.
func TestLaunch_CodexRefusedForRemotePRHead(t *testing.T) {
	codexBin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("FORGECTL_CODEX_BIN", codexBin)

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{
		Ref:       Ref{Owner: "o", Repo: "r", Number: 42},
		Workspace: fakeWorkspace(t),
		Agent:     "codex",
	}

	_, err := c.Launch(context.Background(), sess, config.Config{})
	if err == nil {
		t.Fatal("Codex must be refused for a remote PR head")
	}
	// The behavioral assertion: nothing was dispatched.
	if len(fake.Calls) != 0 {
		t.Errorf("a refused agent must issue ZERO Runner calls; got %+v", fake.Calls)
	}
	// The message must be actionable, naming the reason and the way forward.
	// Under forgectl#232 the reason is stated as authorship rather than as
	// "remote PR head" — a remote ref resolves third-party unconditionally, so
	// the operator is told whose code it is, not how it was fetched.
	for _, want := range []string{"someone else", "which commands run", "--agent claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message missing %q: %v", want, err)
		}
	}
	// It must NOT offer `pr local` as the way to review THIS PR. Doing so
	// routes the operator around the control: `pr local` pointed at the
	// clean-room workspace reviews the same hostile head with the same
	// unconfined agent. (rejectCleanRoomPath enforces that separately; the
	// message must not advertise the attempt either.)
	if strings.Contains(err.Error(), "pr local") {
		t.Errorf("refusal message routes the operator around the control: %v", err)
	}
	// Nor may it name the assertion flag here. On a known third-party head the
	// flag would have to be a false statement to work, so advertising it is the
	// same routing-around in a smaller costume.
	if strings.Contains(err.Error(), "--operator-authored") {
		t.Errorf("refusal message advertises the flag that would silence it: %v", err)
	}
}

// TestCheckAgentForReview_BoundaryIsAuthorshipNotLocality walks the full matrix
// through the ref+declaration seam, which is how every production caller reaches
// it. The Codex agent is refused everywhere except over code the operator has
// asserted they wrote; every other pairing is untouched.
//
// The row that changed with forgectl#232 is marked. Under the old check, `codex`
// on a LOCAL ref was permitted outright — that permission is what a `gh pr
// checkout` tree inherited. It now requires the declaration, and locality alone
// buys nothing.
func TestCheckAgentForReview_BoundaryIsAuthorshipNotLocality(t *testing.T) {
	remote := Ref{Owner: "o", Repo: "r", Number: 42}
	local := mustLocalRef("abc1234", 1)

	for _, tc := range []struct {
		agent      string
		ref        Ref
		declared   ReviewProvenance
		wantRefuse bool
	}{
		{"codex", remote, ReviewProvenanceThirdParty, true},
		{"codex", remote, ReviewProvenanceOperatorAuthored, true}, // downgraded: remote cannot be authored
		{"codex", local, ReviewProvenanceUnknown, true},           // CHANGED by #232: locality is not authorship
		{"codex", local, ReviewProvenanceThirdParty, true},
		{"codex", local, ReviewProvenanceOperatorAuthored, false}, // the one permitted cell

		{"claude", remote, ReviewProvenanceThirdParty, false},
		{"claude", local, ReviewProvenanceUnknown, false},
		{"", remote, ReviewProvenanceThirdParty, false}, // default → agent A
		{"", local, ReviewProvenanceUnknown, false},
		{"escalation", remote, ReviewProvenanceThirdParty, false}, // unwired, but not THIS refusal's business
	} {
		err := CheckAgentForReview(tc.agent, EffectiveProvenance(tc.ref, tc.declared))
		if got := err != nil; got != tc.wantRefuse {
			t.Errorf("CheckAgentForReview(%q, local=%v, declared=%v) refused = %v, want %v (err: %v)",
				tc.agent, tc.ref.IsLocal(), tc.declared, got, tc.wantRefuse, err)
		}
	}
}

// TestLaunch_CodexStillDispatchesForLocalReview is the other direction of the
// ruling: confining by use must not disable the use that is safe.
//
// forgectl#232 narrowed what "safe" means here. The session must now carry the
// operator's authorship assertion — locality alone no longer dispatches, because
// a local tree can hold a contributor's commit.
func TestLaunch_CodexStillDispatchesForLocalReview(t *testing.T) {
	codexBin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("FORGECTL_CODEX_BIN", codexBin)

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{
		Ref:         mustLocalRef("abc1234", 1),
		Workspace:   fakeWorkspace(t),
		Agent:       "codex",
		FindingsDir: t.TempDir(),
		Provenance:  ReviewProvenanceOperatorAuthored,
	}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("local Codex review must still dispatch: %v", err)
	}
	args := fake.Last().Args
	if !contains(args, codexBin) || !contains(args, "exec") {
		t.Errorf("local Codex review did not reach `codex exec`: %v", args)
	}
}

func TestLaunch_CodexLocalWritesOnlyWorkspaceAndFindings(t *testing.T) {
	codexBin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("FORGECTL_CODEX_BIN", codexBin)
	findings := t.TempDir()
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{
		Ref:         mustLocalRef("abc1234", 1),
		Workspace:   fakeWorkspace(t),
		Agent:       "codex",
		FindingsDir: findings,
		Provenance:  ReviewProvenanceOperatorAuthored,
	}
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch local Codex: %v", err)
	}
	args := fake.Last().Args
	if !argPair(args, "--sandbox", "workspace-write") ||
		!argPair(args, "--add-dir", findings) {
		t.Errorf("local Codex sandbox did not scope findings: %v", args)
	}
	if !contains(args, localReviewPrompt(findings, false)) {
		t.Errorf("local Codex prompt missing findings path: %v", args)
	}
	// The Codex path must NOT claim the findings dir is the only writable one —
	// nothing enforces that under `codex exec`, and prose that reads as a
	// control is worse than none.
	if contains(args, localReviewPrompt(findings, true)) {
		t.Errorf("local Codex prompt asserts an unenforced write restriction: %v", args)
	}
}

// TestLocalReviewPrompt_OnlyClaimsEnforcementWhenEnforced is the unit-level
// guard on the same property: the "only directory you may write to" phrasing
// is reserved for the harness that actually enforces it.
func TestLocalReviewPrompt_OnlyClaimsEnforcementWhenEnforced(t *testing.T) {
	const dir = "/tmp/findings-xyz"
	const claim = "the only directory you may write to"

	enforced := localReviewPrompt(dir, true)
	if !strings.Contains(enforced, claim) {
		t.Errorf("enforced prompt should state the restriction as fact: %q", enforced)
	}
	unenforced := localReviewPrompt(dir, false)
	if strings.Contains(unenforced, claim) {
		t.Errorf("unenforced prompt must not assert an enforcement: %q", unenforced)
	}
	for _, p := range []string{enforced, unenforced} {
		if !strings.Contains(p, dir) {
			t.Errorf("prompt must name the findings dir: %q", p)
		}
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
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws := fakeWorkspace(t)
	localSess := Session{Ref: mustLocalRef("abc1234", 1), Workspace: ws, Agent: "claude", FindingsDir: findingsDir}

	if _, err := c.Launch(context.Background(), localSess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	call := fake.Last()
	if !argPair(call.Args, "--add-dir", findingsDir) {
		t.Errorf("local session argv missing --add-dir %s: %v", findingsDir, call.Args)
	}
	if !contains(call.Args, localReviewPrompt(findingsDir, true)) {
		t.Errorf("local session argv missing localReviewPrompt: %v", call.Args)
	}
	if contains(call.Args, reviewPrompt) {
		t.Errorf("local session must not use the PR reviewPrompt: %v", call.Args)
	}

	// Non-local session: no --add-dir for any findings-shaped path, PR prompt used.
	fake2 := successfulLaunchRunner()
	c2 := New(fake2, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws2 := fakeWorkspace(t)
	prSess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws2, Agent: "claude"}
	if _, err := c2.Launch(context.Background(), prSess, config.Config{}); err != nil {
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
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Agent: "claude"} // no workspace
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Error("a session with no workspace (dry-run) should be refused")
	}
}

// TestLaunchInline_DoesNotInheritCodexModel guards the mirror of launchCodex's
// existing harness check. The clean-room review dispatches `claude` regardless
// of the operator's ambient launch profile, and `harness = "codex"` in
// [launch.defaults] is a plausible config for exactly the users this work
// targets — so without forcing the harness, a Codex model id reaches
// `claude --model <that>`.
// TestLaunchInline_DoesNotInheritCodexEffort is the effort half of
// TestLaunchInline_DoesNotInheritCodexModel below. Codex emits no --effort, so
// a level set in a Codex [launch.defaults] is inert there — and it becomes
// reachable only when this dispatch forces the harness to claude. Carrying it
// across would pair a level chosen under a Codex profile with a re-derived
// Claude model, the same mispairing the model half exists to prevent.
func TestLaunchInline_DoesNotInheritCodexEffort(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	cfg := config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{
		Harness: "codex",
		Model:   "gpt-5",
		Effort:  "max",
	}}}
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	if contains(args, "max") {
		t.Errorf("a Codex profile's effort must not ride the forced harness switch: %v", args)
	}
	// opus is DefaultModelFor("claude"), so the re-derived level is medium.
	if !argPair(args, "--effort", "medium") {
		t.Errorf("effort must be re-derived from the forced model (opus → medium): %v", args)
	}
}

func TestLaunchInline_DoesNotInheritCodexModel(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	cfg := config.Config{Launch: config.LaunchConfig{
		Defaults: config.LaunchDefaults{Harness: "codex", Model: "gpt-5-codex"},
	}}
	if _, err := c.Launch(context.Background(), sess, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	args := fake.Last().Args
	if contains(args, "gpt-5-codex") {
		t.Errorf("a Codex model id reached `claude --model`: %v", args)
	}
	if !argPair(args, "--model", "opus") {
		t.Errorf("expected the Claude default model, got: %v", args)
	}
}

// TestLaunch_LocalSessionWithoutFindingsDirRefused covers the reload shape:
// FindingsDir is not persisted, so a breadcrumb-reconstituted local session has
// it empty. Dispatching would pass --add-dir "" and seed a prompt naming no
// directory — a silently misconfigured review. It must fail loudly.
func TestLaunch_LocalSessionWithoutFindingsDirRefused(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	for _, agent := range []string{"claude", "codex"} {
		t.Run("agent="+agent, func(t *testing.T) {
			fake := successfulLaunchRunner()
			c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
			// As loadSession produces it for a canonical local breadcrumb: Ref
			// restored, provenance restored, FindingsDir zero because it is
			// deliberately not persisted.
			//
			// Provenance is operator-authored so the Codex case actually REACHES
			// this check — the #232 gate runs first, being the security boundary
			// rather than a configuration error, and would otherwise mask it.
			sess := Session{
				Ref:        mustLocalRef("abc1234", 1),
				Workspace:  fakeWorkspace(t),
				Agent:      agent,
				Provenance: ReviewProvenanceOperatorAuthored,
			}

			_, err := c.Launch(context.Background(), sess, config.Config{})
			if err == nil {
				t.Fatal("a local session with no findings dir must be refused")
			}
			if !strings.Contains(err.Error(), "findings directory") {
				t.Errorf("unexpected error: %v", err)
			}
			if len(fake.Calls) != 0 {
				t.Errorf("refusal must precede dispatch; got %+v", fake.Calls)
			}
		})
	}
}

// TestLaunchInline_ForcesStrictMCP asserts on the ARGV, not the profile field:
// the control only exists if the flag reaches the child process.
//
// Claude Code discovers project-scoped MCP config against its cwd, and the
// review's cwd IS the workspace holding the PR author's checkout. A discovered
// server's `command` + `args` are spawned at session START, before the agent
// invokes any tool — so --permission-mode plan and the deny-by-default
// workspace allowlist, which govern which TOOLS the agent may call, both sit
// downstream of a boundary already crossed. Measured on 2.1.220: with the flag
// a planted carrier did not spawn; without it, the same carrier did.
//
// The flag is forced regardless of the ambient launch config — there is no
// config surface that can set or clear it (see launch.TestResolve_NeverSetsStrictMCP).
func TestLaunchInline_ForcesStrictMCP(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	allowDanger := true
	cfg := config.Config{Launch: config.LaunchConfig{
		Defaults: config.LaunchDefaults{
			Harness: "claude", Model: "sonnet", PermissionMode: "acceptEdits", AllowDanger: &allowDanger,
		},
	}}

	// Remote PR head — the exploitable case.
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	prSess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(context.Background(), prSess, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !contains(fake.Last().Args, "--strict-mcp-config") {
		t.Errorf("clean-room review must refuse discovered MCP config; argv: %v", fake.Last().Args)
	}

	// Local review takes the same hardened path and must not lose it.
	//
	// mustLocalRef, not a Ref literal: locality is an unexported flag, so a
	// field-by-field Ref{Owner: localOwnerSentinel, …} is a REMOTE ref and this
	// case would silently re-test the remote path above.
	fake2 := successfulLaunchRunner()
	c2 := New(fake2, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	findingsDir := t.TempDir()
	localSess := Session{
		Ref:         mustLocalRef("abc1234", 1),
		Workspace:   fakeWorkspace(t),
		Agent:       "claude",
		FindingsDir: findingsDir,
	}
	if _, err := c2.Launch(context.Background(), localSess, cfg); err != nil {
		t.Fatalf("Launch (local): %v", err)
	}
	localArgs := fake2.Last().Args
	// Assert the local branch was actually TAKEN before asserting what it
	// preserved. profile.StrictMCP is set unconditionally, ahead of any
	// locality branch, so the --strict-mcp-config check below cannot fail
	// independently of the remote case — these two argv facts are what prove
	// this case exercised a different code path at all.
	if !contains(localArgs, "--add-dir") || !contains(localArgs, findingsDir) {
		t.Fatalf("local session did not take the local branch (no --add-dir %s); argv: %v", findingsDir, localArgs)
	}
	if !contains(localArgs, localReviewPrompt(findingsDir, true)) {
		t.Fatalf("local session did not take the local branch (not the local prompt); argv: %v", localArgs)
	}
	if !contains(localArgs, "--strict-mcp-config") {
		t.Errorf("local review lost --strict-mcp-config; argv: %v", localArgs)
	}
}

// TestLaunchInline_DropsAmbientAddDir guards the clean room's scoping. The
// hardening block clears AllowDanger, PermissionMode, Harness and Model — and
// must also clear AddDir, because launch.Resolve returns the operator's
// [launch.defaults].add_dir and BuilderArgs emits --add-dir for each. Otherwise
// `add_dir = ["~/notes"]` hands the reviewer of a REMOTE hostile head a root
// outside the workspace.
func TestLaunchInline_DropsAmbientAddDir(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	cfg := config.Config{Launch: config.LaunchConfig{
		Defaults: config.LaunchDefaults{AddDir: []string{"/private/notes", "/private/keys"}},
	}}

	// Remote PR review: no --add-dir at all.
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	prSess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(context.Background(), prSess, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	for _, leaked := range []string{"/private/notes", "/private/keys"} {
		if contains(args, leaked) {
			t.Errorf("ambient add_dir %q reached the clean-room reviewer: %v", leaked, args)
		}
	}
	if contains(args, "--add-dir") {
		t.Errorf("a remote review must grant no additional root: %v", args)
	}

	// Local review: exactly one --add-dir, the findings dir.
	findings := t.TempDir()
	fake2 := successfulLaunchRunner()
	c2 := New(fake2, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	localSess := Session{
		Ref:         mustLocalRef("abc1234", 1),
		Workspace:   fakeWorkspace(t),
		Agent:       "claude",
		FindingsDir: findings,
	}
	if _, err := c2.Launch(context.Background(), localSess, cfg); err != nil {
		t.Fatalf("Launch local: %v", err)
	}
	args2 := fake2.Last().Args
	if !argPair(args2, "--add-dir", findings) {
		t.Errorf("local review lost its findings dir: %v", args2)
	}
	for _, leaked := range []string{"/private/notes", "/private/keys"} {
		if contains(args2, leaked) {
			t.Errorf("ambient add_dir %q survived into the local review: %v", leaked, args2)
		}
	}
	if got := countArg(args2, "--add-dir"); got != 1 {
		t.Errorf("expected exactly one --add-dir, got %d: %v", got, args2)
	}
}

func countArg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}

// TestLaunchInline_PrConfigSetsModelAndEffort covers the [pr] knob against a
// HOSTILE ambient config — the case every other launchInline test misses,
// because they all pass config.Config{} and so cannot see a [pr]/[launch]
// interaction at all.
//
// Two things are asserted at once. The knob works: [pr] model/effort reach the
// argv. And the knob's blast radius is bounded: an ambient config doing
// everything wrong (bypassPermissions, allow_danger, a private add_dir) still
// gets the hardened clean-room posture, because [pr] carries no key that can
// reach those controls.
func TestLaunchInline_PrConfigSetsModelAndEffort(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	yes := true
	cfg := config.Config{
		Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{
			Model:          "sonnet", // would derive effort "high"
			PermissionMode: "bypassPermissions",
			AllowDanger:    &yes,
			AddDir:         []string{"/private/notes"},
		}},
		Pr: config.PrConfig{Model: "opus", Effort: "xhigh"},
	}

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args

	if !argPair(args, "--model", "opus") {
		t.Errorf("[pr] model did not reach the reviewer argv: %v", args)
	}
	if !argPair(args, "--effort", "xhigh") {
		t.Errorf("[pr] effort did not reach the reviewer argv: %v", args)
	}
	// The hardening, unchanged by anything [pr] can say.
	if !argPair(args, "--permission-mode", "plan") {
		t.Errorf("ambient bypassPermissions reached the clean room: %v", args)
	}
	if contains(args, "--allow-dangerously-skip-permissions") {
		t.Errorf("ambient allow_danger reached the clean room: %v", args)
	}
	if contains(args, "/private/notes") || contains(args, "--add-dir") {
		t.Errorf("ambient add_dir reached the clean room: %v", args)
	}
}

// TestLaunchInline_PrModelRederivesEffort is the reason [pr] is its own posture
// rather than a patch on the ambient one. Filling only when empty would be a
// silent no-op — Profile.Effort is already non-empty here (launch.Resolve
// derived "high" from the ambient sonnet) — so `[pr] model = "opus"` alone
// would have shipped `--model opus --effort high`, the exact mispairing this
// feature exists to prevent.
func TestLaunchInline_PrModelRederivesEffort(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	cfg := config.Config{
		Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{Model: "sonnet"}},
		Pr:     config.PrConfig{Model: "opus"}, // effort deliberately unset
	}
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	if !argPair(args, "--effort", "medium") {
		t.Errorf("[pr] model must re-derive effort (opus → medium), not carry the ambient sonnet's \"high\": %v", args)
	}
}

// TestLaunchInline_PrConfigRejectsOptionLikeValues: both values land adjacent
// to security flags in the hardened argv. There is no shell (exec.Command with
// separate args), so a space cannot split a token — the residual risk is a
// value like "--allow-dangerously-skip-permissions" riding in as the operand
// of --model or --effort and being parsed as a flag in its own right. Refuse
// before dispatch, and prove ZERO Runner calls happened.
func TestLaunchInline_PrConfigRejectsOptionLikeValues(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	for name, pc := range map[string]config.PrConfig{
		"model":  {Model: "--allow-dangerously-skip-permissions"},
		"effort": {Effort: "--dangerously-skip-permissions"},
	} {
		fake := successfulLaunchRunner()
		c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
		sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
		if _, err := c.Launch(context.Background(), sess, config.Config{Pr: pc}); err == nil {
			t.Errorf("[pr] %s: an option-like value must be refused", name)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("[pr] %s: a refused value must issue ZERO Runner calls; got %+v", name, fake.Calls)
		}
	}
}

// TestLaunchInline_RejectsOptionLikeAmbientValues is the other half of the
// option-like guard, and the half that covers the DEFAULT configuration. When
// [pr] is unset — the common case — the reviewer's --model and --effort
// operands come from the ambient [launch] profile instead, travel the exact
// same argv positions next to the clean room's security flags, and would be
// unchecked if the guard only covered the newest source of them.
func TestLaunchInline_RejectsOptionLikeAmbientValues(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	for name, defaults := range map[string]config.LaunchDefaults{
		"model":  {Model: "--allow-dangerously-skip-permissions"},
		"effort": {Model: "opus", Effort: "--dangerously-skip-permissions"},
	} {
		cfg := config.Config{Launch: config.LaunchConfig{Defaults: defaults}}
		fake := successfulLaunchRunner()
		c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
		sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
		if _, err := c.Launch(context.Background(), sess, cfg); err == nil {
			t.Errorf("ambient [launch] %s: an option-like value must be refused", name)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("ambient [launch] %s: a refused value must issue ZERO Runner calls; got %+v", name, fake.Calls)
		}
	}
}

// TestLaunchInline_RejectsInvalidEffort is the finding this closes: a review is
// dispatched into a DETACHED tmux window, so a value claude rejects produces an
// empty pane and no error anywhere — indistinguishable from a review still
// running. Refuse before dispatch, and prove zero Runner calls.
func TestLaunchInline_RejectsInvalidEffort(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	for name, cfg := range map[string]config.Config{
		"[pr] effort":      {Pr: config.PrConfig{Effort: "hihg"}},
		"ambient [launch]": {Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{Effort: "maximum"}}},
	} {
		fake := successfulLaunchRunner()
		c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
		sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}
		_, err := c.Launch(context.Background(), sess, cfg)
		if err == nil {
			t.Errorf("%s: an invalid effort must be refused before dispatch", name)
		} else if !strings.Contains(err.Error(), "effort") {
			t.Errorf("%s: the error must name the offending setting, got %q", name, err)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("%s: a refused value must issue ZERO Runner calls; got %+v", name, fake.Calls)
		}
	}
}

// TestLaunchCodex_RejectsOptionLikeModel is the Codex half of the option-like
// guard. launchCodex adopts the ambient model whenever the resolved harness is
// codex, and it lands in the same argv neighbourhood as the sandbox flag
// (`codex exec --sandbox read-only --model <value>`). Validate does not close
// it — on a Codex profile Validate only rejects CLAUDE model ids, so an
// option-like value passes untouched.
func TestLaunchCodex_RejectsOptionLikeModel(t *testing.T) {
	codexBin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("FORGECTL_CODEX_BIN", codexBin)

	cfg := config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{
		Harness: "codex",
		Model:   "--dangerously-bypass-approvals-and-sandbox",
	}}}
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{
		Ref:         mustLocalRef("abc1234", 1),
		Workspace:   fakeWorkspace(t),
		Agent:       "codex",
		FindingsDir: t.TempDir(),
	}
	if _, err := c.Launch(context.Background(), sess, cfg); err == nil {
		t.Error("an option-like Codex model must be refused before dispatch")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a refused value must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}
