//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

// TestResumeHooksSIGTERMLeavesUnrecorded sends a real SIGTERM to this process
// while the watcher's restart waits on a busy session — what launchd does on
// bootout. The run's own signal context must end the waiting, start no
// further hook, and leave the version unrecorded so the next run fires again.
func TestResumeHooksSIGTERMLeavesUnrecorded(t *testing.T) {
	env := &cliRestartEnv{status: "busy"}
	restartFixture(t, env)
	f := newHooksFixture(t)
	cfg := "[[resume.on_update]]\nharness = \"claude\"\naction = \"restart\"\n" + commandHookTOML
	f.version = "2.1.99"
	if _, err := f.run(t, cfg, "run"); err != nil {
		t.Fatal(err)
	}
	f.version = "2.1.100"
	hooksHandleSignals = true
	prev := restartOverride
	restartOverride = func(r *resume.RestartRequest) {
		prev(r)
		inner, sent := r.Progress, false
		r.Progress = func(ev resume.RestartEvent) {
			inner(ev)
			if !sent && ev.State == resume.StateWaiting {
				sent = true
				// The run's signal.NotifyContext is registered by now, so
				// this cancels the run rather than killing the test binary.
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					t.Errorf("send SIGTERM: %v", err)
				}
			}
		}
	}
	t.Cleanup(func() { restartOverride = prev })
	// hookNotify is a no-op off macOS, so count its calls here: on macOS each
	// one is an osascript run, which the assertion below would catch.
	prevNotify, notices := hookNotify, 0
	hookNotify = func(context.Context, module.Deps, string, string) error { notices++; return nil }
	t.Cleanup(func() { hookNotify = prevNotify })

	out, err := f.run(t, cfg, "run")
	if !errors.Is(err, resume.ErrHooksInterrupted) || ExitCode(err) != 1 {
		t.Fatalf("err = %v (exit %d), want an interrupted run\n%s", err, ExitCode(err), out)
	}
	if env.terminated != 0 {
		t.Fatal("a busy session was stopped")
	}
	if len(f.commandCalls()) != 0 {
		t.Fatal("the command hook ran after the SIGTERM")
	}
	if notices != 0 {
		t.Fatalf("%d notice(s) posted after the SIGTERM", notices)
	}
	st, _, lerr := resume.FileHookStore{Dir: f.state}.Load("claude")
	if lerr != nil || st.Version != "2.1.99" {
		t.Fatalf("state %+v, %v; want 2.1.99 left recorded", st, lerr)
	}
}
