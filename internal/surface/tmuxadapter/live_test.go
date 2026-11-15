// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package tmuxadapter

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// liveTmuxEnv gates the live test: it starts a real tmux server, so it never
// runs by default.
const liveTmuxEnv = "FORGECTL_LIVE_TMUX"

// TestLiveTmux drives the adapter against a real tmux, with
// FORGECTL_LIVE_TMUX=1. It starts a tmux server of its own on a private
// socket in a fresh 0700 directory, so it never touches the operator's tmux
// server or any session on it. Two phases, each with one session: a short
// command that has ended before Close, and a long one still running at Close.
// Each runs Start, Probe, Close, then Probe and a second Close to see it gone.
// It always kills its own server afterwards. The tmux version the run saw is
// logged.
func TestLiveTmux(t *testing.T) {
	if os.Getenv(liveTmuxEnv) != "1" {
		t.Skipf("set %s=1 to run against a real tmux", liveTmuxEnv)
	}
	tmuxPath, err := osexec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux on PATH")
	}
	tmuxPath, err = filepath.Abs(tmuxPath)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := osexec.Command(tmuxPath, "-V").Output(); err != nil { //nolint:gosec,noctx // G204: the operator's tmux binary, fixed argument
		t.Logf("tmux -V failed: %v", err)
	} else {
		t.Logf("tmux version: %s", string(v))
	}

	// A short path (the socket path limit is small on macOS) in a private
	// directory, named for this test only. MkdirTemp creates it 0700.
	dir, err := os.MkdirTemp("/tmp", "fxt")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	// Only this socket's server is ever killed, found by the path this test
	// made; nothing is removed by a guess.
	t.Cleanup(func() {
		out, err := osexec.Command(tmuxPath, "-S", socket, "kill-server").CombinedOutput() //nolint:gosec,noctx // G204: the test's own private server
		t.Logf("cleanup: tmux kill-server on the private socket: err=%v out=%s", err, out)
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})

	// The private server still reads ~/.tmux.conf, and a config with
	// remain-on-exit or a plugin would change what is measured (or write the
	// test's sessions into the operator's saved state). The runner copies the
	// environment when it is built, so point HOME and XDG_CONFIG_HOME at an
	// empty directory first. /etc/tmux.conf, the system file, still applies.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	getenv := func(k string) string {
		if k == "TMUX" {
			return socket + ",1,0"
		}
		return os.Getenv(k)
	}
	a, err := New(exec.NewOSSensitiveRunner(), tmuxPath, getenv, os.Getuid)
	if err != nil {
		t.Fatal(err)
	}
	for _, ph := range []struct {
		name    string
		program string
		finish  bool
	}{
		{"finished before close", "'/bin/sh' '-c' 'sleep 4; exit 7'", true},
		{"running at close", "'/bin/sh' '-c' 'sleep 120'", false},
	} {
		t.Run(ph.name, func(t *testing.T) {
			ctx := t.Context()
			tag, err := backend.NewRecoveryTag()
			if err != nil {
				t.Fatal(err)
			}
			cwd, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			boot, err := backend.NewBootstrapCommand(exec.Opaque(ph.program))
			if err != nil {
				t.Fatal(err)
			}
			spec, err := backend.NewStartSpec(cwd, "forgectl-live-tmux", tag, boot)
			if err != nil {
				t.Fatal(err)
			}
			res := a.Start(ctx, spec)
			ref, has := res.Ref()
			if has {
				// A result can carry a ref and a cause; close it through the
				// adapter whatever happens next. Close checks identity first.
				t.Cleanup(func() {
					_ = a.Close(context.Background(), ref)
				})
			}
			if !has || res.Failed() {
				c, _ := res.Cause()
				t.Fatalf("Start = %v (%v)", res, c)
			}
			id, err := ref.TMuxIdentity()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("started session %s", id.Session())
			if p := a.Probe(ctx, ref); p.State() != backend.ProbePresent {
				t.Fatalf("Probe while running = %v", p)
			}
			if ph.finish {
				// tmux removes a session when its only program ends, so Probe
				// must say gone; anything else (unreadable, or still present
				// long after a 4s program) fails the phase.
				deadline := time.Now().Add(20 * time.Second)
				for {
					p := a.Probe(ctx, ref)
					if p.State() == backend.ProbeGone {
						t.Logf("Probe after the program ended = %v", p)
						break
					}
					if p.State() != backend.ProbePresent {
						t.Fatalf("Probe after the program ended = %v, want gone", p)
					}
					if time.Now().After(deadline) {
						t.Fatalf("session still present 20s after a 4s program")
					}
					time.Sleep(250 * time.Millisecond)
				}
			}
			c := a.Close(ctx, ref)
			// A session still running must report closed; one whose program
			// has ended is already gone by now.
			want := backend.CloseClosed
			if ph.finish {
				want = backend.CloseAlreadyGone
			}
			if c.State() != want {
				cause, _ := c.Cause()
				t.Fatalf("Close = %v (%v), want %v", c, cause, want)
			}
			t.Logf("Close settled as %v", c)
			if p := a.Probe(ctx, ref); p.State() != backend.ProbeGone {
				t.Errorf("Probe after Close = %v, want gone", p)
			}
			if c := a.Close(ctx, ref); c.State() != backend.CloseAlreadyGone {
				t.Errorf("second Close = %v, want already-gone", c)
			}
		})
	}
}
