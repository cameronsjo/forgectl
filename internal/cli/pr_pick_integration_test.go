//go:build unix

package cli

// Actual-tmux integration matrix for launchPicked's first-ever bulk launch.
//
// Everything else in this package fakes tmux. These rows do not: they run the
// real binary against an isolated default socket under a fresh TMUX_TMPDIR, so
// the typed absent-default fact, the strict version floor, the display probe,
// ensureSession's first server, and `new-window -P -F` are all exercised as
// they ship. What they must prove is an ORDER, not just an outcome:
//
//   1. admission's real list-windows sees the absent default and returns
//      live=0, free>0 without starting a server;
//   2. capability's real `tmux -V` and display probe see the same absent fact;
//   3. only AFTER that does PrepareMany create a clean room or breadcrumb;
//   4. Launch rechecks capability, ensureSession creates the first server, and
//      new-window returns a dispatch;
//   5. the injected verification lists that exact live dispatch and the command
//      succeeds.
//
// The instrumented runner below records a filesystem checkpoint beside every
// command, which is what turns "it returned nil" into evidence about step 3.
// Repeating the positive path under --no-verify pins that only the sweep
// differs. The stale-socket and custom-$TMUX rows are the negative controls:
// both must stop at admission and leave nothing behind.

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	internalexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// checkpoint is one command plus what existed on disk when it was issued.
type checkpoint struct {
	name string
	verb string
	// review artifacts visible at this instant
	breadcrumbs   int
	newWorkspaces int
	socketExists  bool
}

// tmuxRoutingRunner sends tmux to the real binary and everything else to the
// deterministic PR fakes, recording a checkpoint beside every call.
type tmuxRoutingRunner struct {
	real internalexec.OSRunner
	fake *internalexec.FakeRunner

	sessionsDir string
	socketPath  string
	tempRoot    string          // this bench's private $TMPDIR
	baseline    map[string]bool // workspaces that existed before the test

	// PrepareMany fans out, so record runs on several goroutines at once.
	mu          sync.Mutex
	checkpoints []checkpoint
}

func newRoutingRunner(t *testing.T, fake *internalexec.FakeRunner, sessionsDir, socketPath, tempRoot string) *tmuxRoutingRunner {
	t.Helper()
	return &tmuxRoutingRunner{
		fake:        fake,
		sessionsDir: sessionsDir,
		socketPath:  socketPath,
		tempRoot:    tempRoot,
		baseline:    workspaceSet(tempRoot),
	}
}

// workspaceSet is every sandbox workspace currently under tempRoot, which the
// bench installs as $TMPDIR so sandbox.Sandbox's os.MkdirTemp("", …) lands
// there. Scoping the glob to this test's own root is what keeps a sibling
// package's workspace, created in the shared OS temp root while this test runs,
// from being charged to it.
func workspaceSet(tempRoot string) map[string]bool {
	matches, _ := filepath.Glob(filepath.Join(tempRoot, sandbox.WorkspacePrefix+"*"))
	set := make(map[string]bool, len(matches))
	for _, m := range matches {
		set[m] = true
	}
	return set
}

func (r *tmuxRoutingRunner) record(name string, args []string) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	entries, _ := os.ReadDir(r.sessionsDir)
	newWorkspaces := 0
	for path := range workspaceSet(r.tempRoot) {
		if !r.baseline[path] {
			newWorkspaces++
		}
	}
	_, statErr := os.Lstat(r.socketPath)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkpoints = append(r.checkpoints, checkpoint{
		name: name, verb: verb,
		breadcrumbs:   len(entries),
		newWorkspaces: newWorkspaces,
		socketExists:  statErr == nil,
	})
}

func (r *tmuxRoutingRunner) isTmux(name string) bool { return filepath.Base(name) == "tmux" }

func (r *tmuxRoutingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.record(name, args)
	if r.isTmux(name) {
		return r.real.Run(ctx, name, args...)
	}
	return r.fake.Run(ctx, name, args...)
}

func (r *tmuxRoutingRunner) RunInteractive(ctx context.Context, name string, args ...string) error {
	r.record(name, args)
	if r.isTmux(name) {
		return r.real.RunInteractive(ctx, name, args...)
	}
	return r.fake.RunInteractive(ctx, name, args...)
}

