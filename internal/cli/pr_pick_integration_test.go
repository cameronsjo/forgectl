//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	internalexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

type tmuxRoutingRunner struct {
	real internalexec.OSRunner
	fake *internalexec.FakeRunner
}

func (r tmuxRoutingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if filepath.Base(name) == "tmux" {
		return r.real.Run(ctx, name, args...)
	}
	return r.fake.Run(ctx, name, args...)
}

func (r tmuxRoutingRunner) RunInteractive(ctx context.Context, name string, args ...string) error {
	if filepath.Base(name) == "tmux" {
		return r.real.RunInteractive(ctx, name, args...)
	}
	return r.fake.RunInteractive(ctx, name, args...)
}

func (r tmuxRoutingRunner) RunWithInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if filepath.Base(name) == "tmux" {
		return r.real.RunWithInput(ctx, stdin, name, args...)
	}
	return r.fake.RunWithInput(ctx, stdin, name, args...)
}

func (r tmuxRoutingRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	if filepath.Base(name) == "tmux" {
		return r.real.RunWithEnv(ctx, env, name, args...)
	}
	return r.fake.RunWithEnv(ctx, env, name, args...)
}

func TestLaunchPickedIsolatedTmux_FirstServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	for _, noVerify := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "no-verify"}[noVerify], func(t *testing.T) {
			root, err := os.MkdirTemp("/private/tmp", "f242-pick-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			t.Setenv("TMUX", "")
			t.Setenv("TMUX_TMPDIR", root)

			claudeBin := filepath.Join(root, "claude-helper")
			if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

			fake := prepareRunner()
			router := tmuxRoutingRunner{fake: fake}
			client := pr.New(router,
				pr.WithSessionsDir(filepath.Join(root, "sessions")),
				pr.WithTmuxSession("reviews"),
				pr.WithDispatchWait(func(context.Context) error { return nil }),
			)
			t.Cleanup(func() {
				for _, session := range mustListSessions(t, client) {
					_ = client.Teardown(context.Background(), session.Path)
				}
				_, _ = router.real.Run(context.Background(), "tmux", "kill-server")
			})

			ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
			cmd, _, _ := newTestCmd()
			store := pr.LoadReviewed(filepath.Join(root, "reviewed.json"))
			err = launchPicked(context.Background(), client, config.Config{}, cmd, []pr.PR{{Ref: ref}}, store, noVerify)
			if err != nil {
				t.Fatalf("launchPicked: %v", err)
			}
			if sessions := mustListSessions(t, client); len(sessions) != 1 {
				t.Fatalf("sessions = %+v, want one breadcrumb", sessions)
			}
		})
	}
}

func mustListSessions(t *testing.T, client *pr.Client) []pr.Session {
	t.Helper()
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	return sessions
}
