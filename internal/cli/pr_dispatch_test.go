package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	internalexec "github.com/cameronsjo/forgectl/internal/exec"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
	"github.com/cameronsjo/forgectl/internal/pr"
)

func oldTmuxRunner() *internalexec.FakeRunner {
	return &internalexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 {
			if args[0] == "list-windows" {
				return "", nil
			}
			if args[0] == "-V" {
				return "tmux 2.1", nil
			}
		}
		return "", nil
	}}
}

func assertNoReviewMutationCalls(t *testing.T, calls []internalexec.Call) {
	t.Helper()
	for _, call := range calls {
		if call.Name == "git" || (call.Name == "gh" && len(call.Args) > 1 && call.Args[0] == "pr" && call.Args[1] == "view") ||
			(call.Name == "tmux" && len(call.Args) > 0 && (call.Args[0] == "new-session" || call.Args[0] == "new-window")) {
			t.Errorf("review mutation reached before capability refusal: %s %v", call.Name, call.Args)
		}
	}
}

func TestPrRemoteDispatchCapabilityRefusesBeforePrepare(t *testing.T) {
	fake := oldTmuxRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()))
	netClient := netpkg.New(fake)
	cmd := newPrCmdForClient(config.Config{}, client, netClient, filepath.Join(t.TempDir(), "reviewed.json"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"cameronsjo/forgectl#42"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tmux 2.2 or newer") {
		t.Fatalf("error = %v, want tmux floor refusal", err)
	}
	assertNoReviewMutationCalls(t, fake.Calls)
}

func TestPrLocalDispatchCapabilityRefusesBeforePrepareEvenNoVerify(t *testing.T) {
	fake := oldTmuxRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithFindingsDir(t.TempDir()))
	cmd := newPrLocalCmd(client, config.Config{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--no-verify"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tmux 2.2 or newer") {
		t.Fatalf("error = %v, want tmux floor refusal", err)
	}
	assertNoReviewMutationCalls(t, fake.Calls)
}

func TestLaunchPickedDispatchCapabilityRefusesBeforePrepareEvenNoVerify(t *testing.T) {
	fake := oldTmuxRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()))
	cmd, _, _ := newTestCmd()
	store := pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json"))
	err := launchPicked(context.Background(), client, config.Config{}, cmd,
		[]pr.PR{{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}}}, store, true)
	if err == nil || !strings.Contains(err.Error(), "tmux 2.2 or newer") {
		t.Fatalf("error = %v, want tmux floor refusal", err)
	}
	assertNoReviewMutationCalls(t, fake.Calls)
}