func (r *tmuxRoutingRunner) RunWithInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	r.record(name, args)
	if r.isTmux(name) {
		return r.real.RunWithInput(ctx, stdin, name, args...)
	}
	return r.fake.RunWithInput(ctx, stdin, name, args...)
}

func (r *tmuxRoutingRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	r.record(name, args)
	if r.isTmux(name) {
		return r.real.RunWithEnv(ctx, env, name, args...)
	}
	return r.fake.RunWithEnv(ctx, env, name, args...)
}

func (r *tmuxRoutingRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	r.record(name, args)
	if r.isTmux(name) {
		return r.real.RunWithEnvFiltered(ctx, env, unset, name, args...)
	}
	return r.fake.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// tmuxVerbs is the ordered list of tmux verbs the run issued.
func (r *tmuxRoutingRunner) tmuxVerbs() []string {
	var verbs []string
	for _, c := range r.checkpoints {
		if r.isTmux(c.name) {
			verbs = append(verbs, c.verb)
		}
	}
	return verbs
}

// isolatedPickBench sets up a fresh TMUX_TMPDIR, an empty $TMUX, a long-lived
// fake claude, and a pr.Client with a zero-cost dispatch waiter — the shared
// preamble of every row below. It returns the routing runner and the client.
func isolatedPickBench(t *testing.T) (*tmuxRoutingRunner, *pr.Client, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Not t.TempDir(): macOS caps a Unix socket path at ~104 bytes and
	// t.TempDir() embeds the full test name, which overflows it. "/tmp" is
	// absolute and filepath.Clean-stable on macOS and Linux alike, which is what
	// classifyServerFailure requires of TMUX_TMPDIR.
	root, err := os.MkdirTemp("/tmp", "f242-pick-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", root)
	// os.TempDir() reads $TMPDIR on every call, and sandbox.Sandbox creates
	// workspaces with os.MkdirTemp("", …). Pointing $TMPDIR at this bench's own
	// root is what makes the workspace checkpoints below observe only THIS
	// test's workspaces: sibling packages run in separate processes and keep
	// writing forgectl-workflow-* dirs into the shared OS temp root throughout.
	t.Setenv("TMPDIR", root)

	// The review agent must outlive the dispatch long enough to be listed.
	claudeBin := filepath.Join(root, "claude-helper")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(os.Geteuid()), "default")

	router := newRoutingRunner(t, prepareRunner(), sessionsDir, socketPath, root)
	client := pr.New(router,
		pr.WithSessionsDir(sessionsDir),
		pr.WithTmuxSession("reviews"),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	t.Cleanup(func() {
		for _, session := range mustListSessions(t, client) {
			_ = client.Teardown(context.Background(), session.Path())
		}
		_, _ = router.real.Run(context.Background(), "tmux", "kill-server")
	})
	return router, client, socketPath
}

func pickOne(t *testing.T, client *pr.Client, router *tmuxRoutingRunner, noVerify bool) error {
	t.Helper()
	cmd, _, _ := newTestCmd()
	store := pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json"))
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	return launchPicked(context.Background(), client, config.Config{}, cmd, []pr.PR{{Ref: ref}}, store, noVerify)
}

func TestLaunchPickedIsolatedTmux_FirstServer(t *testing.T) {
	for _, noVerify := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "no-verify"}[noVerify], func(t *testing.T) {
			router, client, socketPath := isolatedPickBench(t)

			if err := pickOne(t, client, router, noVerify); err != nil {
				t.Fatalf("launchPicked: %v", err)
			}

			// Step 1-2 then 3: the ordered tmux conversation. The floor and the
			// capability probe both precede any review mutation, and the
			// defensive recheck inside Launch precedes ensureSession.
			//
			// list-sessions replaced has-session with #237: presence is decided
			// by exact Go equality over a listing, because has-session's own -t
			// operand went through tmux's prefix-matching target grammar. This
			// runs against a REAL tmux on an isolated socket, so it is also the
			// live proof that `new-session -P -F` returns an identity the
			// package can parse.
			verbs := router.tmuxVerbs()
			wantPrefix := []string{"list-windows", "-V", "display-message", "-V", "display-message", "list-sessions", "new-session", "list-sessions", "new-window"}
			if len(verbs) < len(wantPrefix) {
				t.Fatalf("tmux verbs = %v, want at least %v", verbs, wantPrefix)
			}
			for i, want := range wantPrefix {
				if verbs[i] != want {
					t.Fatalf("tmux verb %d = %q, want %q (full: %v)", i, verbs[i], want, verbs)
				}
			}

			// Step 5, and the whole point of --no-verify: the delayed sweep is
			// the ONLY difference between the two rows.
			sweeps := 0
			for _, v := range verbs[len(wantPrefix):] {
				if v == "list-windows" {
					sweeps++
				}
			}
			wantSweeps := 1
			if noVerify {
				wantSweeps = 0
			}
			if sweeps != wantSweeps {
				t.Errorf("post-dispatch list-windows sweeps = %d, want %d (verbs: %v)", sweeps, wantSweeps, verbs)
			}

			// Step 3, the instrumented claim: at every checkpoint up to and
			// including capability success, there was no review workspace, no
			// breadcrumb, and no tmux server. The socket PARENT directory tmux
			// creates on a read-only connection is allowed; the socket is not.
			capabilityDone := 0
			for i, c := range router.checkpoints {
				if router.isTmux(c.name) && c.verb == "display-message" {
					capabilityDone = i
					break
				}
			}
			for i, c := range router.checkpoints[:capabilityDone+1] {
				if c.breadcrumbs != 0 || c.newWorkspaces != 0 {
					t.Errorf("checkpoint %d (%s %s): %d breadcrumbs, %d workspaces before capability success",
						i, c.name, c.verb, c.breadcrumbs, c.newWorkspaces)
				}
				if c.socketExists {
					t.Errorf("checkpoint %d (%s %s): a tmux server socket existed before capability success", i, c.name, c.verb)
				}
			}

			// Step 4: the first server really was created, and the review landed.
			if _, err := os.Lstat(socketPath); err != nil {
				t.Errorf("no tmux socket at %q after dispatch: %v", socketPath, err)
			}
			if sessions := mustListSessions(t, client); len(sessions) != 1 {
				t.Fatalf("sessions = %+v, want one breadcrumb", sessions)
			}
		})
	}
}

