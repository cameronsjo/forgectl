package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

func TestInteractiveTTY_RequiresBothDescriptors(t *testing.T) {
	for _, tt := range []struct {
		name            string
		stdinTTY        bool
		stdoutTTY       bool
		wantInteractive bool
	}{
		{name: "neither", stdinTTY: false, stdoutTTY: false, wantInteractive: false},
		{name: "stdout only", stdinTTY: false, stdoutTTY: true, wantInteractive: false},
		{name: "stdin only", stdinTTY: true, stdoutTTY: false, wantInteractive: false},
		{name: "both", stdinTTY: true, stdoutTTY: true, wantInteractive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := interactiveTTY(tt.stdinTTY, tt.stdoutTTY); got != tt.wantInteractive {
				t.Errorf("interactiveTTY(%t, %t) = %t, want %t", tt.stdinTTY, tt.stdoutTTY, got, tt.wantInteractive)
			}
		})
	}
}

// liveServer answers listings describing one session ($1 "main") holding one
// window (@3), so an Action's identity survives the revalidation every dispatch
// now performs. Building fixtures this way rather than stubbing the client is
// the point: the argv a test asserts is the argv that came out the far side of
// a real identity check.
func liveServer() *exec.FakeRunner {
	const sep = "\x1f"
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return strings.Join([]string{"123", "456", "$1", "main", "1", "1", "1700000000", "/w"}, sep), nil
		case "list-windows":
			return strings.Join([]string{"123", "456", "@3", "$1", "main", "0", "editor", "1", "1"}, sep), nil
		}
		return "", nil
	}}
}

func TestDispatchAction(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name            string
		action          func(*tmux.Client) tui.Action
		wantCallCount   int
		wantCmd         string
		wantArgs        []string
		wantInteractive bool
	}{
		{
			name:   "ActionNone makes no calls",
			action: func(*tmux.Client) tui.Action { return tui.Action{} },
		},
		{
			name: "ActionAttachSession inside tmux switches by native id",
			action: func(c *tmux.Client) tui.Action {
				return tui.Action{
					Kind:    tui.ActionAttachSession,
					Session: c.SessionIdentity(tmux.Session{ServerPID: "123", ServerStart: "456", ID: "$1", Name: "main"}),
				}
			},
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-t", "$1"},
		},
		{
			name: "ActionAttachWindow selects the window then switches to its parent",
			action: func(c *tmux.Client) tui.Action {
				return tui.Action{
					Kind:   tui.ActionAttachWindow,
					Window: c.WindowIdentity(tmux.Window{ServerPID: "123", ServerStart: "456", ID: "@3", SessionID: "$1", Name: "editor"}),
				}
			},
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-t", "$1"},
		},
		{
			name:          "ActionLast inside tmux issues switch-client -l",
			action:        func(*tmux.Client) tui.Action { return tui.Action{Kind: tui.ActionLast} },
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-l"},
		},
		{
			name:            "ActionPick issues interactive sesh connect",
			action:          func(*tmux.Client) tui.Action { return tui.Action{Kind: tui.ActionPick, Pick: "dev"} },
			wantCallCount:   1,
			wantCmd:         "sesh",
			wantInteractive: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := liveServer()
			// Stub the sesh PATH check so the ActionPick case exercises the
			// dispatch, not a real sesh binary (CI runners have none).
			client := tmux.New(fake,
				tmux.WithInsideTmux(func() bool { return true }),
				tmux.WithLookPath(func(string) (string, error) { return "sesh", nil }),
			)

			if err := dispatchAction(ctx, client, tc.action(client)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantCallCount == 0 {
				if len(fake.Calls) != 0 {
					t.Errorf("expected no calls, got %d: %v", len(fake.Calls), fake.Calls)
				}
				return
			}

			if len(fake.Calls) == 0 {
				t.Fatal("expected calls but got none")
			}
			last := fake.Last()
			if last.Name != tc.wantCmd {
				t.Errorf("cmd = %q, want %q", last.Name, tc.wantCmd)
			}
			if tc.wantInteractive && !last.Interactive {
				t.Errorf("expected interactive call, got non-interactive")
			}
			for i, want := range tc.wantArgs {
				if i >= len(last.Args) {
					t.Errorf("args[%d] missing, want %q", i, want)
					continue
				}
				if last.Args[i] != want {
					t.Errorf("args[%d] = %q, want %q", i, last.Args[i], want)
				}
			}
		})
	}
}

