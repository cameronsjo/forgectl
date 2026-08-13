package cli

// Test plan for the single-review verification outcomes (plan step 5's CLI
// half — `forgectl pr <ref>` and `forgectl pr local`).
//
// pr_verify.go's formatter is table-tested in pr_verify_test.go, but that
// pins the RENDERING of a result someone else built. These rows drive the
// wiring that produces it: a real Launch, a real pr.Client.VerifyDispatched,
// and the state each outcome lands the command in.
//
//   [x] Remote live: dispatched window survives → success block printed, nil
//   [x] Remote gone: window vanishes between dispatch and sweep → non-zero
//       error naming the ref, and no success block
//   [x] Remote unknown: the sweep's list-windows fails → cause-neutral unknown
//       error wrapping the cause, and no ref labeled gone
//   [x] Remote --no-verify: the gone window is not observed, command succeeds
//   [x] Local live / gone / unknown: the same three outcomes through
//       newPrLocalCmd's separate construction path
//
// Every row uses a zero-cost dispatch waiter: no test may sleep for the
// production eight-second observation delay.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// remoteReviewCmd wires `forgectl pr <ref>` over a ledger-backed runner, with a
// throwaway breadcrumb dir and a zero-cost waiter.
func remoteReviewCmd(t *testing.T, l *tmuxLedger) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	fakeClaudeBin(t)
	reviewTempRoot(t)
	fake := l.runner()
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(l.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cmd := newPrCmdForClient(config.Config{}, client, netpkg.New(fake), filepath.Join(t.TempDir(), "reviewed.json"))
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	return cmd, out
}

// localReviewCmd is the same for `forgectl pr local`. PrepareLocal wants real
// `git rev-parse` answers, so the ledger's non-tmux fallback is the local one.
func localReviewCmd(t *testing.T, l *tmuxLedger) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	fakeClaudeBin(t)
	reviewTempRoot(t)
	git := prLocalFakeRunner().RunFunc
	fake := l.runnerWith(git)
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(l.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cmd := newPrLocalCmd(client, config.Config{})
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	return cmd, out
}

// reviewTempRoot points $TMPDIR at a t.TempDir, so the sandbox workspace a real
// prepare creates (os.MkdirTemp("", …), which reads $TMPDIR per call) is torn
// down with the test. Reading the printed workspace path instead would clean up
// only the rows that got as far as printing one — a failing row prepares a
// workspace and prints nothing.
func reviewTempRoot(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}

func TestPrRemoteDispatchVerificationOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		dieAll    bool
		listErr   error
		args      []string
		wantErr   string
		wantOut   string
		forbidOut string
	}{
		{
			name:    "live",
			args:    []string{"cameronsjo/forgectl#42"},
			wantOut: "prepared clean-room review of cameronsjo/forgectl#42",
		},
		{
			name:      "gone",
			dieAll:    true,
			args:      []string{"cameronsjo/forgectl#42"},
			wantErr:   "review window disappeared during startup: cameronsjo/forgectl#42",
			forbidOut: "prepared clean-room review",
		},
		{
			name:      "unknown",
			listErr:   errors.New("boom: tmux exploded"),
			args:      []string{"cameronsjo/forgectl#42"},
			wantErr:   "dispatch state is unknown",
			forbidOut: "prepared clean-room review",
		},
		{
			// --no-verify drops the observation, not the dispatch: the window
			// that the "gone" row reports is simply never looked at.
			name:    "no-verify over a dying window",
			dieAll:  true,
			args:    []string{"cameronsjo/forgectl#42", "--no-verify"},
			wantOut: "prepared clean-room review of cameronsjo/forgectl#42",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := newTmuxLedger("forgectl")
			ledger.dieAll = tt.dieAll
			ledger.listErr = tt.listErr // a single review never lists at admission
			cmd, out := remoteReviewCmd(t, ledger)
			cmd.SetArgs(tt.args)

			err := cmd.ExecuteContext(context.Background())
			assertOutcome(t, err, out.String(), tt.wantErr, tt.wantOut, tt.forbidOut)
			if tt.wantErr != "" && strings.Contains(out.String(), "prepared clean-room review") {
				t.Errorf("stdout = %q, a failed verification must not print the success block", out.String())
			}
		})
	}
}

func TestPrLocalDispatchVerificationOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		dieAll    bool
		listErr   error
		noVerify  bool
		wantErr   string
		wantOut   string
		forbidOut string
	}{
		{name: "live", wantOut: "prepared local clean-room review of main @"},
		{
			name:      "gone",
			dieAll:    true,
			wantErr:   "review window disappeared during startup: local/",
			forbidOut: "prepared local clean-room review",
		},
		{
			name:      "unknown",
			listErr:   errors.New("boom: tmux exploded"),
			wantErr:   "dispatch state is unknown",
			forbidOut: "prepared local clean-room review",
		},
		{name: "no-verify over a dying window", dieAll: true, noVerify: true, wantOut: "prepared local clean-room review of main @"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := newTmuxLedger("forgectl")
			ledger.dieAll = tt.dieAll
			ledger.listErr = tt.listErr
			cmd, out := localReviewCmd(t, ledger)
			args := []string{t.TempDir()}
			if tt.noVerify {
				args = append(args, "--no-verify")
			}
			cmd.SetArgs(args)

			err := cmd.ExecuteContext(context.Background())
			assertOutcome(t, err, out.String(), tt.wantErr, tt.wantOut, tt.forbidOut)
		})
	}
}

func assertOutcome(t *testing.T, err error, body, wantErr, wantOut, forbidOut string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
	} else {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("error = %v, want containing %q", err, wantErr)
		}
		// An unreadable tmux says nothing about any individual window; only the
		// gone outcome may name a ref as disappeared.
		if !strings.Contains(wantErr, "disappeared") && strings.Contains(err.Error(), "disappeared") {
			t.Errorf("error = %q, must not label a ref gone", err.Error())
		}
	}
	if wantOut != "" && !strings.Contains(body, wantOut) {
		t.Errorf("stdout = %q, want containing %q", body, wantOut)
	}
	if forbidOut != "" && strings.Contains(body, forbidOut) {
		t.Errorf("stdout = %q, must not contain %q", body, forbidOut)
	}
}