// TestLaunchPickedIsolatedTmux_RefusesAtAdmission is the negative-control pair.
// Neither a stale default socket nor an explicit $TMUX may be read as a clean
// absent default: both must fail admission's real list-windows, keep the
// existing unreadable-count message, and leave nothing behind.
func TestLaunchPickedIsolatedTmux_RefusesAtAdmission(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, socketPath string)
	}{
		{
			name: "stale default socket",
			setup: func(t *testing.T, socketPath string) {
				if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
					t.Fatal(err)
				}
				// A real bound-but-unserved socket: present on disk, nothing
				// listening. ensureSession must never be allowed to replace it.
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
				if err != nil {
					t.Fatalf("bind stale socket: %v", err)
				}
				listener.SetUnlinkOnClose(false)
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "custom $TMUX",
			setup: func(t *testing.T, socketPath string) {
				t.Setenv("TMUX", filepath.Join(filepath.Dir(socketPath), "custom")+",1,0")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, client, socketPath := isolatedPickBench(t)
			tt.setup(t, socketPath)

			err := pickOne(t, client, router, false)
			if err == nil || !strings.Contains(err.Error(), "cannot read the tmux review window count") {
				t.Fatalf("error = %v, want the fail-closed unreadable-count refusal", err)
			}

			// Refusal happens at admission: the floor was never even probed.
			verbs := router.tmuxVerbs()
			if len(verbs) != 1 || verbs[0] != "list-windows" {
				t.Errorf("tmux verbs = %v, want exactly one list-windows", verbs)
			}
			for i, c := range router.checkpoints {
				if c.breadcrumbs != 0 || c.newWorkspaces != 0 {
					t.Errorf("checkpoint %d (%s %s): %d breadcrumbs, %d workspaces after a refusal",
						i, c.name, c.verb, c.breadcrumbs, c.newWorkspaces)
				}
			}
			if sessions := mustListSessions(t, client); len(sessions) != 0 {
				t.Errorf("sessions = %+v, want none after a refusal", sessions)
			}
		})
	}
}

func mustListSessions(t *testing.T, client *pr.Client) []pr.SessionSummary {
	t.Helper()
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	return sessions
}