func TestDispatchAction_ErrorPropagates(t *testing.T) {
	ctx := context.Background()
	fake := liveServer()
	fake.InteractiveErr = &exec.CommandError{Name: "tmux", Err: &mockExitErr{}}
	client := tmux.New(fake, tmux.WithInsideTmux(func() bool { return false }))

	act := tui.Action{
		Kind:    tui.ActionAttachSession,
		Session: client.SessionIdentity(tmux.Session{ServerPID: "123", ServerStart: "456", ID: "$1", Name: "main"}),
	}
	if err := dispatchAction(ctx, client, act); err == nil {
		t.Fatal("expected error from failed attach, got nil")
	}
}

// TestDispatchAction_StaleIdentityIsRefused closes the gap the TUI's deferred
// action opens: Bubble Tea's teardown sits between choosing a session and
// attaching to it, and the server can restart in that window. The captured id
// would still resolve — to a different session.
func TestDispatchAction_StaleIdentityIsRefused(t *testing.T) {
	fake := liveServer()
	client := tmux.New(fake, tmux.WithInsideTmux(func() bool { return true }))
	act := tui.Action{
		Kind: tui.ActionAttachSession,
		// Same $1, a server generation that no longer exists.
		Session: client.SessionIdentity(tmux.Session{ServerPID: "999", ServerStart: "999", ID: "$1", Name: "main"}),
	}
	if err := dispatchAction(context.Background(), client, act); err == nil {
		t.Fatal("attached to a session id minted by a dead server generation")
	}
	for _, call := range fake.Calls {
		if len(call.Args) > 0 && call.Args[0] != "list-sessions" {
			t.Fatalf("ran %v on a stale identity, want only the listing", call.Args)
		}
	}
}

// mockExitErr satisfies the error interface for testing error propagation.
type mockExitErr struct{}

func (e *mockExitErr) Error() string { return "exit status 1" }

// --- group parents refuse stray tokens (forgectl#479) ---

// hubArgsAllowlist names parents that legitimately take arbitrary args on
// their own invocation — launch's Use is "launch [harness args…]", the
// pre-Cobra passthrough for the launcher. Every other parent with
// subcommands and its own RunE must reject a stray token via Args, or a typo
// of a real subverb (quarantine's `restor` for `restore`) falls through to
// that RunE with the typo as an ignored positional.
var hubArgsAllowlist = map[string]bool{"launch": true}

// TestGroupParentsRefuseStrayTokens walks every top-level command: a parent
// with subcommands AND its own RunE must declare Args, or an unmatched
// subverb reaches that RunE instead of Cobra's own unknown-command error.
// Commit ordering matters here: this must be true before shouldLaunchTUI's
// unknown-subverb arm is removed, because that arm is today the only thing
// standing between `quarantine restor` and a real quarantine hide.
func TestGroupParentsRefuseStrayTokens(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	for _, cmd := range root.Commands() {
		if len(cmd.Commands()) == 0 || cmd.RunE == nil {
			continue
		}
		if hubArgsAllowlist[cmd.Name()] {
			continue
		}
		if cmd.Args == nil {
			t.Errorf("command %q has subcommands and its own RunE but no Args validator — a stray subverb falls through to RunE instead of Cobra's unknown-command error", cmd.Name())
		}
	}
}

// TestTmuxFrobnicateReturnsCobraError pins the fix directly: an unknown
// tmux subverb must fail with Cobra's own error, never open the TUI (which
// would read as a Bubble Tea error/hang under go test's non-terminal stdio).
func TestTmuxFrobnicateReturnsCobraError(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{"tmux", "frobnicate"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for an unknown tmux subverb, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want cobra's unknown-command error", err.Error())
	}
}

// TestQuarantineRestorReturnsCobraErrorAndTouchesNothing pins the
// destructive half of the same fix: a typo of `restore` must never reach
// runQuarantineHide.
func TestQuarantineRestorReturnsCobraErrorAndTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	claudeMd := dir + "/CLAUDE.md"
	if err := os.WriteFile(claudeMd, []byte("hi"), 0o600); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}

	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{"quarantine", "restor", "--root", dir})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for the unknown subverb `restor`, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want cobra's unknown-command error", err.Error())
	}
	if _, statErr := os.Stat(claudeMd); statErr != nil {
		t.Errorf("CLAUDE.md should still be present at its original name, stat error: %v", statErr)
	}
}

// --- leadsWithPath / path-preserving error rendering (forgectl#481) ---

func TestLeadsWithPath(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  string
		want bool
	}{
		{name: "leading dotfile", msg: ".env not found", want: true},
		{name: "leading absolute path", msg: "/etc/passwd is unreadable", want: true},
		{name: "leading relative path", msg: "config/local.toml not found", want: true},
		{name: "prose with a path later", msg: "example file .env.example not found", want: false},
		{name: "ordinary prose", msg: "plain failure", want: false},
		{name: "empty", msg: "", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := leadsWithPath(tt.msg); got != tt.want {
				t.Errorf("leadsWithPath(%q) = %t, want %t", tt.msg, got, tt.want)
			}
		})
	}
}

// TestFangErrorSinkKeepsPathCaseForLeadingPathErrors pins the specific
// corruption fang's ErrorText transform causes: it title-cases only the
// FIRST WORD of a message, so a path-leading error like ".env not found"
// arrives as ".Env not found" — a spelling that does not exist and that a
// --file value the user typed byte-for-byte should never grow letters it
// did not have.
func TestFangErrorSinkKeepsPathCaseForLeadingPathErrors(t *testing.T) {
	root := &cobra.Command{
		Use:          "forgectl",
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New(".env not found")
		},
	}
	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs(nil)

	if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef", theme.Default())...); err == nil {
		t.Fatal("expected the command to fail")
	}
	out := stderr.String()
	if !strings.Contains(out, ".env not found.") {
		t.Errorf("path-leading error lost its original case: %q", out)
	}
	if strings.Contains(out, ".Env") {
		t.Errorf("path-leading error was title-cased: %q", out)
	}
}

// TestFangErrorSinkStructuredHeadlinePathLeadingKeepsCase is
// renderStructuredTerminalError's sibling of the test above — the
// structured error surface applies the same UnsetTransform guard to its
// headline.
func TestFangErrorSinkStructuredHeadlinePathLeadingKeepsCase(t *testing.T) {
	root := &cobra.Command{
		Use:          "forgectl",
		SilenceUsage: true,
		Args: func(*cobra.Command, []string) error {
			return &structuredTerminalError{headline: "/etc/passwd is unreadable"}
		},
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs(nil)

	if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef", theme.Default())...); err == nil {
		t.Fatal("expected the command to fail")
	}
	out := stderr.String()
	if !strings.Contains(out, "/etc/passwd is unreadable.") {
		t.Errorf("structured headline lost its original case: %q", out)
	}
	if strings.Contains(out, "/Etc") {
		t.Errorf("structured headline was title-cased: %q", out)
	}
}

// TestTermsafeErrorHandler_SilentCodedError_RendersNothing pins
// silentCodedError's whole reason to exist: env check --json has already
// written its one JSON object to stderr, and fang's error frame must not
// be appended after it.
func TestTermsafeErrorHandler_SilentCodedError_RendersNothing(t *testing.T) {
	var buf bytes.Buffer
	err := &silentCodedError{code: 2}
	termsafeErrorHandler(&buf, fang.Styles{}, err)
	if buf.Len() != 0 {
		t.Errorf("termsafeErrorHandler wrote %q for a silentCodedError, want nothing", buf.String())
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(silentCodedError) = %d, want 2", got)
	}
}
